// Package upnp starts streams on a SoundTouch speaker through its UPnP
// AVTransport service on port 8091.
package upnp

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Port is the UPnP port of SoundTouch speakers.
const Port = 8091

const avTransport = "urn:schemas-upnp-org:service:AVTransport:1"

// Media is what to play.
type Media struct {
	URI   string // must be plain http://
	Title string
	Art   string // optional album art URL
	MIME  string // defaults to audio/mpeg
}

// Player controls one speaker.
type Player struct {
	Base     string // http://<host>:8091
	DeviceID string
	HTTP     *http.Client

	mu      sync.Mutex
	control string // cached control URL
}

// New returns a player for the speaker at host.
func New(host, deviceID string) *Player {
	return &Player{
		Base:     fmt.Sprintf("http://%s:%d", host, Port),
		DeviceID: deviceID,
		HTTP:     &http.Client{Timeout: 8 * time.Second},
	}
}

// Play runs Stop (errors ignored: an idle speaker may refuse it),
// SetAVTransportURI and Play.
func (p *Player) Play(ctx context.Context, m Media) error {
	_ = p.call(ctx, "Stop", "")
	args := "<CurrentURI>" + html.EscapeString(m.URI) + "</CurrentURI>" +
		"<CurrentURIMetaData>" + html.EscapeString(DIDL(m)) + "</CurrentURIMetaData>"
	if err := p.call(ctx, "SetAVTransportURI", args); err != nil {
		return err
	}
	return p.call(ctx, "Play", "<Speed>1</Speed>")
}

// Stop stops playback.
func (p *Player) Stop(ctx context.Context) error { return p.call(ctx, "Stop", "") }

// DIDL builds the DIDL-Lite metadata the speaker shows on its display.
func DIDL(m Media) string {
	mime := m.MIME
	if mime == "" {
		mime = "audio/mpeg"
	}
	art := ""
	if m.Art != "" {
		art = "<upnp:albumArtURI>" + html.EscapeString(m.Art) + "</upnp:albumArtURI>"
	}
	return `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">` +
		`<item id="0" parentID="-1" restricted="1">` +
		`<dc:title>` + html.EscapeString(m.Title) + `</dc:title>` +
		`<upnp:class>object.item.audioItem.audioBroadcast</upnp:class>` + art +
		`<res protocolInfo="http-get:*:` + mime + `:*">` + html.EscapeString(m.URI) + `</res>` +
		`</item></DIDL-Lite>`
}

func (p *Player) call(ctx context.Context, action, args string) error {
	ctl, err := p.controlURL(ctx)
	if err != nil {
		return err
	}
	env := `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>` +
		`<u:` + action + ` xmlns:u="` + avTransport + `"><InstanceID>0</InstanceID>` + args + `</u:` + action + `>` +
		`</s:Body></s:Envelope>`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ctl, strings.NewReader(env))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+avTransport+`#`+action+`"`)
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("UPnP %s: %w", action, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK || bytes.Contains(body, []byte("Fault>")) {
		p.mu.Lock()
		p.control = "" // re-read the description next time
		p.mu.Unlock()
		return fmt.Errorf("UPnP %s: %s %s", action, resp.Status, faultText(body))
	}
	return nil
}

// controlURL reads the AVTransport control URL from the device description,
// falling back to the path SoundTouch speakers use.
func (p *Player) controlURL(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.control != "" {
		return p.control, nil
	}
	fallback := p.Base + "/AVTransport/Control"
	desc := fmt.Sprintf("%s/XD/BO5EBO5E-F00D-F00D-FEED-%s.xml", p.Base, p.DeviceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, desc, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return fallback, nil // speaker may be slow; let the SOAP call report the real error
	}
	defer resp.Body.Close()
	var d struct {
		Services []struct {
			Type    string `xml:"serviceType"`
			Control string `xml:"controlURL"`
		} `xml:"device>serviceList>service"`
	}
	if resp.StatusCode == http.StatusOK && xml.NewDecoder(resp.Body).Decode(&d) == nil {
		for _, s := range d.Services {
			if s.Type == avTransport && s.Control != "" {
				base, _ := url.Parse(p.Base + "/")
				ref, err := url.Parse(s.Control)
				if err == nil {
					p.control = base.ResolveReference(ref).String()
					return p.control, nil
				}
			}
		}
	}
	p.control = fallback
	return p.control, nil
}

func faultText(body []byte) string {
	var f struct {
		Code string `xml:"Body>Fault>detail>UPnPError>errorCode"`
		Desc string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
	}
	if xml.Unmarshal(body, &f) == nil && f.Code != "" {
		return fmt.Sprintf("(UPnP-Fehler %s: %s)", f.Code, f.Desc)
	}
	return strings.TrimSpace(string(body))
}
