// Package speaker is a small client for the SoundTouch Web API on port 8090.
package speaker

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Port is the Web API port of every SoundTouch speaker.
const Port = 8090

// Client talks to one speaker. Base defaults to http://<host>:8090.
type Client struct {
	Base string
	HTTP *http.Client
}

// New returns a client for the speaker at host (IP or name, without port).
func New(host string) *Client {
	return &Client{Base: fmt.Sprintf("http://%s:%d", host, Port), HTTP: &http.Client{Timeout: 6 * time.Second}}
}

// Info is the answer of GET /info.
type Info struct {
	DeviceID string `xml:"deviceID,attr"`
	Name     string `xml:"name"`
	Type     string `xml:"type"`
	Network  []struct {
		Type string `xml:"type,attr"`
		IP   string `xml:"ipAddress"`
	} `xml:"networkInfo"`
	Components []struct {
		Category string `xml:"componentCategory"`
		Version  string `xml:"softwareVersion"`
	} `xml:"components>component"`
}

// Firmware returns the SCM software version (first field only).
func (i Info) Firmware() string {
	for _, c := range i.Components {
		if c.Category == "SCM" {
			return strings.Fields(c.Version + " ")[0]
		}
	}
	return ""
}

// ContentItem describes what a preset or the speaker plays.
type ContentItem struct {
	XMLName       xml.Name `xml:"ContentItem"`
	Source        string   `xml:"source,attr"`
	Type          string   `xml:"type,attr,omitempty"`
	Location      string   `xml:"location,attr,omitempty"`
	SourceAccount string   `xml:"sourceAccount,attr"`
	IsPresetable  bool     `xml:"isPresetable,attr"`
	ItemName      string   `xml:"itemName,omitempty"`
	ContainerArt  string   `xml:"containerArt,omitempty"`
}

// Preset is one stored preset slot (1–6).
type Preset struct {
	ID   int         `xml:"id,attr"`
	Item ContentItem `xml:"ContentItem"`
}

// Group is a Bose Stereo Link pair (GET /getGroup). Empty when not paired.
type Group struct {
	ID       string `xml:"id,attr"`
	Name     string `xml:"name"`
	MasterID string `xml:"masterDeviceId"`
	Roles    []struct {
		DeviceID string `xml:"deviceId"`
		Role     string `xml:"role"`
	} `xml:"roles>groupRole"`
	Status string `xml:"status"`
}

// Paired reports whether the speaker is part of a stereo pair.
func (g Group) Paired() bool { return g.MasterID != "" }

// Partner returns the deviceID of the other speaker in the pair.
func (g Group) Partner(self string) string {
	for _, r := range g.Roles {
		if r.DeviceID != self {
			return r.DeviceID
		}
	}
	return ""
}

// Info fetches /info.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.getXML(ctx, "/info", &i)
	if err == nil && i.DeviceID == "" {
		err = fmt.Errorf("%s/info: no deviceID, not a SoundTouch speaker", c.Base)
	}
	return i, err
}

// Presets fetches /presets. raw is the unmodified answer, for backups.
func (c *Client) Presets(ctx context.Context) (presets []Preset, raw []byte, err error) {
	raw, err = c.get(ctx, "/presets")
	if err != nil {
		return nil, nil, err
	}
	var p struct {
		Presets []Preset `xml:"preset"`
	}
	if err := xml.Unmarshal(raw, &p); err != nil {
		return nil, nil, fmt.Errorf("%s/presets: %w", c.Base, err)
	}
	return p.Presets, raw, nil
}

// Group fetches /getGroup.
func (c *Client) Group(ctx context.Context) (Group, error) {
	var g Group
	err := c.getXML(ctx, "/getGroup", &g)
	return g, err
}

// NowPlayingSource returns the source attribute of /now_playing, e.g. STANDBY or UPNP.
func (c *Client) NowPlayingSource(ctx context.Context) (string, error) {
	var n struct {
		Source string `xml:"source,attr"`
	}
	err := c.getXML(ctx, "/now_playing", &n)
	return n.Source, err
}

// StorePreset writes one preset slot.
func (c *Client) StorePreset(ctx context.Context, id int, item ContentItem) error {
	body, err := xml.Marshal(item)
	if err != nil {
		return err
	}
	return c.StoreRawPreset(ctx, id, body)
}

// StoreRawPreset writes one preset slot with a pre-serialised <ContentItem>.
func (c *Client) StoreRawPreset(ctx context.Context, id int, contentItemXML []byte) error {
	body := fmt.Sprintf(`<preset id="%d">%s</preset>`, id, contentItemXML)
	_, err := c.post(ctx, "/storePreset", body)
	return err
}

// RemovePreset clears one preset slot.
func (c *Client) RemovePreset(ctx context.Context, id int) error {
	_, err := c.post(ctx, "/removePreset", fmt.Sprintf(`<preset id="%d"></preset>`, id))
	return err
}

func (c *Client) getXML(ctx context.Context, path string, v any) error {
	b, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	if err := xml.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s%s: %w", c.Base, path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) post(ctx context.Context, path, body string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml")
	return c.do(req)
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || bytes.Contains(b, []byte("<errors")) {
		return b, fmt.Errorf("%s %s: %s %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}
