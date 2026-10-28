// Kerf Bridge: sits between a GRBL laser's USB port and the network.
//
//   - Job mode: Kerf uploads a whole job over HTTP and the bridge streams it
//     to the laser itself, so the iPad can sleep or lose Wi-Fi mid-burn.
//   - Pass-through: raw GRBL on TCP (port 23 by default), the same as
//     ser2net, so LightBurn's "GRBL over TCP/IP" keeps working.
//   - A web page for status, jog, stop and settings.
//   - Advertised over Bonjour as _kerf._tcp so Kerf finds it.
//
// One controller at a time: whoever starts sending first owns the laser
// until they finish; everyone else can watch and can always stop it.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", defaultConfigPath(), "settings file")
	snapshot := flag.String("snapshot", "", "save one camera picture to this file and exit (doesn't touch the laser)")
	flag.Parse()

	if *snapshot != "" {
		os.Exit(saveSnapshot(*snapshot, *configPath))
	}
	app := inAppBundle()
	if app {
		appLog()
	}

	store, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("reading %s: %v", *configPath, err)
	}
	cfg := store.Get()
	log.Printf("Kerf Bridge %s, settings in %s", Version, *configPath)

	m := NewMachine()
	stop := make(chan struct{})
	go RunSerial(m, store.Get, stop)

	if cfg.RawAddr != "" {
		ln, err := net.Listen("tcp", cfg.RawAddr)
		if err != nil {
			// Port 23 needs privileges on Linux; the service file grants them.
			log.Printf("raw TCP on %s is off: %v", cfg.RawAddr, err)
		} else {
			log.Printf("raw GRBL (LightBurn, Kerf direct mode) on %s", cfg.RawAddr)
			go ServeRaw(m, ln)
		}
	}

	httpLn, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		log.Fatalf("web page on %s: %v", cfg.HTTPAddr, err)
	}
	log.Printf("web page and job API on %s", cfg.HTTPAddr)
	cam := NewCamera(store.Get)
	cam.save = store.Update
	ha := StartHomeAssistant(m, cam, store.Get, stop)
	server := &http.Server{Handler: NewAPI(m, store, cam, ha), ReadHeaderTimeout: 10 * time.Second}
	go server.Serve(httpLn)

	httpPort := httpLn.Addr().(*net.TCPAddr).Port
	var mdnsMu sync.Mutex
	mdns := StartAnnouncer(cfg, httpPort)
	// A new name (from the set-up wizard or Settings) is announced at once,
	// so Kerf lists the bridge by it without a restart.
	store.OnChange(func(old, now Config) {
		if old.Name == now.Name {
			return
		}
		mdnsMu.Lock()
		defer mdnsMu.Unlock()
		mdns.Stop()
		mdns = StartAnnouncer(now, httpPort)
	})

	shutdown := func() {
		log.Printf("stopping")
		// A job the bridge is streaming would otherwise stop dead mid-line
		// when the process ends. Hold and reset instead, so it stops cleanly.
		if j := m.Snapshot().Job; j != nil && j.State.active() {
			log.Printf("stopping the running job first")
			m.Stop()
		}
		close(stop)
		mdnsMu.Lock()
		mdns.Stop()
		mdnsMu.Unlock()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	if app {
		// The Mac app: the menu bar runs on this thread; a signal (logging
		// out, say) quits the same way as Quit in the menu.
		go func() { <-sig; goQuit() }()
		runApp(m, store, httpPort, shutdown)
		return
	}
	<-sig
	shutdown()
}

func defaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "kerf-bridge", "config.json")
	}
	return "kerf-bridge.json"
}

// saveSnapshot is for checking the camera: it takes one picture with the
// camera settings from the config file (or the defaults) and saves it.
func saveSnapshot(path, configPath string) int {
	cfg := defaultConfig()
	if data, err := os.ReadFile(configPath); err == nil {
		json.Unmarshal(data, &cfg)
	}
	cam := NewCamera(func() Config { return cfg })
	jpeg, _, err := cam.Frame(0, 8*time.Second)
	if err != nil {
		log.Printf("no picture: %v", err)
		return 1
	}
	info := cam.Info()
	if err := os.WriteFile(path, jpeg, 0o644); err != nil {
		log.Printf("saving %s: %v", path, err)
		return 1
	}
	log.Printf("saved %s: %s, %d × %d, %d bytes", path, info.Name, info.Width, info.Height, len(jpeg))
	return 0
}
