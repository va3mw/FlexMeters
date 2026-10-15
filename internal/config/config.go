// Package config stores FlexMeters settings as JSON in the user config dir.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Range overrides the display range of one meter.
type Range struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

type Config struct {
	RadioSerial string           `json:"radio_serial"`
	RadioIP     string           `json:"radio_ip"`
	Station     string           `json:"station"`
	Selected    []string         `json:"selected"` // meter keys, in display order
	PickerOpen  bool             `json:"picker_open"`
	Ranges      map[string]Range `json:"ranges"`
}

type Store struct {
	mu  sync.Mutex
	cfg Config
}

// Dir is %AppData%\FlexMeters on Windows, ~/Library/Application Support/FlexMeters on macOS.
func Dir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "FlexMeters")
}

func path() string { return filepath.Join(Dir(), "settings.json") }

func Load() *Store {
	s := &Store{cfg: Config{PickerOpen: true}}
	if b, err := os.ReadFile(path()); err == nil {
		json.Unmarshal(b, &s.cfg)
	}
	if s.cfg.Ranges == nil {
		s.cfg.Ranges = map[string]Range{}
	}
	return s
}

func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	c.Selected = append([]string(nil), s.cfg.Selected...)
	c.Ranges = make(map[string]Range, len(s.cfg.Ranges))
	for k, v := range s.cfg.Ranges {
		c.Ranges[k] = v
	}
	return c
}

// Update applies f and writes the file (via a temp file, so a crash
// mid-write cannot leave it truncated).
func (s *Store) Update(f func(c *Config)) error {
	s.mu.Lock()
	f(&s.cfg)
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	tmp := path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path())
}
