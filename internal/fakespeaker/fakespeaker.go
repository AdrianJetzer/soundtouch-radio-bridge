// Package fakespeaker simulates a SoundTouch speaker for tests: the Web API
// (presets, info, group), the WebSocket notifications and the UPnP
// AVTransport. Its behaviour follows what M0 recorded on a real
// SoundTouch 10 (docs/M0-FINDINGS.md). When told to play, it fetches the
// stream like a real speaker does, so tests see the full chain.
package fakespeaker

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/speaker"
	"github.com/coder/websocket"
)

// Play is one stream the speaker started.
type Play struct {
	URI, Title string
	Bytes      int   // bytes received in the first moment
	Err        error // fetching the stream failed
}

// Speaker is a simulated SoundTouch speaker on 127.0.0.1.
type Speaker struct {
	ID, Name string

	// UPnPDelay delays every UPnP answer (slow speaker, Z6).
	UPnPDelay time.Duration

	Plays chan Play // every started stream, in order

	mu       sync.Mutex
	presets  map[int]speaker.ContentItem
	group    *speaker.Group
	silent   bool
	ports    [3]int // api, ws, upnp
	servers  []*http.Server
	conns    map[*websocket.Conn]context.CancelFunc
	uri      string
	title    string
	stopPlay context.CancelFunc
	stores   int
}

// New starts a speaker with the given deviceID. It is stopped at test end.
func New(id, name string) (*Speaker, error) {
	s := &Speaker{ID: id, Name: name, Plays: make(chan Play, 64),
		presets: map[int]speaker.ContentItem{}, conns: map[*websocket.Conn]context.CancelFunc{}}
	for i := 1; i <= 6; i++ { // like the real box after the cloud shutdown
		s.presets[i] = speaker.ContentItem{Source: "TUNEIN", Type: "stationurl", Location: fmt.Sprintf("/v1/playback/station/s%d", i), IsPresetable: true, ItemName: fmt.Sprintf("Old TuneIn %d", i)}
	}
	return s, s.Start()
}

// Ports returns the API, WebSocket and UPnP ports.
func (s *Speaker) Ports() (api, ws, upnp int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ports[0], s.ports[1], s.ports[2]
}

// Start (re)opens the three services. After Stop it reuses the same ports,
// like a speaker that was unplugged and comes back.
func (s *Speaker) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	handlers := []http.Handler{s.apiHandler(), http.HandlerFunc(s.serveWS), s.upnpHandler()}
	for i, h := range handlers {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.ports[i]))
		if err != nil {
			return err
		}
		s.ports[i] = ln.Addr().(*net.TCPAddr).Port
		srv := &http.Server{Handler: h}
		s.servers = append(s.servers, srv)
		go srv.Serve(ln)
	}
	return nil
}

// Stop closes every service and connection, like pulling the plug.
func (s *Speaker) Stop() {
	s.mu.Lock()
	servers := s.servers
	s.servers = nil
	for c, cancel := range s.conns {
		cancel()
		c.CloseNow()
	}
	s.conns = map[*websocket.Conn]context.CancelFunc{}
	if s.stopPlay != nil {
		s.stopPlay()
	}
	s.mu.Unlock()
	for _, srv := range servers {
		srv.Close()
	}
}

// Move restarts the speaker on new ports (a new IP address).
func (s *Speaker) Move() error {
	s.Stop()
	s.mu.Lock()
	s.ports = [3]int{}
	s.mu.Unlock()
	return s.Start()
}

// SetSilent makes new WebSocket connections stay open but never answer a
// ping, like a speaker that lost power without closing the connection (Z2).
// Combine with DropConnections to make the current client reconnect into
// a silent connection.
func (s *Speaker) SetSilent(v bool) {
	s.mu.Lock()
	s.silent = v
	s.mu.Unlock()
}

// DropConnections closes the WebSockets, as the real box does when a
// stereo pair re-forms.
func (s *Speaker) DropConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c, cancel := range s.conns {
		cancel()
		c.CloseNow()
	}
	s.conns = map[*websocket.Conn]context.CancelFunc{}
}

// Connections is the number of open WebSockets.
func (s *Speaker) Connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// SetGroup makes the speaker part of a stereo pair and announces it.
func (s *Speaker) SetGroup(masterID, partnerID, status string) {
	g := &speaker.Group{ID: "1", Name: "Pair", MasterID: masterID, Status: status}
	g.Roles = append(g.Roles,
		struct {
			DeviceID string `xml:"deviceId"`
			Role     string `xml:"role"`
		}{masterID, "LEFT"},
		struct {
			DeviceID string `xml:"deviceId"`
			Role     string `xml:"role"`
		}{partnerID, "RIGHT"})
	s.mu.Lock()
	s.group = g
	s.mu.Unlock()
	s.Push(fmt.Sprintf(`<updates deviceID='%s'><groupUpdated>%s</groupUpdated></updates>`, s.ID, groupXML(g)))
}

// Preset returns what is stored in a slot.
func (s *Speaker) Preset(slot int) speaker.ContentItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.presets[slot]
}

// Stores is how often presets were written.
func (s *Speaker) Stores() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stores
}

// ForgetPresets simulates firmware losing its presets (Z8).
func (s *Speaker) ForgetPresets() {
	s.mu.Lock()
	s.presets = map[int]speaker.ContentItem{}
	s.mu.Unlock()
	s.Push(fmt.Sprintf(`<updates deviceID="%s"><presetsUpdated/></updates>`, s.ID))
}

// Press simulates pressing a hardware button, with the messages the real
// box sends: the selection, then error 1036 for UPNP presets.
func (s *Speaker) Press(slot int) {
	s.mu.Lock()
	item := s.presets[slot]
	s.mu.Unlock()
	b, _ := xml.Marshal(item)
	s.Push(fmt.Sprintf(`<updates deviceID="%s"><nowSelectionUpdated><preset id="%d">%s</preset></nowSelectionUpdated></updates>`, s.ID, slot, b))
	s.Push(fmt.Sprintf(`<userActivityUpdate deviceID="%s" />`, s.ID))
	if item.Source == "UPNP" {
		s.Push(fmt.Sprintf(`<errorUpdate deviceID="%s"><error value="1036" name="UNABLE_TO_PROCESS_NOT_LOGGED_IN" severity="Unrecoverable">UpnpRcvdContentItemInWrongState</error></errorUpdate>`, s.ID))
	}
}

// PressPower simulates the power button (preset id 0).
func (s *Speaker) PressPower() {
	s.Push(fmt.Sprintf(`<updates deviceID="%s"><nowSelectionUpdated><preset id="0"><ContentItem source="INVALID_SOURCE" type="DO_NOT_RESUME" location="unplayable location" sourceAccount="UPnPUserName" isPresetable="false" /></preset></nowSelectionUpdated></updates>`, s.ID))
}

// Push sends a raw message to every connected WebSocket client.
func (s *Speaker) Push(msg string) {
	s.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = c.Write(ctx, websocket.MessageText, []byte(msg))
		cancel()
	}
}

// --- WebSocket :8080 ---------------------------------------------------

func (s *Speaker) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"gabbo"}})
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.mu.Lock()
	s.conns[c] = cancel
	silent := s.silent
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		c.CloseNow()
	}()
	_ = c.Write(ctx, websocket.MessageText, []byte(`<SoundTouchSdkInfo serverVersion="4" serverBuild="fake" />`))
	if silent { // never read: pings stay unanswered although the socket is open
		<-ctx.Done()
		return
	}
	for { // reading answers pings
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
	}
}

// --- Web API :8090 -----------------------------------------------------

func (s *Speaker) apiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8" ?><info deviceID="%s"><name>%s</name><type>SoundTouch 10</type><components><component><componentCategory>SCM</componentCategory><softwareVersion>27.0.6.46330.5043500 fake</softwareVersion></component></components></info>`, s.ID, html.EscapeString(s.Name))
	})
	mux.HandleFunc("GET /presets", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		ids := make([]int, 0, len(s.presets))
		for id := range s.presets {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8" ?><presets>`)
		for _, id := range ids {
			item, _ := xml.Marshal(s.presets[id])
			fmt.Fprintf(&b, `<preset id="%d">%s</preset>`, id, item)
		}
		b.WriteString(`</presets>`)
		io.WriteString(w, b.String())
	})
	mux.HandleFunc("POST /storePreset", func(w http.ResponseWriter, r *http.Request) {
		var p speaker.Preset
		if err := xml.NewDecoder(r.Body).Decode(&p); err != nil || p.ID < 1 || p.ID > 6 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `<errors><error>bad preset</error></errors>`)
			return
		}
		s.mu.Lock()
		s.presets[p.ID] = p.Item
		s.stores++
		s.mu.Unlock()
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8" ?><presets/>`)
		s.Push(fmt.Sprintf(`<updates deviceID="%s"><presetsUpdated/></updates>`, s.ID))
	})
	mux.HandleFunc("GET /getGroup", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		g := s.group
		s.mu.Unlock()
		if g == nil {
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8" ?><group />`)
			return
		}
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8" ?>`+groupXML(g))
	})
	mux.HandleFunc("GET /now_playing", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		uri := s.uri
		s.mu.Unlock()
		src := "STANDBY"
		if uri != "" {
			src = "UPNP"
		}
		fmt.Fprintf(w, `<nowPlaying deviceID="%s" source="%s"></nowPlaying>`, s.ID, src)
	})
	return mux
}

func groupXML(g *speaker.Group) string {
	var roles strings.Builder
	for _, r := range g.Roles {
		fmt.Fprintf(&roles, `<groupRole><deviceId>%s</deviceId><role>%s</role><ipAddress>192.168.1.34</ipAddress></groupRole>`, r.DeviceID, r.Role)
	}
	return fmt.Sprintf(`<group id="%s"><name>%s</name><masterDeviceId>%s</masterDeviceId><roles>%s</roles><status>%s</status></group>`, g.ID, g.Name, g.MasterID, roles.String(), g.Status)
}

// --- UPnP :8091 --------------------------------------------------------

var (
	reURI   = regexp.MustCompile(`<CurrentURI>(.*?)</CurrentURI>`)
	reMeta  = regexp.MustCompile(`<CurrentURIMetaData>(.*?)</CurrentURIMetaData>`)
	reTitle = regexp.MustCompile(`<dc:title>(.*?)</dc:title>`)
)

func (s *Speaker) upnpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /AVTransport/Control", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		delay := s.UPnPDelay
		s.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		body, _ := io.ReadAll(r.Body)
		action := strings.Trim(r.Header.Get("SOAPAction"), `"`)
		action = action[strings.LastIndex(action, "#")+1:]
		switch action {
		case "Stop":
			s.mu.Lock()
			if s.stopPlay != nil {
				s.stopPlay()
			}
			s.uri = ""
			s.mu.Unlock()
		case "SetAVTransportURI":
			m := reURI.FindSubmatch(body)
			if m == nil {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>402</errorCode><errorDescription>No URI supplied</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
				return
			}
			title := ""
			if mm := reMeta.FindSubmatch(body); mm != nil {
				if t := reTitle.FindStringSubmatch(html.UnescapeString(string(mm[1]))); t != nil {
					title = html.UnescapeString(t[1])
				}
			}
			s.mu.Lock()
			s.uri = html.UnescapeString(string(m[1]))
			s.title = title
			s.mu.Unlock()
		case "Play":
			s.startStream()
		}
		fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:%sResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"></u:%sResponse></s:Body></s:Envelope>`, action, action)
	})
	return mux
}

// startStream fetches the current URI like the real box: one stream at a time.
func (s *Speaker) startStream() {
	s.mu.Lock()
	if s.stopPlay != nil {
		s.stopPlay()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stopPlay = cancel
	uri, title := s.uri, s.title
	s.mu.Unlock()
	go func() {
		p := Play{URI: uri, Title: title}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
		if err == nil {
			req.Header.Set("User-Agent", "Bose/7.22.0")
			var resp *http.Response
			if resp, err = http.DefaultClient.Do(req); err == nil {
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					err = fmt.Errorf("stream answered %s", resp.Status)
				} else {
					buf := make([]byte, 64)
					p.Bytes, err = io.ReadAtLeast(resp.Body, buf, 16)
				}
			}
		}
		p.Err = err
		select {
		case s.Plays <- p:
		default:
		}
		if err == nil { // keep listening like a speaker until stopped
			<-ctx.Done()
		}
	}()
}
