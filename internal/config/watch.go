package config

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"
)

// Watch polls path every interval and calls onChange whenever the file
// content changes. On a parse error, onChange receives the error and the
// caller keeps its previous configuration. Polling (instead of fsnotify)
// keeps strb dependency-free and works with editors that replace files.
func Watch(ctx context.Context, path string, interval time.Duration, onChange func(*Config, error)) {
	settle := min(300*time.Millisecond, interval)
	last, _ := os.ReadFile(path)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.Equal(data, last) {
			continue // a missing file mid-save is not a change
		}
		c, err := Parse(data)
		if err != nil {
			// The file may be half-written by an editor: look again shortly
			// and only report an error for content that stays the same.
			select {
			case <-ctx.Done():
				return
			case <-time.After(settle):
			}
			if again, rerr := os.ReadFile(path); rerr != nil || !bytes.Equal(again, data) {
				continue // still changing; the next tick picks it up
			}
		}
		last = data
		if c != nil {
			c.Path = path
		}
		onChange(c, err)
	}
}

// Diff describes what changed between two configurations in human terms,
// e.g. "Taste 2: Lounge FM → Radio Paradise". Only preset assignments and
// station URLs are compared, per managed speaker list entry.
func Diff(old, cur *Config, deviceID string) []string {
	var out []string
	a, b := old.PresetsFor(deviceID), cur.PresetsFor(deviceID)
	for i := range a {
		switch {
		case a[i] == nil && b[i] == nil:
		case a[i] == nil:
			out = append(out, fmt.Sprintf("Taste %d: – → %s", i+1, b[i].Name))
		case b[i] == nil:
			out = append(out, fmt.Sprintf("Taste %d: %s → –", i+1, a[i].Name))
		case a[i].Name != b[i].Name:
			out = append(out, fmt.Sprintf("Taste %d: %s → %s", i+1, a[i].Name, b[i].Name))
		case a[i].URL != b[i].URL || a[i].Logo != b[i].Logo:
			out = append(out, fmt.Sprintf("Taste %d: %s (neue Adresse)", i+1, b[i].Name))
		}
	}
	return out
}
