package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/discovery"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/upnp"
)

// Speakers returns the managed speakers that can be reached now: configured
// addresses first, then discovery. Non-master members of a pair are skipped.
func (b *Bridge) Speakers(ctx context.Context) []discovery.Found {
	b.defaults()
	cfg := b.Config()
	var hosts []string
	for _, sp := range cfg.Speakers {
		if sp.Host != "" {
			hosts = append(hosts, sp.Host)
		}
	}
	byID := map[string]discovery.Found{}
	for _, f := range discovery.Verify(ctx, hosts) {
		byID[f.Info.DeviceID] = f
	}
	missing := len(cfg.Speakers) == 0
	for _, sp := range cfg.Speakers {
		if _, ok := byID[sp.ID]; !ok {
			missing = true
		}
	}
	if missing {
		for _, f := range b.Search(ctx) {
			if _, ok := byID[f.Info.DeviceID]; !ok {
				byID[f.Info.DeviceID] = f
			}
		}
	}
	var out []discovery.Found
	for id, f := range byID {
		if cfg.Manages(id) {
			out = append(out, f)
		}
	}
	return out
}

// SyncOnce writes the presets of all reachable managed speakers and returns.
func (b *Bridge) SyncOnce(ctx context.Context) error {
	found := b.Speakers(ctx)
	if len(found) == 0 {
		return errors.New("keine passende Box erreichbar")
	}
	for _, f := range found {
		x := newBox(b, f.Info.DeviceID, f.Host, f.Info.Name)
		x.checkGroup()
		x.syncPresets(ctx)
	}
	return nil
}

// PlayOnce serves the proxy and starts a button or station on the speaker
// at host. It keeps serving until ctx is cancelled (Ctrl+C).
func (b *Bridge) PlayOnce(ctx context.Context, host, deviceID string, slot int, stationKey string) error {
	cfg := b.Config()
	title, path := "", ""
	if stationKey != "" {
		st, ok := cfg.Stations[stationKey]
		if !ok {
			return fmt.Errorf("unbekannter Sender %q", stationKey)
		}
		title, path = st.Name, "/station/"+stationKey
	} else {
		st := cfg.PresetsFor(deviceID)[slot-1]
		if st == nil {
			return fmt.Errorf("Taste %d ist nicht belegt", slot)
		}
		title, path = st.Name, fmt.Sprintf("/stream/%s/%d", deviceID, slot)
	}
	public, err := b.publicURL(host)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("Port %s belegt – läuft `strb run` schon? Dann einfach die Taste drücken: %w", cfg.Listen, err)
	}
	srv := &http.Server{Handler: b.Handler(), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go srv.Serve(ln)
	defer srv.Close()

	start := time.Now()
	if err := upnp.New(host, deviceID).Play(ctx, upnp.Media{URI: public + path, Title: title}); err != nil {
		return err
	}
	b.Log.Info(fmt.Sprintf("%s ▶ läuft (%d ms). Beenden mit Ctrl+C", title, time.Since(start).Milliseconds()))
	<-ctx.Done()
	return nil
}
