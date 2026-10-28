package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"
)

// First-run set-up: the web page's wizard picks the laser (testing a port
// first), the camera (with its live picture) and a name, then shows how to
// connect Kerf and LightBurn. It opens by itself until it's been finished
// once (setupDone), and again from Settings.
//
//	GET  /api/setup             ports, cameras and the current choices
//	POST /api/setup/test-port   {"port": "/dev/ttyUSB0"} — is a GRBL laser on it?
//	POST /api/setup             any of serialPort, baud, cameraDevice, name, done

// probeMu makes the search for the laser and the wizard's port tests take
// turns: two opens of one port at once fail, or reset the board twice.
var probeMu sync.Mutex

// PortTest is what testing a port found.
type PortTest struct {
	OK     bool   `json:"ok"`
	Banner string `json:"banner,omitempty"`
	Baud   int    `json:"baud,omitempty"`
	// The bridge is already using the laser on this port.
	InUse   bool   `json:"inUse,omitempty"`
	Message string `json:"message"`
}

// TestPort opens a port and waits for GRBL's greeting, then closes it.
// Opening a serial port restarts most boards on it, so the wizard says so
// before anyone presses Test. Only ports the bridge lists can be tested.
func TestPort(m *Machine, cfg Config, name string, baud int) (PortTest, error) {
	if !portListed(name, cfg) {
		return PortTest{}, errors.New("that isn't one of the serial ports the bridge can see")
	}
	if s := m.Snapshot(); s.Connected && s.Port == name {
		return PortTest{OK: true, InUse: true, Banner: s.Banner, Baud: s.Baud,
			Message: "This is the laser the bridge is connected to."}, nil
	}
	bauds := []int{baud}
	if baud == 0 {
		bauds = baudRates
	}
	probeMu.Lock()
	defer probeMu.Unlock()
	opened := false
	var openErr error
	for _, b := range bauds {
		port, banner, err := openGRBL(name, b)
		if err == nil {
			port.Close()
			return PortTest{OK: true, Banner: banner, Baud: b, Message: "A GRBL laser answered."}, nil
		}
		if errors.Is(err, errNoBanner) {
			opened = true
			continue
		}
		// Can't be opened at all (in use, no permission): no other speed
		// will help. A speed the port refuses after others opened is just
		// skipped.
		if !opened {
			openErr = err
			break
		}
	}
	if openErr != nil {
		return PortTest{Message: "The port couldn't be opened: " + openErr.Error()}, nil
	}
	return PortTest{Message: "Nothing answered as a GRBL laser. Check the laser is switched on, or try another port."}, nil
}

func portListed(name string, cfg Config) bool {
	if name == "" {
		return false
	}
	if name == cfg.SerialPort {
		return true
	}
	for _, p := range ListPorts() {
		if p.Name == name {
			return true
		}
	}
	return false
}

func cameraListed(dev string) bool {
	cams, _ := listCameras()
	for _, c := range cams {
		if c.Device == dev {
			return true
		}
	}
	return false
}

// setupRoutes adds the wizard's endpoints.
func setupRoutes(mux *http.ServeMux, m *Machine, cfg *ConfigStore, cam *Camera) {
	mux.HandleFunc("GET /api/setup", func(w http.ResponseWriter, r *http.Request) {
		c := cfg.Get()
		snap := m.Snapshot()
		_, addrs := lanAddresses()
		type port struct {
			PortInfo
			Connected bool `json:"connected"`
		}
		ports := []port{}
		listed := false
		for _, p := range ListPorts() {
			ports = append(ports, port{p, snap.Connected && snap.Port == p.Name})
			listed = listed || p.Name == c.SerialPort
		}
		// A port set by hand (or not plugged in right now) still shows.
		if c.SerialPort != "" && !listed {
			ports = append(ports, port{PortInfo{Name: c.SerialPort}, snap.Connected && snap.Port == c.SerialPort})
		}
		type camera struct {
			Name   string `json:"name"`
			Device string `json:"device"`
		}
		cams := []camera{}
		found, camErr := listCameras()
		for _, f := range found {
			cams = append(cams, camera{f.Name, f.Device})
		}
		out := map[string]any{
			"ports":        ports,
			"cameras":      cams,
			"serialPort":   c.SerialPort,
			"baud":         c.Baud,
			"cameraDevice": c.CameraDevice,
			"name":         c.Name,
			"rawAddr":      c.RawAddr,
			"httpAddr":     c.HTTPAddr,
			"setupDone":    c.SetupDone,
			// How other computers reach this one, for LightBurn.
			"host":      announceHost() + ".local",
			"addresses": addrs,
			"laser":     map[string]any{"connected": snap.Connected, "port": snap.Port, "baud": snap.Baud, "banner": snap.Banner},
		}
		if camErr != nil && len(found) == 0 {
			out["cameraError"] = camErr.Error()
		}
		writeJSON(w, 200, out)
	})

	mux.HandleFunc("POST /api/setup/test-port", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Port string `json:"port"`
			Baud int    `json:"baud"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, 400, `send {"port": "…"}`)
			return
		}
		res, err := TestPort(m, cfg.Get(), req.Port, req.Baud)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, res)
	})

	mux.HandleFunc("POST /api/setup", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SerialPort   *string `json:"serialPort"`
			Baud         *int    `json:"baud"`
			CameraDevice *string `json:"cameraDevice"`
			Name         *string `json:"name"`
			Done         *bool   `json:"done"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, 400, "that isn't a valid set-up")
			return
		}
		c := cfg.Get()
		if req.SerialPort != nil && *req.SerialPort != "" && !portListed(*req.SerialPort, c) {
			writeError(w, 400, "that isn't one of the serial ports the bridge can see")
			return
		}
		if req.Baud != nil && *req.Baud != 0 && !knownBaud(*req.Baud) {
			writeError(w, 400, "that baud rate isn't one the bridge uses")
			return
		}
		if req.CameraDevice != nil {
			if d := *req.CameraDevice; d != "" && d != "off" && d != c.CameraDevice && !cameraListed(d) {
				writeError(w, 400, "that isn't one of the cameras the bridge can see")
				return
			}
		}
		if req.Name != nil {
			n := strings.TrimSpace(*req.Name)
			if n == "" || utf8.RuneCountInString(n) > 60 {
				writeError(w, 400, "give the bridge a name of up to 60 characters")
				return
			}
			*req.Name = n
		}
		if err := cfg.Update(func(c *Config) {
			if req.SerialPort != nil {
				c.SerialPort = *req.SerialPort
			}
			if req.Baud != nil {
				c.Baud = *req.Baud
			}
			if req.CameraDevice != nil {
				c.CameraDevice = *req.CameraDevice
			}
			if req.Name != nil {
				c.Name = *req.Name
			}
			if req.Done != nil {
				c.SetupDone = *req.Done
			}
		}); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		out := map[string]any{"ok": true}
		// A different laser port takes effect now if the laser is free;
		// connected to the wrong one, it lets go and the search picks up
		// the new setting.
		if req.SerialPort != nil && *req.SerialPort != "" {
			if s := m.Snapshot(); s.Connected && s.Port != *req.SerialPort {
				if err := m.Disconnect(); err != nil {
					out["note"] = "Saved. The bridge switches to " + *req.SerialPort + " when the laser is free: " + err.Error()
				}
			}
		}
		if req.CameraDevice != nil {
			cam.Restart()
		}
		writeJSON(w, 200, out)
	})
}

func knownBaud(b int) bool {
	for _, k := range baudRates {
		if k == b {
			return true
		}
	}
	return false
}
