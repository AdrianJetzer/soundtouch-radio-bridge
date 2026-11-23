// Package config loads, validates and watches the strb.yaml configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxPresets is the number of hardware preset buttons on a SoundTouch speaker.
const MaxPresets = 6

// Station is one internet radio stream.
type Station struct {
	Key  string `yaml:"-"`
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	Logo string `yaml:"logo"`
}

// Speaker pins a speaker by deviceID, optionally with a start address and own presets.
type Speaker struct {
	ID      string   `yaml:"id"`
	Host    string   `yaml:"host"`
	Presets []string `yaml:"presets"`
}

// Config is the parsed strb.yaml.
type Config struct {
	Listen    string             `yaml:"listen"`
	PublicURL string             `yaml:"public_url"`
	Stations  map[string]Station `yaml:"stations"`
	Presets   []string           `yaml:"presets"`
	Speakers  []Speaker          `yaml:"speakers"`

	// Path is the file the configuration was loaded from.
	Path string `yaml:"-"`
}

// DefaultListen is used when listen is empty.
const DefaultListen = ":8686"

// Candidates returns the paths searched when no --config is given.
func Candidates() []string {
	paths := []string{"strb.yaml"}
	if dir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "strb", "strb.yaml"))
	}
	return paths
}

// Find returns the first existing candidate path.
func Find() (string, error) {
	for _, p := range Candidates() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("keine Konfiguration gefunden (gesucht: %s); strb.example.yaml nach strb.yaml kopieren", strings.Join(Candidates(), ", "))
}

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	return c, nil
}

// Parse decodes and validates configuration data. Unknown fields are errors.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	for k, s := range c.Stations {
		s.Key = k
		s.URL = cleanURL(s.URL)
		s.Name = strings.TrimSpace(s.Name)
		if s.Name == "" {
			s.Name = k
		}
		c.Stations[k] = s
	}
	for i := range c.Speakers {
		c.Speakers[i].ID = strings.ToUpper(strings.TrimSpace(c.Speakers[i].ID))
		c.Speakers[i].Host = strings.TrimSpace(c.Speakers[i].Host)
	}
	c.PublicURL = strings.TrimRight(strings.TrimSpace(c.PublicURL), "/")
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	if len(c.Stations) == 0 {
		errs = append(errs, errors.New("stations: mindestens ein Sender nötig"))
	}
	for _, k := range sortedKeys(c.Stations) {
		s := c.Stations[k]
		u, err := url.Parse(s.URL)
		if s.URL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("stations.%s.url: %q ist keine gültige http(s)-Adresse", k, s.URL))
		}
	}
	errs = append(errs, c.checkPresets("presets", c.Presets)...)
	seen := map[string]bool{}
	for i, sp := range c.Speakers {
		field := fmt.Sprintf("speakers[%d]", i)
		if sp.ID == "" {
			errs = append(errs, fmt.Errorf("%s.id: fehlt (siehe `strb discover`)", field))
		} else if seen[sp.ID] {
			errs = append(errs, fmt.Errorf("%s.id: %s steht doppelt in der Liste", field, sp.ID))
		}
		seen[sp.ID] = true
		if sp.Presets != nil {
			errs = append(errs, c.checkPresets(field+".presets", sp.Presets)...)
		}
	}
	if c.PublicURL != "" {
		if u, err := url.Parse(c.PublicURL); err != nil || u.Scheme != "http" || u.Host == "" {
			errs = append(errs, fmt.Errorf("public_url: %q muss so aussehen: http://192.168.1.10:8686 (die Boxen können kein https)", c.PublicURL))
		}
	}
	return errors.Join(errs...)
}

func (c *Config) checkPresets(field string, keys []string) []error {
	var errs []error
	if len(keys) > MaxPresets {
		errs = append(errs, fmt.Errorf("%s: höchstens %d Einträge (einer pro Taste), sind aber %d", field, MaxPresets, len(keys)))
	}
	for i, k := range keys {
		if k == "" {
			continue // empty slot: button stays untouched
		}
		if _, ok := c.Stations[k]; !ok {
			errs = append(errs, fmt.Errorf("%s[%d]: unbekannter Sender %q (bekannt: %s)", field, i, k, strings.Join(sortedKeys(c.Stations), ", ")))
		}
	}
	return errs
}

// PresetsFor returns the stations for buttons 1–6 of a speaker. A nil entry
// means the button is not managed.
func (c *Config) PresetsFor(deviceID string) [MaxPresets]*Station {
	keys := c.Presets
	if sp := c.Speaker(deviceID); sp != nil && sp.Presets != nil {
		keys = sp.Presets
	}
	var out [MaxPresets]*Station
	for i, k := range keys {
		if i >= MaxPresets || k == "" {
			continue
		}
		if s, ok := c.Stations[k]; ok {
			out[i] = &s
		}
	}
	return out
}

// Speaker returns the pinned speaker entry for deviceID, or nil.
func (c *Config) Speaker(deviceID string) *Speaker {
	for i := range c.Speakers {
		if c.Speakers[i].ID == deviceID {
			return &c.Speakers[i]
		}
	}
	return nil
}

// Manages reports whether strb should control the speaker. With no
// speakers listed, every discovered speaker is managed.
func (c *Config) Manages(deviceID string) bool {
	return len(c.Speakers) == 0 || c.Speaker(deviceID) != nil
}

func cleanURL(s string) string {
	s = strings.Trim(strings.TrimSpace(s), "\"'`")
	if s != "" && !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return s
}

func sortedKeys(m map[string]Station) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
