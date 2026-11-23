// Package logx provides a compact, human-readable slog handler:
//
//	20:01:03 INFO  [Links] Taste 3 → SwissGroove ▶ ok  ms=420
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// Handler writes one line per record.
type Handler struct {
	w      io.Writer
	level  slog.Level
	mu     *sync.Mutex
	prefix string // from WithAttrs("box", …)
	attrs  []slog.Attr
}

// New returns a logger writing to w at the given minimum level.
func New(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(&Handler{w: w, level: level, mu: &sync.Mutex{}})
}

func (h *Handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format("15:04:05"))
	b.WriteByte(' ')
	b.WriteString(fmt.Sprintf("%-5s ", levelName(r.Level)))
	if h.prefix != "" {
		b.WriteString("[" + h.prefix + "] ")
	}
	b.WriteString(r.Message)
	write := func(a slog.Attr) {
		if a.Key == "" {
			return
		}
		b.WriteString("  " + a.Key + "=")
		v := a.Value.String()
		if strings.ContainsAny(v, " \t") {
			v = fmt.Sprintf("%q", v)
		}
		b.WriteString(v)
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(func(a slog.Attr) bool { write(a); return true })
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

// WithAttrs turns a "box" attribute into the [prefix]; others are appended.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append([]slog.Attr{}, h.attrs...)
	for _, a := range attrs {
		if a.Key == "box" {
			n.prefix = a.Value.String()
			continue
		}
		n.attrs = append(n.attrs, a)
	}
	return &n
}

func (h *Handler) WithGroup(string) slog.Handler { return h }

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "FEHLER"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}
