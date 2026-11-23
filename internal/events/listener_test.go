package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeSpeaker is a WebSocket server that behaves like the speaker's port 8080.
type fakeSpeaker struct {
	srv     *httptest.Server
	conns   atomic.Int32
	mu      sync.Mutex
	current *websocket.Conn
	silent  atomic.Bool // accept, but never read: pings stay unanswered
}

func newFake(t *testing.T) *fakeSpeaker {
	f := &fakeSpeaker{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"gabbo"}})
		if err != nil {
			return
		}
		f.conns.Add(1)
		f.mu.Lock()
		f.current = c
		f.mu.Unlock()
		_ = c.Write(r.Context(), websocket.MessageText, []byte(msgHello))
		if f.silent.Load() {
			<-r.Context().Done()
			return
		}
		for { // reading is what answers pings
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSpeaker) url() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func (f *fakeSpeaker) send(t *testing.T, msg string) {
	t.Helper()
	f.mu.Lock()
	c := f.current
	f.mu.Unlock()
	if err := c.Write(context.Background(), websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeSpeaker) drop() {
	f.mu.Lock()
	f.current.CloseNow()
	f.mu.Unlock()
}

type recorder struct {
	msgs   chan string
	states chan bool
}

func startListener(t *testing.T, f *fakeSpeaker, tune func(*Listener)) *recorder {
	r := &recorder{msgs: make(chan string, 32), states: make(chan bool, 32)}
	l := &Listener{
		URL:        f.url,
		OnMessage:  func(b []byte) { r.msgs <- string(b) },
		OnState:    func(up bool, _ error) { r.states <- up },
		MinBackoff: 20 * time.Millisecond,
		MaxBackoff: 100 * time.Millisecond,
	}
	if tune != nil {
		tune(l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

func wait[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
	var zero T
	return zero
}

func expectMsg(t *testing.T, r *recorder, want string) {
	t.Helper()
	for {
		if got := wait(t, r.msgs, want); got == want {
			return
		}
	}
}

func TestListenerReceives(t *testing.T) {
	f := newFake(t)
	r := startListener(t, f, nil)
	if !wait(t, r.states, "connect") {
		t.Fatal("expected up")
	}
	expectMsg(t, r, msgHello)
	f.send(t, msgPresetUpnp)
	expectMsg(t, r, msgPresetUpnp)
}

// Z1: the speaker drops the connection (as it does when a stereo pair re-forms).
func TestListenerReconnectsAfterDrop(t *testing.T) {
	f := newFake(t)
	r := startListener(t, f, nil)
	wait(t, r.states, "connect")
	expectMsg(t, r, msgHello)

	f.drop()
	if wait(t, r.states, "down") {
		t.Fatal("expected down after drop")
	}
	if !wait(t, r.states, "reconnect") {
		t.Fatal("expected up again")
	}
	expectMsg(t, r, msgHello)
	f.send(t, msgPresetTune)
	expectMsg(t, r, msgPresetTune)
}

// Z1: the speaker is not there at start and appears later.
func TestListenerWaitsForSpeaker(t *testing.T) {
	f := newFake(t)
	var ready atomic.Bool
	r := startListener(t, f, func(l *Listener) {
		l.URL = func() string {
			if ready.Load() {
				return f.url()
			}
			return "ws://127.0.0.1:1" // nothing listens here
		}
	})
	if wait(t, r.states, "failed dial") {
		t.Fatal("expected down while speaker is missing")
	}
	ready.Store(true)
	for !wait(t, r.states, "connect") {
	}
	expectMsg(t, r, msgHello)
}

// Z2: the socket stays open but the speaker never answers again.
func TestListenerDetectsSilentSpeaker(t *testing.T) {
	f := newFake(t)
	f.silent.Store(true)
	r := startListener(t, f, func(l *Listener) {
		l.PingEvery = 50 * time.Millisecond
		l.PongTimeout = 50 * time.Millisecond
	})
	wait(t, r.states, "connect")
	if wait(t, r.states, "ping timeout") {
		t.Fatal("expected down after missing pong")
	}
	if f.conns.Load() < 1 {
		t.Fatal("no connection made")
	}
}

// Z2: periodic renew opens a new connection without a gap.
func TestListenerRenews(t *testing.T) {
	f := newFake(t)
	r := startListener(t, f, func(l *Listener) { l.RenewEvery = 100 * time.Millisecond })
	wait(t, r.states, "connect")
	deadline := time.Now().Add(2 * time.Second)
	for f.conns.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := f.conns.Load(); n < 3 {
		t.Fatalf("expected renewed connections, got %d", n)
	}
	select {
	case up := <-r.states:
		if !up {
			t.Fatal("renew must not report the connection as down")
		}
	default:
	}
	f.send(t, msgPresetTune) // goes over the newest connection
	expectMsg(t, r, msgPresetTune)
}
