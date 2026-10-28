package main

import (
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webFiles embed.FS

// Version is set at build time with -ldflags "-X main.Version=…".
var Version = "0.4.0"

// maxJobBytes caps an upload: a dithered 300 mm photo is ~40 MB of G-code.
const maxJobBytes = 200 << 20

// API is the HTTP side: the web page, and the endpoints Kerf uses for job
// mode. Everything is JSON; errors are {"error": "…"} with a 4xx/5xx.
//
//	GET  /api/status          machine, controller, job
//	POST /api/job?name=…      body: G-code. Starts streaming from the bridge.
//	POST /api/job/pause|resume
//	POST /api/stop            hold, then reset — works whoever is in control
//	POST /api/command         {"line": "$H"} — only when nobody's in control
//	GET  /api/ports           serial ports the bridge can see
//	GET  /api/config, PUT /api/config
//	GET  /api/camera, /api/camera/frame, /api/camera/stream, POST /api/camera/focus (camera.go)
func NewAPI(m *Machine, cfg *ConfigStore, cam *Camera, ha *HomeAssistant) http.Handler {
	mux := http.NewServeMux()
	cameraRoutes(mux, cam)
	web, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /", http.FileServerFS(web))

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		snap := m.Snapshot()
		out := map[string]any{"bridge": map[string]any{"version": Version, "name": cfg.Get().Name}, "machine": snap}
		if ha != nil {
			out["homeAssistant"] = ha.Status()
		}
		writeJSON(w, 200, out)
	})

	mux.HandleFunc("POST /api/job", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJobBytes))
		if err != nil {
			writeError(w, 413, "that job is too large to upload")
			return
		}
		lines, err := prepareJob(string(body))
		if err != nil {
			writeError(w, 400, "that G-code can't be run: "+err.Error())
			return
		}
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			name = "Job"
		}
		job, err := m.StartJob(name, lines)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 201, job)
	})

	mux.HandleFunc("POST /api/job/pause", func(w http.ResponseWriter, r *http.Request) {
		m.Pause()
		writeJSON(w, 200, m.Snapshot())
	})
	mux.HandleFunc("POST /api/job/resume", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Resume(); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, m.Snapshot())
	})
	mux.HandleFunc("POST /api/stop", func(w http.ResponseWriter, r *http.Request) {
		m.Stop()
		writeJSON(w, 200, m.Snapshot())
	})

	mux.HandleFunc("POST /api/command", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Line string `json:"line"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			writeError(w, 400, "send {\"line\": \"…\"}")
			return
		}
		replies, err := m.Command(req.Line)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"replies": replies})
	})

	mux.HandleFunc("GET /api/ports", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, ListPorts())
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, publicConfig(cfg.Get()))
	})
	mux.HandleFunc("PUT /api/config", func(w http.ResponseWriter, r *http.Request) {
		var next struct {
			Config
			// The password only changes when one is typed, or on request.
			MQTTPasswordClear bool `json:"mqttPasswordClear"`
		}
		next.Config = cfg.Get()
		next.MQTTPassword = ""
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&next); err != nil {
			writeError(w, 400, "that isn't a valid config")
			return
		}
		if err := cfg.Update(func(c *Config) {
			c.SerialPort, c.Baud, c.Name = next.SerialPort, next.Baud, next.Name
			c.MQTTBroker, c.MQTTUsername = strings.TrimSpace(next.MQTTBroker), strings.TrimSpace(next.MQTTUsername)
			c.MQTTPrefix, c.MQTTCamera = strings.TrimSpace(next.MQTTPrefix), next.MQTTCamera
			if next.MQTTPassword != "" {
				c.MQTTPassword = next.MQTTPassword
			} else if next.MQTTPasswordClear {
				c.MQTTPassword = ""
			}
		}); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		// Port and baud apply the next time the laser is (re)connected;
		// the page says so rather than dropping a running job. Home
		// Assistant reconnects within a couple of seconds.
		writeJSON(w, 200, publicConfig(cfg.Get()))
	})
	return mux
}

// publicConfig is the config as the API shows it: the MQTT password left
// out, with whether one is set.
func publicConfig(c Config) any {
	set := c.MQTTPassword != ""
	c.MQTTPassword = ""
	return struct {
		Config
		MQTTPasswordSet bool `json:"mqttPasswordSet"`
	}{c, set}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
