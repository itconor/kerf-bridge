package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setupServer(t *testing.T, m *Machine, c Config) (*httptest.Server, *ConfigStore) {
	t.Helper()
	store, err := LoadConfig(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	store.Update(func(cfg *Config) {
		cfg.SerialPort, cfg.CameraDevice, cfg.Name = c.SerialPort, c.CameraDevice, c.Name
	})
	srv := httptest.NewServer(NewAPI(m, store, NewCamera(store.Get), nil))
	t.Cleanup(srv.Close)
	return srv, store
}

func postJSON(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	r, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func TestSetupSavesWhatItIsGiven(t *testing.T) {
	srv, store := setupServer(t, NewMachine(), Config{Name: "Old name", CameraDevice: ""})

	if store.Get().SetupDone {
		t.Fatal("a new bridge starts with set-up done")
	}
	code, out := postJSON(t, srv.URL+"/api/setup", `{"name": "  Workshop laser  ", "cameraDevice": "off"}`)
	if code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	c := store.Get()
	if c.Name != "Workshop laser" || c.CameraDevice != "off" || c.SetupDone {
		t.Fatalf("after saving the name and camera: %+v", c)
	}
	if code, _ := postJSON(t, srv.URL+"/api/setup", `{"done": true}`); code != 200 || !store.Get().SetupDone {
		t.Fatal("finishing didn't mark set-up done")
	}
	// Fields not sent are left alone.
	if c := store.Get(); c.Name != "Workshop laser" || c.CameraDevice != "off" {
		t.Fatalf("finishing changed other settings: %+v", c)
	}

	r, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Bridge struct {
			SetupDone bool `json:"setupDone"`
		} `json:"bridge"`
	}
	json.NewDecoder(r.Body).Decode(&status)
	r.Body.Close()
	if !status.Bridge.SetupDone {
		t.Fatal("/api/status doesn't say set-up is done")
	}
}

func TestSetupRefusesWhatItCantSee(t *testing.T) {
	srv, store := setupServer(t, NewMachine(), Config{Name: "Bridge"})
	for _, body := range []string{
		`{"serialPort": "/etc/passwd"}`,               // not a serial port the bridge lists
		`{"cameraDevice": "/dev/sda"}`,                // not a camera it found
		`{"name": "   "}`,                             // no name
		`{"name": "` + strings.Repeat("x", 61) + `"}`, // too long
		`{"baud": 12345}`,
	} {
		if code, _ := postJSON(t, srv.URL+"/api/setup", body); code != 400 {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	if c := store.Get(); c.SerialPort != "" || c.CameraDevice != "" || c.Name != "Bridge" || c.Baud != 0 {
		t.Fatalf("a refused set-up changed the settings: %+v", c)
	}
	if code, _ := postJSON(t, srv.URL+"/api/setup/test-port", `{"port": "/etc/passwd"}`); code != 400 {
		t.Fatalf("testing a file that isn't a serial port: %d, want 400", code)
	}
}

func TestSetupTestOfTheConnectedPort(t *testing.T) {
	m, f := setup(t)
	srv, _ := setupServer(t, m, Config{SerialPort: "fake", Name: "Bridge"})
	code, out := postJSON(t, srv.URL+"/api/setup/test-port", `{"port": "fake"}`)
	if code != 200 || out["ok"] != true || out["inUse"] != true || !strings.HasPrefix(out["banner"].(string), "Grbl") {
		t.Fatalf("testing the port in use: %d %v", code, out)
	}
	// The connected port is reported as answered, never reopened.
	if f.closed {
		t.Fatal("testing the connected port closed it")
	}
}

func TestDisconnectOnlyWhenFree(t *testing.T) {
	m, _ := setup(t)
	if _, err := m.StartJob("busy", jobLines(20000)); err != nil {
		t.Fatal(err)
	}
	if err := m.Disconnect(); err == nil {
		t.Fatal("let go of the laser in the middle of a job")
	}
	m.Stop()
	waitFor(t, func() bool { j := m.Snapshot().Job; return j != nil && !j.State.active() })
	waitFor(t, func() bool { return m.Snapshot().Controller == ControllerNone })
	if err := m.Disconnect(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !m.Snapshot().Connected })
}

func TestConfigChangeIsAnnounced(t *testing.T) {
	store, err := LoadConfig(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	store.OnChange(func(old, now Config) { got = append(got, old.Name+" → "+now.Name) })
	store.Update(func(c *Config) { c.Name = "A" })
	store.Update(func(c *Config) { c.Name = "B" })
	if len(got) != 2 || !strings.HasSuffix(got[0], "→ A") || got[1] != "A → B" {
		t.Fatalf("changes seen: %q", got)
	}
}

func TestSetupSwitchesToTheChosenPort(t *testing.T) {
	m, _ := setup(t)
	// The settings name another port than the one the laser is on (picked
	// in the wizard): saving it lets go of the wrong one.
	srv, store := setupServer(t, m, Config{SerialPort: "other", Name: "Bridge"})
	code, out := postJSON(t, srv.URL+"/api/setup", `{"serialPort": "other", "baud": 115200}`)
	if code != 200 || out["note"] != nil {
		t.Fatalf("save: %d %v", code, out)
	}
	waitFor(t, func() bool { return !m.Snapshot().Connected })
	if c := store.Get(); c.SerialPort != "other" || c.Baud != 115200 {
		t.Fatalf("saved: %+v", c)
	}
}
