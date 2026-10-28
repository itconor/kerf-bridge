//go:build darwin && cgo

package main

/*
#cgo CFLAGS: -fobjc-arc -mmacosx-version-min=13.0
#cgo LDFLAGS: -framework Cocoa -framework ServiceManagement -mmacosx-version-min=13.0
#include <stdlib.h>

void runMenuBar(const char *url, const char *version);
void setStatusLine(const char *text);
void openPageNow(void);
*/
import "C"

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

// On a Mac the bridge ships as Kerf Bridge.app: a menu bar icon with the
// laser's state, Open Kerf Bridge (its web page), Start at Login and Quit.
// Run from Terminal (the bare program), it behaves as everywhere else.

func init() {
	// Cocoa must run on the main thread, which is the one main() starts on.
	runtime.LockOSThread()
}

var quitApp = make(chan struct{}, 1)

//export goQuit
func goQuit() {
	select {
	case quitApp <- struct{}{}:
	default:
	}
}

// inAppBundle: started from Kerf Bridge.app rather than Terminal.
func inAppBundle() bool {
	exe, err := os.Executable()
	return err == nil && strings.Contains(exe, ".app/Contents/MacOS/")
}

// appLog sends the log to ~/Library/Logs/Kerf Bridge.log, since an app has
// no terminal to print to.
func appLog() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, "Library", "Logs")
	os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "Kerf Bridge.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		log.SetOutput(f)
	}
}

// runApp shows the menu bar icon until Quit, keeping its status line up
// to date, and opens the web page on the first run so the set-up wizard
// is there. It never returns: shutdown runs, then the process exits.
func runApp(m *Machine, store *ConfigStore, httpPort int, shutdown func()) {
	url := "http://localhost:" + strconv.Itoa(httpPort) + "/"
	go func() {
		last := ""
		for {
			s := m.Snapshot()
			line := "Looking for the laser…"
			if s.Connected {
				line = "Laser connected"
				if s.Status != nil && s.Status.State != "" {
					line = "Laser: " + s.Status.State
				}
				if j := s.Job; j != nil && j.State.active() && j.Total > 0 {
					line += " — " + j.Name + " " + strconv.Itoa(j.Done*100/j.Total) + " %"
				}
			}
			if line != last {
				cs := C.CString(line)
				C.setStatusLine(cs)
				C.free(unsafe.Pointer(cs))
				last = line
			}
			time.Sleep(time.Second)
		}
	}()
	if !store.Get().SetupDone {
		go func() {
			time.Sleep(time.Second)
			C.openPageNow()
		}()
	}
	go func() {
		<-quitApp
		shutdown()
		os.Exit(0)
	}()
	cu, cv := C.CString(url), C.CString(Version)
	C.runMenuBar(cu, cv)
}
