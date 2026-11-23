package events

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/coder/websocket"
)

// Port is the WebSocket notification port of SoundTouch speakers.
const Port = 8080

// Listener keeps one WebSocket open to a speaker, forever:
//   - reconnects with exponential backoff (MinBackoff…MaxBackoff), no limit
//   - pings every PingEvery; a missing pong within PongTimeout means the
//     connection is dead (a speaker that lost power never closes it)
//   - replaces the connection every RenewEvery, opening the new one before
//     closing the old one so no press falls into the gap
type Listener struct {
	// URL returns the WebSocket address to dial; called before every dial so
	// a changed speaker address is picked up. Typically ws://<ip>:8080.
	URL func() string

	OnMessage func(raw []byte)                // every text message, in order
	OnState   func(connected bool, err error) // connection up/down

	PingEvery   time.Duration
	PongTimeout time.Duration
	RenewEvery  time.Duration
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
}

// Defaults fills unset timings with the values from the spec (Z1, Z2).
func (l *Listener) Defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	set(&l.PingEvery, 30*time.Second)
	set(&l.PongTimeout, 10*time.Second)
	set(&l.RenewEvery, 10*time.Minute)
	set(&l.MinBackoff, time.Second)
	set(&l.MaxBackoff, 60*time.Second)
}

type conn struct {
	ws   *websocket.Conn
	gen  int
	done chan error // closed reader result
}

// Run blocks until ctx is cancelled.
func (l *Listener) Run(ctx context.Context) {
	l.Defaults()
	backoff := l.MinBackoff
	gen := 0
	for ctx.Err() == nil {
		gen++
		c, err := l.dial(ctx, gen)
		if err != nil {
			l.state(false, err)
			if !sleep(ctx, jitter(backoff)) {
				return
			}
			backoff = min(backoff*2, l.MaxBackoff)
			continue
		}
		backoff = l.MinBackoff
		l.state(true, nil)
		err = l.serve(ctx, c, &gen)
		if ctx.Err() != nil {
			return
		}
		l.state(false, err)
		if !sleep(ctx, jitter(l.MinBackoff)) {
			return
		}
	}
}

// serve watches the current connection, pings it and renews it. It returns
// when the connection is lost.
func (l *Listener) serve(ctx context.Context, cur *conn, gen *int) error {
	ping := time.NewTicker(l.PingEvery)
	defer ping.Stop()
	renew := time.NewTimer(l.RenewEvery)
	defer renew.Stop()
	defer func() { cur.ws.CloseNow() }()

	for {
		select {
		case <-ctx.Done():
			cur.ws.Close(websocket.StatusNormalClosure, "")
			return ctx.Err()
		case err := <-cur.done:
			return fmt.Errorf("Verbindung beendet: %w", err)
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, l.PongTimeout)
			err := cur.ws.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				return fmt.Errorf("keine Antwort auf Ping innerhalb %s: %w", l.PongTimeout, err)
			}
		case <-renew.C:
			*gen++
			next, err := l.dial(ctx, *gen)
			if err == nil {
				old := cur
				cur = next
				old.ws.Close(websocket.StatusNormalClosure, "renew")
			} // if the renew dial fails we keep the old, still working connection
			renew.Reset(l.RenewEvery)
		}
	}
}

func (l *Listener) dial(ctx context.Context, gen int) (*conn, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, l.URL(), &websocket.DialOptions{Subprotocols: []string{"gabbo"}})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(1 << 20)
	c := &conn{ws: ws, gen: gen, done: make(chan error, 1)}
	go func() {
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				c.done <- err
				return
			}
			if typ == websocket.MessageText && l.OnMessage != nil {
				l.OnMessage(data)
			}
		}
	}()
	return c, nil
}

func (l *Listener) state(up bool, err error) {
	if l.OnState != nil && !errors.Is(err, context.Canceled) {
		l.OnState(up, err)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}
