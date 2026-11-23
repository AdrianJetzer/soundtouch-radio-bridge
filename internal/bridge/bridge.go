// Package bridge ties everything together: it discovers speakers, keeps one
// supervisor per managed speaker, serves the stream proxy and applies
// configuration changes at runtime.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/config"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/discovery"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/proxy"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/speaker"
)

// Bridge is the running strb service.
type Bridge struct {
	Log     *slog.Logger
	Version string

	// Timings; zero means the default from the spec.
	DiscoverEvery time.Duration // 5 min
	SyncEvery     time.Duration // 30 min
	WatchEvery    time.Duration // 2 s

	// Search finds speakers; defaults to SSDP discovery. Replaceable in tests.
	Search func(ctx context.Context) []discovery.Found

	cfg   atomic.Pointer[config.Config]
	proxy *proxy.Proxy

	mu         sync.Mutex
	boxes      map[string]*box // managed speakers by deviceID
	others     map[string]string
	lastSearch time.Time
	searchNow  chan struct{}
}

// New prepares a bridge for the given configuration.
func New(cfg *config.Config, log *slog.Logger, version string) *Bridge {
	b := &Bridge{Log: log, Version: version, boxes: map[string]*box{}, others: map[string]string{}, searchNow: make(chan struct{}, 1)}
	b.cfg.Store(cfg)
	b.proxy = proxy.New(lookup{b}, log, "strb/"+version)
	return b
}

// Config returns the active configuration.
func (b *Bridge) Config() *config.Config { return b.cfg.Load() }

// Handler is the HTTP handler the speakers fetch streams from.
func (b *Bridge) Handler() http.Handler { return b.proxy }

// Run serves until ctx is cancelled. It only fails if the HTTP port cannot be opened.
func (b *Bridge) Run(ctx context.Context) error {
	b.defaults()
	cfg := b.Config()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("Port %s lässt sich nicht öffnen (läuft strb schon?): %w", cfg.Listen, err)
	}
	srv := &http.Server{Handler: b.proxy, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.Log.Error("HTTP-Server beendet", "fehler", err)
		}
	}()

	b.Log.Info(fmt.Sprintf("strb %s läuft", b.Version), "port", cfg.Listen, "konfiguration", cfg.Path)
	b.logStations(cfg)

	// Configured speakers start immediately with their known address, even
	// if they are offline right now (Z1).
	for _, sp := range cfg.Speakers {
		b.ensureBox(ctx, sp.ID, sp.Host, "")
	}

	go b.discoverLoop(ctx)
	if cfg.Path != "" {
		go config.Watch(ctx, cfg.Path, b.WatchEvery, func(c *config.Config, err error) { b.applyConfig(ctx, c, err) })
	}
	<-ctx.Done()
	b.Log.Info("strb wird beendet")
	return nil
}

func (b *Bridge) defaults() {
	if b.DiscoverEvery == 0 {
		b.DiscoverEvery = 5 * time.Minute
	}
	if b.SyncEvery == 0 {
		b.SyncEvery = 30 * time.Minute
	}
	if b.WatchEvery == 0 {
		b.WatchEvery = 2 * time.Second
	}
	if b.Search == nil {
		b.Search = func(ctx context.Context) []discovery.Found {
			f, _ := discovery.Search(ctx, 3*time.Second)
			return f
		}
	}
}

func (b *Bridge) logStations(cfg *config.Config) {
	p := cfg.PresetsFor("")
	var parts []string
	for i, st := range p {
		if st != nil {
			parts = append(parts, fmt.Sprintf("%d=%s", i+1, st.Name))
		}
	}
	b.Log.Info("Tastenbelegung: " + strings.Join(parts, ", "))
}

// discoverLoop searches the network at start, every DiscoverEvery, and
// whenever a speaker asks for it (rate-limited to once per 30 s).
func (b *Bridge) discoverLoop(ctx context.Context) {
	t := time.NewTicker(b.DiscoverEvery)
	defer t.Stop()
	for {
		b.discover(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.searchNow:
		}
	}
}

// requestSearch asks for an early discovery, e.g. when a speaker vanished.
func (b *Bridge) requestSearch() {
	b.mu.Lock()
	recent := time.Since(b.lastSearch) < 30*time.Second
	b.mu.Unlock()
	if recent {
		return
	}
	select {
	case b.searchNow <- struct{}{}:
	default:
	}
}

func (b *Bridge) discover(ctx context.Context) {
	b.mu.Lock()
	b.lastSearch = time.Now()
	b.mu.Unlock()
	found := b.Search(ctx)
	cfg := b.Config()
	for _, f := range found {
		id := f.Info.DeviceID
		if !cfg.Manages(id) {
			gctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			g, err := speaker.New(f.Host).Group(gctx)
			cancel()
			if err == nil && g.Paired() && g.MasterID != id && cfg.Manages(g.MasterID) {
				b.noteOther(id, fmt.Sprintf("%s (%s) spielt im Stereo-Paar mit, gesteuert über die Master-Box", f.Info.Name, f.Host))
			} else {
				b.noteOther(id, fmt.Sprintf("%s (%s, %s) wird nicht gesteuert, weil sie nicht unter speakers: steht", f.Info.Name, f.Info.Type, f.Host))
			}
			continue
		}
		b.ensureBox(ctx, id, f.Host, f.Info.Name)
	}
}

func (b *Bridge) noteOther(id, msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.others[id] == msg {
		return
	}
	b.others[id] = msg
	b.Log.Info(msg)
}

// ensureBox starts a supervisor for id or updates the address of a running one.
func (b *Bridge) ensureBox(ctx context.Context, id, host, name string) {
	b.mu.Lock()
	bx, ok := b.boxes[id]
	if !ok {
		bx = newBox(b, id, host, name)
		b.boxes[id] = bx
	}
	b.mu.Unlock()
	if !ok {
		go bx.run(ctx)
		return
	}
	bx.setHost(host)
	if name != "" {
		bx.setName(name)
	}
}

func (b *Bridge) applyConfig(ctx context.Context, c *config.Config, err error) {
	if err != nil {
		b.Log.Error("Konfiguration fehlerhaft, die bisherige bleibt aktiv", "fehler", strings.ReplaceAll(err.Error(), "\n", "; "))
		return
	}
	old := b.cfg.Swap(c)
	if c.Listen != old.Listen {
		b.Log.Warn("listen wurde geändert; das wirkt erst nach einem Neustart von strb", "aktiv", old.Listen)
	}
	b.Log.Info("Konfiguration neu geladen")
	b.mu.Lock()
	boxes := make([]*box, 0, len(b.boxes))
	for _, bx := range b.boxes {
		boxes = append(boxes, bx)
	}
	b.mu.Unlock()
	for _, bx := range boxes {
		for _, line := range config.Diff(old, c, bx.id) {
			bx.log().Info(line)
		}
		if !c.Manages(bx.id) {
			bx.log().Warn("steht nicht mehr unter speakers:; die Box wird erst nach einem Neustart losgelassen")
		}
		bx.requestSync()
	}
	for _, sp := range c.Speakers {
		b.ensureBox(ctx, sp.ID, sp.Host, "")
	}
	b.requestSearch()
}

// publicURL is the address speakers use to reach us: configured, or the
// local address on the route to the speaker plus our port.
func (b *Bridge) publicURL(speakerHost string) (string, error) {
	cfg := b.Config()
	if cfg.PublicURL != "" {
		return cfg.PublicURL, nil
	}
	ip, err := discovery.LocalIPFor(speakerHost)
	if err != nil {
		return "", err
	}
	_, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || port == "" {
		port = "8686"
	}
	return "http://" + net.JoinHostPort(ip, port), nil
}

// backupDir is next to the configuration file.
func (b *Bridge) backupDir() string {
	if p := b.Config().Path; p != "" {
		return filepath.Join(filepath.Dir(p), "backups")
	}
	return "backups"
}

// lookup adapts the bridge to proxy.Lookup using the current configuration.
type lookup struct{ b *Bridge }

func (l lookup) Button(deviceID string, slot int) (*config.Station, bool) {
	if slot < 1 || slot > config.MaxPresets {
		return nil, false
	}
	st := l.b.Config().PresetsFor(deviceID)[slot-1]
	return st, st != nil
}

func (l lookup) Station(key string) (*config.Station, bool) {
	st, ok := l.b.Config().Stations[key]
	return &st, ok
}

// StreamURL is the address a speaker fetches for one of its buttons.
func StreamURL(public, deviceID string, slot int) string {
	return fmt.Sprintf("%s/stream/%s/%d", public, deviceID, slot)
}

// WantedPreset is the content item strb stores for a button.
func WantedPreset(public, deviceID string, slot int, st *config.Station) speaker.ContentItem {
	return speaker.ContentItem{
		Source:        "UPNP",
		Location:      StreamURL(public, deviceID, slot),
		SourceAccount: "UPnPUserName",
		IsPresetable:  true,
		ItemName:      st.Name,
		ContainerArt:  st.Logo,
	}
}
