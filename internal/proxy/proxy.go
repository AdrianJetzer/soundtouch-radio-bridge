// Package proxy relays internet radio streams to the speakers as plain HTTP.
// SoundTouch speakers cannot play https:// streams; the proxy fetches the
// station (http or https, following redirects and simple playlists) and
// passes the bytes through unchanged.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/config"
)

// Lookup resolves a speaker button or a station key to a station.
type Lookup interface {
	Button(deviceID string, slot int) (*config.Station, bool)
	Station(key string) (*config.Station, bool)
}

// Proxy is an http.Handler serving
//
//	GET|HEAD /stream/{deviceID}/{slot}  stream for a speaker button
//	GET|HEAD /station/{key}             stream of a station (for tests)
//	GET      /healthz                   "ok"
type Proxy struct {
	Lookup    Lookup
	Client    *http.Client
	Log       *slog.Logger
	UserAgent string

	mu     sync.Mutex
	active map[string]activeStream // one upstream per speaker
	seq    uint64
	mux    *http.ServeMux
	once   sync.Once
}

type activeStream struct {
	id     uint64
	cancel context.CancelFunc
}

// Active reports how many speakers are currently streaming.
func (p *Proxy) Active() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
}

// New returns a proxy with a streaming-friendly HTTP client.
func New(lookup Lookup, log *slog.Logger, userAgent string) *Proxy {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("zu viele Weiterleitungen")
			}
			return nil
		},
	}
	return &Proxy{Lookup: lookup, Client: client, Log: log, UserAgent: userAgent}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.once.Do(func() {
		p.active = map[string]activeStream{}
		p.mux = http.NewServeMux()
		p.mux.HandleFunc("GET /stream/{device}/{slot}", p.handleButton)
		p.mux.HandleFunc("GET /station/{key}", p.handleStation)
		p.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	})
	p.mux.ServeHTTP(w, r)
}

func (p *Proxy) handleButton(w http.ResponseWriter, r *http.Request) {
	device := strings.ToUpper(r.PathValue("device"))
	slot, err := strconv.Atoi(r.PathValue("slot"))
	st, ok := p.Lookup.Button(device, slot)
	if err != nil || !ok {
		p.Log.Warn("Box fragt nach unbekanntem Stream", "pfad", r.URL.Path, "von", r.RemoteAddr)
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	if r.Method == http.MethodGet {
		// Only one live stream per speaker: a new request replaces the old one.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		p.mu.Lock()
		if old, ok := p.active[device]; ok {
			old.cancel()
		}
		p.seq++
		id := p.seq
		p.active[device] = activeStream{id: id, cancel: cancel}
		p.mu.Unlock()
		defer func() {
			p.mu.Lock()
			if p.active[device].id == id {
				delete(p.active, device)
			}
			p.mu.Unlock()
		}()
	}
	p.relay(ctx, w, r, st, slog.String("taste", strconv.Itoa(slot)))
}

func (p *Proxy) handleStation(w http.ResponseWriter, r *http.Request) {
	st, ok := p.Lookup.Station(r.PathValue("key"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	p.relay(r.Context(), w, r, st)
}

func (p *Proxy) relay(ctx context.Context, w http.ResponseWriter, r *http.Request, st *config.Station, attrs ...any) {
	log := p.Log.With(append([]any{"sender", st.Name}, attrs...)...)
	resp, err := p.open(ctx, st.URL, r.Header.Get("Icy-MetaData"))
	if err != nil {
		if ctx.Err() == nil {
			log.Error("Sender nicht erreichbar", "url", st.URL, "fehler", err)
			http.Error(w, fmt.Sprintf("Sender %s nicht erreichbar: %v", st.Name, err), http.StatusBadGateway)
		}
		return
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if ct == "" || strings.HasPrefix(ct, "application/octet-stream") {
		ct = "audio/mpeg"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "no-cache")
	for _, k := range []string{"icy-name", "icy-br", "icy-genre", "icy-url", "icy-description", "icy-metaint"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	log.Debug("Stream läuft", "von", r.RemoteAddr, "typ", ct)
	start := time.Now()
	n, err := io.CopyBuffer(flushWriter{w}, resp.Body, make([]byte, 16<<10))
	dur := time.Since(start).Round(time.Second)
	switch {
	case ctx.Err() != nil:
		log.Debug("Stream beendet (Box hat aufgehört oder anderen Sender gewählt)", "dauer", dur, "bytes", n)
	case err != nil:
		log.Warn("Stream abgebrochen", "dauer", dur, "fehler", err)
	default:
		log.Warn("Sender hat den Stream beendet", "dauer", dur)
	}
}

// open fetches url and follows up to 4 playlist hops (.m3u, .pls).
// The speaker's Range header is deliberately not forwarded (SomaFM answers 403).
func (p *Proxy) open(ctx context.Context, rawURL, icy string) (*http.Response, error) {
	for hop := 0; hop < 5; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", p.UserAgent)
		req.Header.Set("Accept", "*/*")
		if icy != "" {
			req.Header.Set("Icy-MetaData", icy)
		}
		resp, err := p.Client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("Sender antwortet %s", resp.Status)
		}
		if !isPlaylist(resp) {
			return resp, nil
		}
		next, err := firstEntry(resp)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		base, _ := url.Parse(rawURL)
		ref, err := url.Parse(next)
		if err != nil {
			return nil, fmt.Errorf("Playlist-Eintrag %q: %w", next, err)
		}
		rawURL = base.ResolveReference(ref).String()
	}
	return nil, errors.New("zu viele verschachtelte Playlists")
}

func isPlaylist(resp *http.Response) bool {
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch mt {
	case "audio/x-mpegurl", "audio/mpegurl", "application/x-mpegurl", "application/vnd.apple.mpegurl",
		"audio/x-scpls", "application/pls+xml", "audio/scpls":
		return true
	case "audio/mpeg", "audio/aac", "audio/aacp", "audio/ogg", "audio/flac":
		return false
	}
	ext := strings.ToLower(path.Ext(resp.Request.URL.Path))
	return ext == ".m3u" || ext == ".m3u8" || ext == ".pls"
}

// firstEntry returns the first stream URL of an M3U or PLS playlist.
func firstEntry(resp *http.Response) (string, error) {
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 64<<10))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#EXT-X-") {
			return "", errors.New("HLS-Streams (.m3u8) werden noch nicht unterstützt; bitte eine MP3/AAC-Adresse des Senders verwenden")
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.HasPrefix(strings.ToLower(k), "file") {
			return strings.TrimSpace(v), nil // PLS: File1=http://…
		}
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "[") && strings.Contains(line, "://") {
			return line, nil // M3U
		}
	}
	return "", errors.New("Playlist enthält keinen Stream")
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
