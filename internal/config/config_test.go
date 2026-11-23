package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
stations:
  swr3:  { name: "SWR3", url: "https://liveradio.swr.de/sw282p3/swr3/play.mp3" }
  jazz:  { name: "Jazz Cafe", url: "radio.example.org:8000/jazz" }
presets: [swr3, jazz]
speakers:
  - id: aabbccddeeff
    host: 192.168.1.50
  - id: 112233445566
    presets: [jazz, "", swr3]
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen {
		t.Errorf("Listen = %q, want default", c.Listen)
	}
	if got := c.Stations["jazz"].URL; got != "http://radio.example.org:8000/jazz" {
		t.Errorf("scheme not added: %q", got)
	}
	if c.Speakers[0].ID != "AABBCCDDEEFF" {
		t.Errorf("deviceID not upper-cased: %q", c.Speakers[0].ID)
	}

	p := c.PresetsFor("AABBCCDDEEFF")
	if p[0].Name != "SWR3" || p[1].Name != "Jazz Cafe" || p[2] != nil {
		t.Errorf("default presets wrong: %+v", p)
	}
	p = c.PresetsFor("112233445566")
	if p[0].Name != "Jazz Cafe" || p[1] != nil || p[2].Name != "SWR3" {
		t.Errorf("per-speaker presets wrong: %+v", p)
	}
	if !c.Manages("AABBCCDDEEFF") || c.Manages("FFFFFFFFFFFF") {
		t.Error("Manages wrong")
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"unknown field":   {"stations: {a: {url: http://x}}\nfoo: 1", "foo"},
		"no stations":     {"presets: []", "mindestens ein Sender"},
		"bad url":         {"stations: {a: {url: 'ftp://x'}}", "stations.a.url"},
		"unknown station": {"stations: {a: {url: http://x}}\npresets: [b]", `unbekannter Sender "b"`},
		"too many":        {"stations: {a: {url: http://x}}\npresets: [a,a,a,a,a,a,a]", "höchstens 6"},
		"https public":    {"stations: {a: {url: http://x}}\npublic_url: https://h:1", "public_url"},
		"duplicate id":    {"stations: {a: {url: http://x}}\nspeakers: [{id: A}, {id: a}]", "doppelt"},
		"missing id":      {"stations: {a: {url: http://x}}\nspeakers: [{host: h}]", "fehlt"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestManagesAllWhenNoSpeakers(t *testing.T) {
	c, err := Parse([]byte("stations: {a: {url: http://x}}"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Manages("ANY") {
		t.Error("empty speaker list must manage every speaker")
	}
}

func TestDiff(t *testing.T) {
	a, _ := Parse([]byte("stations: {a: {name: A, url: http://a}, b: {name: B, url: http://b}}\npresets: [a, b]"))
	b, _ := Parse([]byte("stations: {a: {name: A, url: http://a2}, b: {name: B, url: http://b}}\npresets: [b, a, a]"))
	got := strings.Join(Diff(a, b, "X"), "; ")
	want := "Taste 1: A → B; Taste 2: B → A; Taste 3: – → A"
	if got != want {
		t.Errorf("Diff = %q, want %q", got, want)
	}
}

func TestWatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "strb.yaml")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("stations: {a: {url: http://a}}")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		c   *Config
		err error
	}
	ch := make(chan result, 4)
	go Watch(ctx, path, 10*time.Millisecond, func(c *Config, err error) { ch <- result{c, err} })

	time.Sleep(30 * time.Millisecond)
	write("stations: {b: {url: http://b}}")
	r := <-ch
	if r.err != nil || r.c.Stations["b"].URL != "http://b" {
		t.Fatalf("valid change: %+v", r)
	}
	write("stations: [broken")
	if r = <-ch; r.err == nil {
		t.Fatal("broken file must report an error")
	}
}
