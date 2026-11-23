package bridge

// Scenario tests: the whole chain against simulated speakers (Spec 9, Z1–Z10).
//
//   fake speaker ──press──▶ bridge ──UPnP──▶ fake speaker ──GET──▶ proxy ──▶ test radio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/config"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/discovery"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/events"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/fakespeaker"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/logx"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/speaker"
)

const (
	idA = "AAAAAAAAAAAA"
	idB = "BBBBBBBBBBBB"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t       *testing.T
	b       *Bridge
	dir     string
	radio   *httptest.Server
	log     *syncBuf
	mu      sync.Mutex
	hosts   map[string]*fakespeaker.Speaker // host name → speaker
	ports   map[string]Ports                // host name → ports at registration
	visible []string                        // host names discovery returns
	cancel  context.CancelFunc
	done    chan error
}

// radioServer streams "<station>:" forever for every /<station> path.
func radioServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		chunk := []byte(strings.TrimPrefix(r.URL.Path, "/") + ":")
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stationsYAML(radio string) string {
	var b strings.Builder
	b.WriteString("stations:\n")
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&b, "  s%d: { name: \"Sender %d\", url: \"%s/s%d\" }\n", i, i, radio, i)
	}
	b.WriteString("  neu: { name: \"Neuer Sender\", url: \"" + radio + "/neu\" }\n")
	return b.String()
}

// newEnv writes a config with the given extra YAML (presets, speakers…),
// registers the fake speakers under host names and starts the bridge.
func newEnv(t *testing.T, extra string, fakes map[string]*fakespeaker.Speaker, start bool) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), radio: radioServer(t), log: &syncBuf{},
		hosts: map[string]*fakespeaker.Speaker{}, ports: map[string]Ports{}}
	for h, f := range fakes {
		e.register(h, f, true)
	}
	e.writeConfig(extra)
	cfg, err := config.Load(filepath.Join(e.dir, "strb.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	e.b = New(cfg, logx.New(io.MultiWriter(e.log, testWriter{t}), -4), "test")
	e.b.DiscoverEvery = 150 * time.Millisecond
	e.b.SyncEvery = time.Second
	e.b.WatchEvery = 40 * time.Millisecond
	e.b.APITimeout = time.Second
	e.b.PortsFor = e.portsFor
	e.b.Search = e.search
	e.b.TuneListener = func(l *events.Listener) {
		l.PingEvery = 100 * time.Millisecond
		l.PongTimeout = 150 * time.Millisecond
		l.MinBackoff = 20 * time.Millisecond
		l.MaxBackoff = 200 * time.Millisecond
	}
	if start {
		e.start()
	}
	return e
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	if testing.Verbose() {
		w.t.Log(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

func (e *env) writeConfig(extra string) {
	data := "listen: \"127.0.0.1:0\"\n" + stationsYAML(e.radio.URL) + extra
	if err := os.WriteFile(filepath.Join(e.dir, "strb.yaml"), []byte(data), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) start() {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.done = make(chan error, 1)
	go func() { e.done <- e.b.Run(ctx) }()
	e.t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(3 * time.Second):
			e.t.Error("bridge did not stop")
		}
	})
	e.eventually("bridge listening", func() bool { return e.b.Addr() != "" })
}

// register maps a host name to the speaker's current ports.
func (e *env) register(host string, f *fakespeaker.Speaker, visible bool) {
	api, ws, up := f.Ports()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hosts[host] = f
	e.ports[host] = Ports{IP: "127.0.0.1", API: api, WS: ws, UPnP: up}
	if visible {
		e.visible = append(e.visible, host)
	}
}

func (e *env) setVisible(hosts ...string) {
	e.mu.Lock()
	e.visible = hosts
	e.mu.Unlock()
}

func (e *env) portsFor(host string) Ports {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.ports[host]; ok {
		return p
	}
	return Ports{IP: "127.0.0.1", API: 1, WS: 1, UPnP: 1} // unknown: nothing listens
}

func (e *env) search(context.Context) []discovery.Found {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []discovery.Found
	for _, h := range e.visible {
		f := e.hosts[h]
		out = append(out, discovery.Found{Host: h, Info: speaker.Info{DeviceID: f.ID, Name: f.Name}})
	}
	return out
}

func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timeout: %s\n--- log ---\n%s", what, e.log.String())
}

func (e *env) waitLog(sub string) {
	e.t.Helper()
	e.eventually("log contains "+sub, func() bool { return strings.Contains(e.log.String(), sub) })
}

// waitPlay returns the next stream the speaker started.
func (e *env) waitPlay(f *fakespeaker.Speaker) fakespeaker.Play {
	e.t.Helper()
	select {
	case p := <-f.Plays:
		return p
	case <-time.After(5 * time.Second):
		e.t.Fatalf("speaker %s started no stream\n--- log ---\n%s", f.Name, e.log.String())
	}
	return fakespeaker.Play{}
}

func (e *env) noPlay(f *fakespeaker.Speaker, d time.Duration) {
	e.t.Helper()
	select {
	case p := <-f.Plays:
		e.t.Fatalf("unexpected stream %+v", p)
	case <-time.After(d):
	}
}

// ready waits until the speaker is connected and its presets point at strb.
func (e *env) ready(f *fakespeaker.Speaker) {
	e.t.Helper()
	e.eventually("presets synced on "+f.Name, func() bool {
		for i := 1; i <= 6; i++ {
			if p := f.Preset(i); p.Source != "UPNP" || !strings.Contains(p.Location, "/stream/"+f.ID+"/") {
				return false
			}
		}
		return true
	})
	e.eventually("connected "+f.Name, func() bool { return f.Connections() > 0 })
}

func expectStation(t *testing.T, p fakespeaker.Play, n int) {
	t.Helper()
	want := fmt.Sprintf("Sender %d", n)
	if p.Err != nil || p.Title != want || p.Bytes == 0 {
		t.Fatalf("play = %+v, want title %q with audio", p, want)
	}
}

func newFake(t *testing.T, id, name string) *fakespeaker.Speaker {
	f, err := fakespeaker.New(id, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Stop)
	return f
}

const oneBox = "presets: [s1, s2, s3, s4, s5, s6]\nspeakers:\n  - id: " + idA + "\n    host: box-a\n"

// --- happy path ------------------------------------------------------------

func TestPressPlaysStation(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)

	// The old TuneIn presets were backed up before being overwritten.
	backups, _ := filepath.Glob(filepath.Join(e.dir, "backups", idA+"-*.xml"))
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	if b, _ := os.ReadFile(backups[0]); !bytes.Contains(b, []byte("Old TuneIn 3")) {
		t.Fatalf("backup content: %s", b)
	}
	if p := f.Preset(3); p.ItemName != "Sender 3" {
		t.Fatalf("preset 3 = %+v", p)
	}

	for _, slot := range []int{3, 1, 6} {
		f.Press(slot)
		expectStation(t, e.waitPlay(f), slot)
	}

	// /status reports the speaker and the last press.
	resp, err := http.Get("http://" + e.b.Addr() + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Boxen []Status `json:"boxen"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if len(st.Boxen) != 1 || !st.Boxen[0].Connected || st.Boxen[0].Name != "Links" || st.Boxen[0].LastPress == nil || st.Boxen[0].LastPress.Slot != 6 {
		t.Fatalf("status = %+v", st.Boxen)
	}
}

func TestPowerButtonIsIgnored(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	f.PressPower()
	e.noPlay(f, 300*time.Millisecond)
}

func TestUnassignedButton(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, "presets: [s1, \"\", s3]\nspeakers:\n  - id: "+idA+"\n    host: box-a\n", map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.eventually("preset 1 synced", func() bool { return f.Preset(1).Source == "UPNP" })
	if f.Preset(2).Source != "TUNEIN" {
		t.Fatal("an empty slot must stay untouched")
	}
	f.Press(5)
	e.waitLog("Taste 5 ist nicht belegt")
	e.noPlay(f, 200*time.Millisecond)
}

// --- Z1: speaker missing at start, connection dropped ------------------------

func TestSpeakerOfflineAtStart(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, false)
	f.Stop()
	e.start()
	e.waitLog("Verbindung zur Box verloren")
	time.Sleep(300 * time.Millisecond) // several failed attempts
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	e.ready(f)
	f.Press(2)
	expectStation(t, e.waitPlay(f), 2)
}

func TestReconnectAfterDrop(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	f.DropConnections() // as when a stereo pair re-forms
	e.waitLog("wieder verbunden")
	e.eventually("reconnected", func() bool { return f.Connections() > 0 })
	f.Press(4)
	expectStation(t, e.waitPlay(f), 4)
}

func TestSpeakerRestartsKeepsWorking(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	f.Stop() // plug pulled
	e.waitLog("Verbindung zur Box verloren")
	if err := f.Start(); err != nil { // plugged in again
		t.Fatal(err)
	}
	e.eventually("reconnected", func() bool { return f.Connections() > 0 })
	f.Press(1)
	expectStation(t, e.waitPlay(f), 1)
}

// --- Z2: socket open, speaker silent ----------------------------------------

func TestSilentSpeakerIsDetected(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	f.SetSilent(true)
	f.DropConnections() // the client reconnects into a connection that never answers
	e.waitLog("keine Antwort auf Ping")
	f.SetSilent(false)
	e.eventually("healthy connection again", func() bool {
		return strings.Count(e.log.String(), "wieder verbunden") >= 2
	})
	f.Press(5)
	expectStation(t, e.waitPlay(f), 5)
}

// --- Z3: speaker gets a new address -----------------------------------------

func TestSpeakerMovesToNewAddress(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	if err := f.Move(); err != nil {
		t.Fatal(err)
	}
	e.register("box-a-new", f, false) // only discovery knows the new address
	e.setVisible("box-a-new")
	e.waitLog("neue Adresse")
	e.eventually("reconnected at new address", func() bool { return f.Connections() > 0 })
	f.Press(6)
	expectStation(t, e.waitPlay(f), 6)
}

// --- Z5: double press ---------------------------------------------------------

func TestDoublePressPlaysOnce(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	f.Press(2)
	f.Press(2)
	expectStation(t, e.waitPlay(f), 2)
	e.noPlay(f, 400*time.Millisecond)
}

// --- Z6: slow speaker, latest press wins -------------------------------------

func TestLatestPressWins(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	f.UPnPDelay = 300 * time.Millisecond
	f.Press(1)
	time.Sleep(100 * time.Millisecond)
	f.Press(4)
	expectStation(t, e.waitPlay(f), 4)
	e.noPlay(f, 800*time.Millisecond)
}

// --- Z7: bad input never stops the bridge ------------------------------------

func TestGarbageMessages(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	for _, m := range []string{
		"not xml at all",
		"<updates><nowSelectionUpdated><preset id=",
		`<updates deviceID="X"><nowSelectionUpdated><preset id="99"/></nowSelectionUpdated></updates>`,
		`<updates deviceID="X"><groupUpdated><group><status>???</status></group></groupUpdated></updates>`,
		strings.Repeat("<a>", 10000),
	} {
		f.Push(m)
	}
	f.Press(3)
	expectStation(t, e.waitPlay(f), 3)
}

// --- Z8: presets lost ----------------------------------------------------------

func TestLostPresetsAreRestored(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	f.ForgetPresets()
	e.ready(f)
	// Restoring only our own presets must not create another backup.
	if b, _ := filepath.Glob(filepath.Join(e.dir, "backups", "*.xml")); len(b) != 1 {
		t.Fatalf("backups = %v", b)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	n := f.Stores()
	time.Sleep(1500 * time.Millisecond) // more than one SyncEvery and several presetsUpdated
	if f.Stores() != n {
		t.Fatalf("presets rewritten without a change: %d → %d", n, f.Stores())
	}
}

// --- Z9 + config reload ----------------------------------------------------------

func TestConfigChangeWithoutRestart(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)

	e.writeConfig("presets: [neu, s2, s3, s4, s5, s6]\nspeakers:\n  - id: " + idA + "\n    host: box-a\n")
	e.waitLog("Taste 1: Sender 1 → Neuer Sender")
	e.eventually("preset 1 rewritten", func() bool { return f.Preset(1).ItemName == "Neuer Sender" })
	f.Press(1)
	if p := e.waitPlay(f); p.Title != "Neuer Sender" || p.Bytes == 0 {
		t.Fatalf("play = %+v", p)
	}

	// A broken file keeps the running configuration.
	if err := os.WriteFile(filepath.Join(e.dir, "strb.yaml"), []byte("stations: [kaputt"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.waitLog("Konfiguration fehlerhaft, die bisherige bleibt aktiv")
	f.Press(2)
	expectStation(t, e.waitPlay(f), 2)
}

func TestPublicURLChangeRewritesPresets(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	_, port, _ := net.SplitHostPort(e.b.Addr())
	newURL := "http://localhost:" + port // same server, different address
	e.writeConfig("public_url: \"" + newURL + "\"\n" + oneBox)
	e.eventually("presets point at the new address", func() bool {
		return strings.HasPrefix(f.Preset(1).Location, newURL) && strings.HasPrefix(f.Preset(6).Location, newURL)
	})
	f.Press(1)
	p := e.waitPlay(f)
	expectStation(t, p, 1)
	if !strings.HasPrefix(p.URI, newURL) {
		t.Fatalf("played %s, want new address", p.URI)
	}
}

// --- Z10: stereo pair ----------------------------------------------------------

func TestStereoPairStatusIsReported(t *testing.T) {
	f := newFake(t, idA, "Links")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": f}, true)
	e.ready(f)
	f.SetGroup(idA, idB, "GROUP_OK")
	e.waitLog("Teil eines Stereo-Paars")
	f.SetGroup(idA, idB, "GROUP_ERROR")
	e.waitLog("Stereo-Paar unvollständig")
	f.SetGroup(idA, idB, "GROUP_OK")
	e.waitLog("Stereo-Paar wieder vollständig")
	f.Press(2)
	expectStation(t, e.waitPlay(f), 2)
}

func TestSecondBoxOfPairIsLeftAlone(t *testing.T) {
	right := newFake(t, idB, "Rechts")
	right.SetGroup(idA, idB, "GROUP_OK") // A is master, this is B
	e := newEnv(t, "presets: [s1]\nspeakers:\n  - id: "+idB+"\n    host: box-b\n", map[string]*fakespeaker.Speaker{"box-b": right}, true)
	e.waitLog("zweite Box eines Stereo-Paars")
	time.Sleep(300 * time.Millisecond)
	if right.Stores() != 0 {
		t.Fatal("presets of the second box must not be written")
	}
	right.Press(1)
	e.waitLog("Nicht-Master-Box")
	e.noPlay(right, 200*time.Millisecond)
}

func TestUnlistedSpeakersAreNotTouched(t *testing.T) {
	a := newFake(t, idA, "Links")
	tv := newFake(t, "CCCCCCCCCCCC", "TV")
	e := newEnv(t, oneBox, map[string]*fakespeaker.Speaker{"box-a": a, "box-tv": tv}, true)
	e.ready(a)
	e.waitLog("TV (box-tv")
	if tv.Stores() != 0 {
		t.Fatal("speaker not under speakers: was changed")
	}
}

func TestTwoSpeakersIndependent(t *testing.T) {
	a := newFake(t, idA, "Links")
	b := newFake(t, idB, "Küche")
	cfg := "presets: [s1, s2, s3, s4, s5, s6]\nspeakers:\n  - id: " + idA + "\n    host: box-a\n  - id: " + idB + "\n    host: box-b\n    presets: [s6, s5, s4, s3, s2, s1]\n"
	e := newEnv(t, cfg, map[string]*fakespeaker.Speaker{"box-a": a, "box-b": b}, true)
	e.ready(a)
	e.ready(b)
	a.Press(1)
	b.Press(1)
	expectStation(t, e.waitPlay(a), 1)
	expectStation(t, e.waitPlay(b), 6)
	a.Stop() // one speaker gone must not disturb the other
	b.Press(2)
	expectStation(t, e.waitPlay(b), 5)
}

// --- start-up -----------------------------------------------------------------

func TestRunFailsWhenPortIsBusy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg, err := config.Parse([]byte("listen: \"" + ln.Addr().String() + "\"\nstations: {a: {url: http://x}}"))
	if err != nil {
		t.Fatal(err)
	}
	b := New(cfg, logx.New(io.Discard, 0), "test")
	b.Search = func(context.Context) []discovery.Found { return nil }
	if err := b.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "läuft strb schon") {
		t.Fatalf("err = %v", err)
	}
}
