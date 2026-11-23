// Package events receives and interprets the notifications a SoundTouch
// speaker pushes over its WebSocket (port 8080, subprotocol "gabbo").
package events

import (
	"bytes"
	"encoding/xml"
	"sync"
	"time"

	"github.com/AdrianJetzer/soundtouch-radio-bridge/internal/speaker"
)

// Kind classifies a notification.
type Kind int

const (
	KindOther          Kind = iota
	KindHello               // <SoundTouchSdkInfo>, sent on connect
	KindPreset              // a preset button (1–6) was pressed
	KindPresetsUpdated      // stored presets changed
	KindNowPlaying          // playback state changed
	KindGroup               // stereo pair status changed
	KindError               // <errorUpdate>
)

// Event is one interpreted notification.
type Event struct {
	Kind     Kind
	DeviceID string
	Preset   int                  // KindPreset: 1–6
	Item     *speaker.ContentItem // KindPreset: what the button holds
	Source   string               // KindNowPlaying: e.g. STANDBY, UPNP
	Group    *speaker.Group       // KindGroup
	Error    string               // KindError: name and text
}

type updates struct {
	DeviceID     string `xml:"deviceID,attr"`
	NowSelection *struct {
		Preset struct {
			ID   int                 `xml:"id,attr"`
			Item speaker.ContentItem `xml:"ContentItem"`
		} `xml:"preset"`
	} `xml:"nowSelectionUpdated"`
	NowPlaying *struct {
		NowPlaying struct {
			Source string `xml:"source,attr"`
		} `xml:"nowPlaying"`
	} `xml:"nowPlayingUpdated"`
	PresetsUpdated *struct{}      `xml:"presetsUpdated"`
	Group          *speaker.Group `xml:"groupUpdated>group"`
}

type errorUpdate struct {
	DeviceID string `xml:"deviceID,attr"`
	Error    struct {
		Name string `xml:"name,attr"`
		Text string `xml:",chardata"`
	} `xml:"error"`
}

// Parse interprets one WebSocket message. Unknown or malformed messages
// yield a single KindOther event; Parse never panics on bad input.
func Parse(msg []byte) Event {
	root := rootName(msg)
	switch root {
	case "SoundTouchSdkInfo":
		return Event{Kind: KindHello}
	case "errorUpdate":
		var e errorUpdate
		if xml.Unmarshal(msg, &e) == nil {
			return Event{Kind: KindError, DeviceID: e.DeviceID, Error: e.Error.Name + ": " + e.Error.Text}
		}
	case "updates":
		var u updates
		if xml.Unmarshal(msg, &u) != nil {
			break
		}
		switch {
		case u.NowSelection != nil:
			p := u.NowSelection.Preset
			if p.ID < 1 || p.ID > 6 { // id 0 = power button
				return Event{Kind: KindOther, DeviceID: u.DeviceID}
			}
			item := p.Item
			return Event{Kind: KindPreset, DeviceID: u.DeviceID, Preset: p.ID, Item: &item}
		case u.NowPlaying != nil:
			return Event{Kind: KindNowPlaying, DeviceID: u.DeviceID, Source: u.NowPlaying.NowPlaying.Source}
		case u.PresetsUpdated != nil:
			return Event{Kind: KindPresetsUpdated, DeviceID: u.DeviceID}
		case u.Group != nil:
			return Event{Kind: KindGroup, DeviceID: u.DeviceID, Group: u.Group}
		}
		return Event{Kind: KindOther, DeviceID: u.DeviceID}
	}
	return Event{Kind: KindOther}
}

func rootName(msg []byte) string {
	d := xml.NewDecoder(bytes.NewReader(msg))
	for {
		tok, err := d.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local
		}
	}
}

// Debouncer drops a repeated press of the same button within Window.
type Debouncer struct {
	Window time.Duration
	mu     sync.Mutex
	last   map[int]time.Time
}

// Allow reports whether a press of slot at now should be handled.
func (d *Debouncer) Allow(slot int, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		d.last = map[int]time.Time{}
	}
	if t, ok := d.last[slot]; ok && now.Sub(t) < d.Window {
		return false
	}
	d.last[slot] = now
	return true
}
