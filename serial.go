package main

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// USB serial chips found on hobby laser controllers.
var knownChips = map[string]string{
	"1A86": "CH340",  // WCH — most cheap diode lasers
	"10C4": "CP210x", // Silicon Labs
	"0403": "FTDI",
	"2341": "Arduino",
	"2A03": "Arduino",
	"303A": "ESP32", // FluidNC on native USB
	"0483": "STM32",
}

var baudRates = []int{115200, 250000, 230400, 57600, 9600}

type PortInfo struct {
	Name    string `json:"name"`
	Chip    string `json:"chip,omitempty"`
	Product string `json:"product,omitempty"`
}

// ListPorts returns serial ports, likely laser controllers first.
func ListPorts() []PortInfo {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		names, _ := serial.GetPortsList()
		out := make([]PortInfo, 0, len(names))
		for _, n := range names {
			out = append(out, PortInfo{Name: n})
		}
		return out
	}
	likely, other := []PortInfo{}, []PortInfo{}
	for _, d := range details {
		p := PortInfo{Name: d.Name, Product: d.Product}
		if d.IsUSB {
			p.Chip = knownChips[strings.ToUpper(d.VID)]
		}
		if p.Chip != "" {
			likely = append(likely, p)
		} else if d.IsUSB {
			other = append(other, p)
		}
	}
	return append(likely, other...)
}

// openGRBL opens a port and waits for GRBL to say hello. Opening the port
// resets most boards, which then print their banner; if it doesn't come, a
// soft reset asks for it. Returns the open port and the banner.
func openGRBL(name string, baud int) (serial.Port, string, error) {
	port, err := serial.Open(name, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, "", err
	}
	if banner, ok := waitBanner(port, 2500*time.Millisecond); ok {
		return port, banner, nil
	}
	port.Write([]byte{rtReset})
	if banner, ok := waitBanner(port, 2000*time.Millisecond); ok {
		return port, banner, nil
	}
	port.Close()
	return nil, "", errNoBanner
}

// errNoBanner: the port opened but nothing answered as GRBL.
var errNoBanner = errors.New("no GRBL banner")

func waitBanner(port serial.Port, timeout time.Duration) (string, bool) {
	port.SetReadTimeout(100 * time.Millisecond)
	defer port.SetReadTimeout(serial.NoTimeout)
	deadline := time.Now().Add(timeout)
	r := bufio.NewReader(timeoutReader{port, deadline})
	for {
		line, err := r.ReadString('\n')
		if s := strings.TrimSpace(line); strings.HasPrefix(strings.ToLower(s), "grbl ") {
			return s, true
		}
		if err != nil {
			return "", false
		}
	}
}

// timeoutReader turns a port with a short read timeout into a reader that
// ends at a deadline.
type timeoutReader struct {
	port     serial.Port
	deadline time.Time
}

func (t timeoutReader) Read(p []byte) (int, error) {
	for time.Now().Before(t.deadline) {
		n, err := t.port.Read(p)
		if n > 0 || err != nil {
			return n, err
		}
	}
	return 0, io.EOF
}

// FindGRBL tries the configured port, or every likely port, at the
// configured baud rate or the common ones.
func FindGRBL(cfg Config) (serial.Port, string, int, string, error) {
	ports := []string{cfg.SerialPort}
	if cfg.SerialPort == "" {
		ports = nil
		for _, p := range ListPorts() {
			ports = append(ports, p.Name)
		}
	}
	bauds := []int{cfg.Baud}
	if cfg.Baud == 0 {
		bauds = baudRates
	}
	for _, name := range ports {
		for _, baud := range bauds {
			probeMu.Lock()
			port, banner, err := openGRBL(name, baud)
			probeMu.Unlock()
			if err == nil {
				return port, name, baud, banner, nil
			}
		}
	}
	if len(ports) == 0 {
		return nil, "", 0, "", errors.New("no USB serial ports found")
	}
	return nil, "", 0, "", errors.New("no GRBL controller answered on " + strings.Join(ports, ", "))
}

// RunSerial keeps the laser connected: find it, hold the port until it
// fails (unplugged, powered off), then look again.
func RunSerial(m *Machine, cfg func() Config, stop <-chan struct{}) {
	lastErr := ""
	for {
		select {
		case <-stop:
			return
		default:
		}
		port, name, baud, banner, err := FindGRBL(cfg())
		if err != nil {
			if err.Error() != lastErr {
				m.logf("waiting for the laser: %v", err)
				lastErr = err.Error()
			}
			select {
			case <-stop:
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		lastErr = ""
		m.Attach(port, name, baud, banner)
		port.Close()
	}
}
