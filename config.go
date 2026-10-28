package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Config struct {
	// Serial port, e.g. /dev/ttyUSB0 or COM3. Empty: find the laser.
	SerialPort string `json:"serialPort"`
	// 0: try the usual rates.
	Baud int `json:"baud"`
	// Web page and Kerf's job API.
	HTTPAddr string `json:"httpAddr"`
	// Raw GRBL for LightBurn ("GRBL over TCP/IP") and Kerf's direct mode,
	// the same as ser2net. Empty turns it off.
	RawAddr string `json:"rawAddr"`
	// Name shown to Kerf when it finds the bridge on the network.
	Name string `json:"name"`
	// Camera over the bed: "" finds the first USB camera, "off" turns it
	// off, or a device such as /dev/video0.
	CameraDevice string `json:"cameraDevice"`
	// Picture size asked of the camera; 0 means 1600 × 1200.
	CameraWidth  int `json:"cameraWidth"`
	CameraHeight int `json:"cameraHeight"`
	// Focus: -1 leaves autofocus on; 0 or more fixes it there, so the
	// picture's scale stays put for lining work up.
	CameraFocus int `json:"cameraFocus"`
	// Home Assistant over MQTT: the broker ("" turns it off), e.g. the
	// Mosquitto add-on at 192.168.0.176 (port 1883 unless given).
	MQTTBroker   string `json:"mqttBroker"`
	MQTTUsername string `json:"mqttUsername"`
	// Kept here, never sent back out by the API.
	MQTTPassword string `json:"mqttPassword"`
	// Home Assistant's discovery prefix; "" means "homeassistant".
	MQTTPrefix string `json:"mqttPrefix"`
	// A picture of the bed every few seconds while a job runs.
	MQTTCamera bool `json:"mqttCamera"`
	// The set-up wizard has been finished once; until then the web page
	// opens with it.
	SetupDone bool `json:"setupDone"`
}

func defaultConfig() Config {
	host, _ := os.Hostname()
	return Config{HTTPAddr: ":8023", RawAddr: ":23", Name: "Kerf Bridge on " + host, CameraFocus: -1,
		MQTTCamera: true}
}

type ConfigStore struct {
	mu       sync.Mutex
	path     string
	cfg      Config
	onChange []func(old, new Config)
}

// OnChange calls f after every saved change, with the settings before and after.
func (s *ConfigStore) OnChange(f func(old, new Config)) {
	s.mu.Lock()
	s.onChange = append(s.onChange, f)
	s.mu.Unlock()
}

// LoadConfig reads the file, creating it with defaults on first run.
func LoadConfig(path string) (*ConfigStore, error) {
	s := &ConfigStore{path: path, cfg: defaultConfig()}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, s.save()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.cfg); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ConfigStore) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *ConfigStore) Update(f func(*Config)) error {
	s.mu.Lock()
	old := s.cfg
	f(&s.cfg)
	now, watchers := s.cfg, s.onChange
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return err
	}
	for _, w := range watchers {
		w(old, now)
	}
	return nil
}

// save writes atomically: a half-written config must never be what the
// next start reads.
func (s *ConfigStore) save() error {
	s.mu.Lock()
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".config-*.json")
	if err != nil {
		return err
	}
	// CreateTemp makes it owner-only; keep it readable like any config.
	tmp.Chmod(0o644)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), s.path)
}
