package events

import (
	"testing"
	"time"
)

// Messages recorded from a SoundTouch 10 (firmware 27.0.6) in M0, IDs anonymised.
const (
	msgHello      = `<SoundTouchSdkInfo serverVersion="4" serverBuild="trunk r46330 v4 epdbuild hepdswbld04" />`
	msgPresetTune = `<updates deviceID="AABBCCDDEEFF"><nowSelectionUpdated><preset id="1"><ContentItem source="TUNEIN" type="stationurl" location="/v1/playback/station/s142689" sourceAccount="" isPresetable="true"><itemName>Radio Regenbogen - Soft &amp; Lazy</itemName><containerArt>http://cdn-profiles.tunein.com/s142689/images/logog.jpg?t=637800324690000000</containerArt></ContentItem></preset></nowSelectionUpdated></updates>`
	msgPresetUpnp = `<updates deviceID="AABBCCDDEEFF"><nowSelectionUpdated><preset id="6"><ContentItem source="UPNP" location="http://192.0.2.10:8686/stream/AABBCCDDEEFF/6" sourceAccount="UPnPUserName" isPresetable="true"><itemName>Chilltrax</itemName></ContentItem></preset></nowSelectionUpdated></updates>`
	msgPower      = `<updates deviceID="AABBCCDDEEFF"><nowSelectionUpdated><preset id="0"><ContentItem source="INVALID_SOURCE" type="DO_NOT_RESUME" location="unplayable location" sourceAccount="UPnPUserName" isPresetable="false" /></preset></nowSelectionUpdated></updates>`
	msgActivity   = `<userActivityUpdate deviceID="AABBCCDDEEFF" />`
	msgStandby    = `<updates deviceID="AABBCCDDEEFF"><nowPlayingUpdated><nowPlaying deviceID="AABBCCDDEEFF" source="STANDBY"><ContentItem source="STANDBY" isPresetable="false" /></nowPlaying></nowPlayingUpdated></updates>`
	msgError      = `<errorUpdate deviceID="AABBCCDDEEFF"><error value="1036" name="UNABLE_TO_PROCESS_NOT_LOGGED_IN" severity="Unrecoverable">UpnpRcvdContentItemInWrongState</error></errorUpdate>`
	msgVolume     = `<updates deviceID="AABBCCDDEEFF"><volumeUpdated><volume><targetvolume>17</targetvolume><actualvolume>17</actualvolume><muteenabled>false</muteenabled></volume></volumeUpdated></updates>`
	msgGroupErr   = `<updates deviceID='AABBCCDDEEFF'><groupUpdated><group id="1"><name>SoundTouch Li… + SoundTouch Re…</name><masterDeviceId>AABBCCDDEEFF</masterDeviceId><roles><groupRole><deviceId>AABBCCDDEEFF</deviceId><role>LEFT</role><ipAddress>192.168.1.34</ipAddress></groupRole><groupRole><deviceId>112233445566</deviceId><role>RIGHT</role><ipAddress>192.168.1.35</ipAddress></groupRole></roles><senderIPAddress>192.168.1.30</senderIPAddress><status>GROUP_ERROR</status></group></groupUpdated></updates>`
	msgPresets    = `<updates deviceID="AABBCCDDEEFF"><presetsUpdated/></updates>`
)

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		kind Kind
		chk  func(t *testing.T, e Event)
	}{
		{"hello", msgHello, KindHello, nil},
		{"preset tunein", msgPresetTune, KindPreset, func(t *testing.T, e Event) {
			if e.Preset != 1 || e.Item.Source != "TUNEIN" || e.Item.ItemName != "Radio Regenbogen - Soft & Lazy" || e.DeviceID != "AABBCCDDEEFF" {
				t.Errorf("%+v %+v", e, e.Item)
			}
		}},
		{"preset upnp", msgPresetUpnp, KindPreset, func(t *testing.T, e Event) {
			if e.Preset != 6 || e.Item.Location != "http://192.0.2.10:8686/stream/AABBCCDDEEFF/6" {
				t.Errorf("%+v %+v", e, e.Item)
			}
		}},
		{"power is not a preset", msgPower, KindOther, nil},
		{"activity", msgActivity, KindOther, nil},
		{"standby", msgStandby, KindNowPlaying, func(t *testing.T, e Event) {
			if e.Source != "STANDBY" {
				t.Errorf("source %q", e.Source)
			}
		}},
		{"error", msgError, KindError, func(t *testing.T, e Event) {
			if e.Error != "UNABLE_TO_PROCESS_NOT_LOGGED_IN: UpnpRcvdContentItemInWrongState" {
				t.Errorf("error %q", e.Error)
			}
		}},
		{"volume", msgVolume, KindOther, nil},
		{"group single quotes", msgGroupErr, KindGroup, func(t *testing.T, e Event) {
			if e.Group.Status != "GROUP_ERROR" || e.Group.MasterID != "AABBCCDDEEFF" || e.Group.Partner("AABBCCDDEEFF") != "112233445566" {
				t.Errorf("%+v", e.Group)
			}
		}},
		{"presets updated", msgPresets, KindPresetsUpdated, nil},
		{"garbage", `<updates><nowSelectionUpdated><preset id="x"`, KindOther, nil},
		{"empty", ``, KindOther, nil},
		{"not xml", `hello`, KindOther, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Parse([]byte(tc.msg))
			if e.Kind != tc.kind {
				t.Fatalf("kind = %v, want %v", e.Kind, tc.kind)
			}
			if tc.chk != nil {
				tc.chk(t, e)
			}
		})
	}
}

func TestDebouncer(t *testing.T) {
	d := Debouncer{Window: 1500 * time.Millisecond}
	t0 := time.Now()
	steps := []struct {
		slot int
		at   time.Duration
		want bool
	}{
		{3, 0, true},
		{3, 500 * time.Millisecond, false}, // repeat within window
		{4, 600 * time.Millisecond, true},  // other button is independent
		{3, 1600 * time.Millisecond, true}, // window over
	}
	for _, s := range steps {
		if got := d.Allow(s.slot, t0.Add(s.at)); got != s.want {
			t.Errorf("slot %d at %v: got %v want %v", s.slot, s.at, got, s.want)
		}
	}
}
