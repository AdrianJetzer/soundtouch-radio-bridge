package bridge

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/events"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/speaker"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/upnp"
)

// box supervises one managed speaker (the master of a stereo pair).
type box struct {
	b  *Bridge
	id string

	mu       sync.Mutex
	host     string
	name     string
	slaveOf  string // set when the speaker turns out to be the non-master of a pair
	group    string // last logged group status
	failures int    // consecutive failed connects
	player   *upnp.Player

	connected  bool
	lastPublic string // public URL used by the last successful sync (Z9)
	lastPress  pressState

	presses  chan int      // latest press wins (buffer 1)
	sync     chan struct{} // sync request (buffer 1)
	debounce events.Debouncer
}

func newBox(b *Bridge, id, host, name string) *box {
	if name == "" {
		name = id
	}
	return &box{
		b: b, id: id, host: host, name: name,
		presses:  make(chan int, 1),
		sync:     make(chan struct{}, 1),
		debounce: events.Debouncer{Window: 1500 * time.Millisecond},
	}
}

func (x *box) log() *slog.Logger {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.b.Log.With("box", x.name)
}

func (x *box) getHost() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.host
}

func (x *box) setHost(h string) {
	if h == "" {
		return
	}
	x.mu.Lock()
	old := x.host
	if old != h {
		x.host = h
		x.player = nil
	}
	x.mu.Unlock()
	if old != "" && old != h {
		x.log().Info("neue Adresse", "alt", old, "neu", h)
	}
}

func (x *box) setName(n string) {
	x.mu.Lock()
	x.name = n
	x.mu.Unlock()
}

func (x *box) upnpPlayer() *upnp.Player {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.player == nil {
		x.player = upnp.New(x.host, x.id)
		x.player.Base = x.b.url("http", x.host, func(p Ports) int { return p.UPnP })
	}
	return x.player
}

func (x *box) requestSync() {
	select {
	case x.sync <- struct{}{}:
	default:
	}
}

// run restarts the supervisor loop after a panic, so one bad message or
// one bug never takes down the speaker for good (Z7).
func (x *box) run(ctx context.Context) {
	for ctx.Err() == nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					x.log().Error("interner Fehler, Box wird neu gestartet", "fehler", r, "stack", string(debug.Stack()))
				}
			}()
			x.loop(ctx)
		}()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

func (x *box) loop(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	l := &events.Listener{
		URL: func() string {
			return x.b.url("ws", x.getHost(), func(p Ports) int { return p.WS })
		},
		OnMessage: x.onMessage,
		OnState:   x.onState,
	}
	if x.b.TuneListener != nil {
		x.b.TuneListener(l)
	}
	go l.Run(ctx)
	go x.playWorker(ctx)

	t := time.NewTicker(x.b.SyncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-x.sync:
		}
		x.syncPresets(ctx)
	}
}

func (x *box) onState(up bool, err error) {
	x.mu.Lock()
	wasFailing := x.failures
	x.connected = up
	if up {
		x.failures = 0
	} else {
		x.failures++
	}
	n := x.failures
	host := x.host
	x.mu.Unlock()

	switch {
	case up:
		x.refreshName()
		if wasFailing > 0 {
			x.log().Info("wieder verbunden, Tasten sind aktiv", "adresse", host)
		} else {
			x.log().Info("verbunden, Tasten sind aktiv", "adresse", host)
		}
		x.checkGroup()
		x.requestSync()
	case n == 1:
		x.log().Warn("Verbindung zur Box verloren, versuche es weiter", "adresse", host, "grund", err)
	case n == 3 || host == "":
		x.log().Info("Box nicht erreichbar, suche im Netz", "adresse", host)
		x.b.requestSearch()
	case n%20 == 0:
		x.log().Warn("Box weiterhin nicht erreichbar (ausgeschaltet oder nicht im WLAN?)", "versuche", n)
		x.b.requestSearch()
	}
}

func (x *box) onMessage(raw []byte) {
	e := events.Parse(raw)
	x.b.Log.Debug("ws", "box", x.id, "msg", string(raw))
	switch e.Kind {
	case events.KindPreset:
		if !x.debounce.Allow(e.Preset, time.Now()) {
			x.log().Debug("doppelter Tastendruck ignoriert", "taste", e.Preset)
			return
		}
		select { // replace a press that has not started yet
		case <-x.presses:
		default:
		}
		x.presses <- e.Preset
	case events.KindPresetsUpdated:
		x.requestSync()
	case events.KindGroup:
		x.onGroup(*e.Group)
	case events.KindError:
		x.log().Debug("Box meldet Fehler", "fehler", e.Error)
	}
}

// playWorker starts stations one at a time; a new press cancels a start
// that is still in progress (Z6).
func (x *box) playWorker(ctx context.Context) {
	var stop context.CancelFunc
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case slot := <-x.presses:
			if stop != nil {
				stop()
			}
			pctx, c := context.WithTimeout(ctx, 20*time.Second)
			stop = c
			go func() {
				defer c()
				x.play(pctx, slot)
			}()
		}
	}
}

func (x *box) play(ctx context.Context, slot int) {
	log := x.log()
	x.mu.Lock()
	slave := x.slaveOf
	x.mu.Unlock()
	if slave != "" {
		log.Warn("Tastendruck an der Nicht-Master-Box eines Paars ignoriert", "taste", slot, "master", slave)
		return
	}
	st := x.b.Config().PresetsFor(x.id)[slot-1]
	if st == nil {
		log.Info(fmt.Sprintf("Taste %d ist nicht belegt", slot))
		return
	}
	host := x.getHost()
	public, err := x.b.publicURL(host)
	if err != nil {
		log.Error(fmt.Sprintf("Taste %d → %s: eigene Adresse unbekannt", slot, st.Name), "fehler", err)
		return
	}
	m := upnp.Media{URI: StreamURL(public, x.id, slot), Title: st.Name, Art: st.Logo, MIME: guessMIME(st.URL)}
	start := time.Now()
	err = x.upnpPlayer().Play(ctx, m)
	if err != nil && ctx.Err() == nil {
		log.Debug("erster Startversuch fehlgeschlagen, wiederhole", "fehler", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			err = x.upnpPlayer().Play(ctx, m)
		}
	}
	ms := time.Since(start).Milliseconds()
	x.mu.Lock()
	x.lastPress = pressState{Slot: slot, Station: st.Name, At: start, OK: err == nil}
	if err != nil {
		x.lastPress.Error = err.Error()
	}
	x.mu.Unlock()
	switch {
	case ctx.Err() != nil && err != nil:
		log.Debug(fmt.Sprintf("Taste %d → %s abgebrochen (neuer Tastendruck)", slot, st.Name))
	case err != nil:
		log.Error(fmt.Sprintf("Taste %d → %s ✗", slot, st.Name), "fehler", err)
	default:
		log.Info(fmt.Sprintf("Taste %d → %s ▶ ok (%d ms)", slot, st.Name, ms))
	}
}

func guessMIME(u string) string {
	l := strings.ToLower(u)
	if strings.HasSuffix(l, ".aac") || strings.Contains(l, "/aac") || strings.Contains(l, "aac-") {
		return "audio/aac"
	}
	return "audio/mpeg"
}

// refreshName reads the speaker's name so logs show "SoundTouch Links"
// instead of the deviceID from the configuration.
func (x *box) refreshName() {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if info, err := x.b.api(x.getHost()).Info(ctx); err == nil && info.Name != "" {
		x.setName(info.Name)
	}
}

// checkGroup reads the stereo pair status once after connecting.
func (x *box) checkGroup() {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	g, err := x.b.api(x.getHost()).Group(ctx)
	if err != nil {
		x.log().Debug("Paar-Status nicht lesbar", "fehler", err)
		return
	}
	x.onGroup(g)
}

func (x *box) onGroup(g speaker.Group) {
	x.mu.Lock()
	prevSlave := x.slaveOf
	if g.Paired() && g.MasterID != x.id {
		x.slaveOf = g.MasterID
	} else {
		x.slaveOf = ""
	}
	slave := x.slaveOf
	prev := x.group
	x.group = g.Status
	x.mu.Unlock()

	log := x.log()
	if slave != "" && prevSlave != slave {
		log.Warn("Box ist die zweite Box eines Stereo-Paars; Tasten meldet nur der Master. Bitte stattdessen die Master-Box unter speakers: eintragen", "master", g.MasterID)
	}
	if !g.Paired() || prev == g.Status {
		return
	}
	partner := g.Partner(x.id)
	switch g.Status {
	case "GROUP_OK":
		if prev != "" {
			log.Info("Stereo-Paar wieder vollständig, Tasten funktionieren", "partner", partner)
		} else {
			log.Info("Teil eines Stereo-Paars", "partner", partner, "master", g.MasterID == x.id)
		}
	default:
		log.Warn("Stereo-Paar unvollständig: die zweite Box ist nicht erreichbar. Bis sie zurück ist, reagieren die Tasten nicht", "partner", partner, "status", g.Status)
	}
}

// syncPresets makes the stored presets point at strb (5.5). It backs up the
// speaker's presets before overwriting anything that strb did not write.
func (x *box) syncPresets(ctx context.Context) {
	x.mu.Lock()
	slave := x.slaveOf
	x.mu.Unlock()
	if slave != "" {
		return
	}
	log := x.log()
	host := x.getHost()
	if host == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	public, err := x.b.publicURL(host)
	if err != nil {
		log.Warn("Presets nicht abgeglichen: eigene Adresse unbekannt", "fehler", err)
		return
	}
	sp := x.b.api(host)
	current, raw, err := sp.Presets(ctx)
	if err != nil {
		log.Debug("Presets nicht lesbar", "fehler", err)
		return
	}
	have := map[int]speaker.ContentItem{}
	for _, p := range current {
		have[p.ID] = p.Item
	}

	want := x.b.Config().PresetsFor(x.id)
	var todo []int
	for i, st := range want {
		if st == nil {
			continue
		}
		w := WantedPreset(public, x.id, i+1, st)
		if h, ok := have[i+1]; !ok || !samePreset(h, w) {
			todo = append(todo, i+1)
		}
	}
	if len(todo) == 0 {
		x.setLastPublic(public)
		return
	}

	if hasForeign(current) {
		if path, err := x.backup(raw); err != nil {
			log.Error("Sicherung der Presets fehlgeschlagen; es wird nichts überschrieben", "fehler", err)
			return
		} else if path != "" {
			log.Info("bisherige Presets gesichert", "datei", path)
		}
	}

	var ok, failed []string
	for _, slot := range todo {
		w := WantedPreset(public, x.id, slot, want[slot-1])
		if err := sp.StorePreset(ctx, slot, w); err != nil {
			failed = append(failed, fmt.Sprintf("%d (%v)", slot, err))
			continue
		}
		ok = append(ok, fmt.Sprintf("%d=%s", slot, w.ItemName))
	}
	if len(ok) > 0 {
		log.Info("Presets auf der Box gespeichert: " + strings.Join(ok, ", "))
	}
	if len(failed) > 0 {
		log.Warn("Presets nicht gespeichert, neuer Versuch beim nächsten Abgleich: " + strings.Join(failed, ", "))
		return
	}
	x.setLastPublic(public)
}

func (x *box) setLastPublic(p string) {
	x.mu.Lock()
	x.lastPublic = p
	x.mu.Unlock()
}

// checkPublicURL requests a sync when our own address changed (Z9), e.g.
// because the computer got a new IP from the router.
func (x *box) checkPublicURL() {
	host := x.getHost()
	if host == "" {
		return
	}
	public, err := x.b.publicURL(host)
	x.mu.Lock()
	changed := err == nil && x.lastPublic != "" && x.lastPublic != public
	old := x.lastPublic
	x.mu.Unlock()
	if changed {
		x.log().Info("eigene Adresse hat sich geändert, Presets werden neu geschrieben", "alt", old, "neu", public)
		x.requestSync()
	}
}

// pressState is the last button press, for /status.
type pressState struct {
	Slot    int       `json:"taste"`
	Station string    `json:"sender"`
	At      time.Time `json:"zeit"`
	OK      bool      `json:"ok"`
	Error   string    `json:"fehler,omitempty"`
}

// Status is the state of one speaker, for /status.
type Status struct {
	DeviceID  string      `json:"deviceID"`
	Name      string      `json:"name"`
	Host      string      `json:"adresse"`
	Connected bool        `json:"verbunden"`
	Group     string      `json:"stereoPaar,omitempty"`
	SecondBox bool        `json:"zweiteBoxImPaar,omitempty"`
	PublicURL string      `json:"eigeneAdresse,omitempty"`
	LastPress *pressState `json:"letzterTastendruck,omitempty"`
	Failures  int         `json:"fehlversuche"`
}

func (x *box) status() Status {
	x.mu.Lock()
	defer x.mu.Unlock()
	s := Status{DeviceID: x.id, Name: x.name, Host: x.host, Connected: x.connected, Group: x.group,
		SecondBox: x.slaveOf != "", PublicURL: x.lastPublic, Failures: x.failures}
	if x.lastPress.Slot != 0 {
		lp := x.lastPress
		s.LastPress = &lp
	}
	return s
}

func samePreset(a, b speaker.ContentItem) bool {
	return a.Source == b.Source && a.Location == b.Location && a.ItemName == b.ItemName && a.ContainerArt == b.ContainerArt
}

// hasForeign reports whether any preset was not written by strb.
func hasForeign(ps []speaker.Preset) bool {
	for _, p := range ps {
		if !(p.Item.Source == "UPNP" && strings.Contains(p.Item.Location, "/stream/")) {
			return true
		}
	}
	return false
}

// backup writes raw to backups/<deviceID>-<time>.xml unless the newest
// backup of this speaker already has the same content.
func (x *box) backup(raw []byte) (string, error) {
	dir := x.b.backupDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	matches, _ := filepath.Glob(filepath.Join(dir, x.id+"-*.xml"))
	sort.Strings(matches)
	if len(matches) > 0 {
		if prev, err := os.ReadFile(matches[len(matches)-1]); err == nil && sha256.Sum256(prev) == sha256.Sum256(raw) {
			return "", nil
		}
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.xml", x.id, time.Now().Format("20060102-150405")))
	return path, os.WriteFile(path, raw, 0o644)
}

// Presets of a speaker, exported for the CLI.
func Presets(ctx context.Context, host string) ([]speaker.Preset, error) {
	p, _, err := speaker.New(host).Presets(ctx)
	return p, err
}
