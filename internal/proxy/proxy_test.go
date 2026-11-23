package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/config"
	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/logx"
)

type lookup map[string]*config.Station

func (l lookup) Button(dev string, slot int) (*config.Station, bool) {
	s, ok := l[dev+"/"+string(rune('0'+slot))]
	return s, ok
}
func (l lookup) Station(key string) (*config.Station, bool) { s, ok := l[key]; return s, ok }

// radio is a fake upstream: /live streams forever, the rest is routing.
func radio(t *testing.T, tls bool) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	var live atomic.Int32
	var lastRange atomic.Value
	lastRange.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("/live", func(w http.ResponseWriter, r *http.Request) {
		lastRange.Store(r.Header.Get("Range"))
		if r.Header.Get("Range") != "" {
			http.Error(w, "no range", http.StatusForbidden) // like SomaFM
			return
		}
		live.Add(1)
		defer live.Add(-1)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("icy-name", "Test FM")
		for {
			if _, err := w.Write([]byte("MP3DATA")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/live", http.StatusFound) })
	mux.HandleFunc("/list.pls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/x-scpls")
		io.WriteString(w, "[playlist]\nNumberOfEntries=1\nFile1=/live\n")
	})
	mux.HandleFunc("/list.m3u", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/x-mpegurl")
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		io.WriteString(w, "#EXTM3U\n#EXTINF:-1,Test\n"+scheme+"://"+r.Host+"/live\n")
	})
	mux.HandleFunc("/hls.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:3\nseg.ts\n")
	})
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(mux)
	} else {
		srv = httptest.NewServer(mux)
	}
	t.Cleanup(srv.Close)
	return srv, &live, &lastRange
}

func newProxy(t *testing.T, up *httptest.Server, l lookup) *httptest.Server {
	p := New(l, logx.New(io.Discard, 0), "strb-test")
	if up.TLS != nil {
		p.Client.Transport = up.Client().Transport // trust the test certificate
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv
}

func readSome(t *testing.T, url string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	buf := make([]byte, 7)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	return resp, string(buf)
}

func TestRelay(t *testing.T) {
	for _, tls := range []bool{false, true} {
		name := map[bool]string{false: "http", true: "https"}[tls]
		t.Run(name, func(t *testing.T) {
			up, _, lastRange := radio(t, tls)
			cases := map[string]string{"direct": "/live", "redirect": "/redirect", "pls": "/list.pls", "m3u": "/list.m3u"}
			for cname, path := range cases {
				t.Run(cname, func(t *testing.T) {
					px := newProxy(t, up, lookup{"st": {Name: "Test", URL: up.URL + path}})
					resp, body := readSome(t, px.URL+"/station/st", map[string]string{"Range": "bytes=0-"})
					if resp.StatusCode != 200 || body != "MP3DATA" {
						t.Fatalf("status %d body %q", resp.StatusCode, body)
					}
					if resp.Header.Get("Content-Type") != "audio/mpeg" || resp.Header.Get("icy-name") != "Test FM" {
						t.Errorf("headers %v", resp.Header)
					}
					if lastRange.Load().(string) != "" {
						t.Error("Range header must not be forwarded")
					}
				})
			}
		})
	}
}

func TestErrors(t *testing.T) {
	up, _, _ := radio(t, false)
	px := newProxy(t, up, lookup{
		"gone":   {Name: "Gone", URL: up.URL + "/gone"},
		"hls":    {Name: "HLS", URL: up.URL + "/hls.m3u8"},
		"AABB/1": {Name: "x", URL: up.URL + "/live"},
	})
	if resp, body := readSome(t, px.URL+"/station/gone", nil); resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "404") {
		t.Errorf("gone: %d %q", resp.StatusCode, body)
	}
	if resp, body := readSome(t, px.URL+"/station/hls", nil); resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "HLS") {
		t.Errorf("hls: %d %q", resp.StatusCode, body)
	}
	if resp, _ := readSome(t, px.URL+"/stream/AABB/2", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown button: %d", resp.StatusCode)
	}
	if resp, _ := readSome(t, px.URL+"/stream/AABB/x", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bad slot: %d", resp.StatusCode)
	}
}

func TestHead(t *testing.T) {
	up, _, _ := radio(t, false)
	px := newProxy(t, up, lookup{"AABB/1": {Name: "x", URL: up.URL + "/live"}})
	resp, err := http.Head(px.URL + "/stream/aabb/1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Errorf("HEAD: %d %v", resp.StatusCode, resp.Header)
	}
}

// A new request of the same speaker replaces the running upstream.
func TestOneStreamPerSpeaker(t *testing.T) {
	up, live, _ := radio(t, false)
	px := newProxy(t, up, lookup{
		"AABB/1": {Name: "a", URL: up.URL + "/live"},
		"AABB/2": {Name: "b", URL: up.URL + "/live"},
		"CCDD/1": {Name: "c", URL: up.URL + "/live"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	open := func(path string) *bufio.Reader {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, px.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		r := bufio.NewReader(resp.Body)
		if _, err := r.Peek(7); err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := open("/stream/AABB/1")
	open("/stream/CCDD/1") // another speaker is independent
	open("/stream/AABB/2") // replaces the first stream of AABB

	deadline := time.Now().Add(2 * time.Second)
	for live.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := live.Load(); n != 2 {
		t.Fatalf("live upstreams = %d, want 2", n)
	}
	_, err := io.Copy(io.Discard, first) // first stream must end
	if err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Logf("first stream ended with %v", err)
	}
}
