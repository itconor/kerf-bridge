package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Controller is who may send G-code right now. Only one at a time: two
// senders interleaving lines on one laser is how things catch fire.
type Controller string

const (
	ControllerNone        Controller = "none"
	ControllerPassthrough Controller = "passthrough" // LightBurn or Kerf over raw TCP
	ControllerJob         Controller = "job"         // a job uploaded to the bridge
)

type JobState string

const (
	JobRunning       JobState = "running"
	JobPaused        JobState = "paused"
	JobPausedOnError JobState = "paused-on-error"
	JobCompleted     JobState = "completed"
	JobStopped       JobState = "stopped"
	JobAlarmed       JobState = "alarmed"
	JobInterrupted   JobState = "interrupted"
)

func (s JobState) active() bool {
	return s == JobRunning || s == JobPaused || s == JobPausedOnError
}

type Job struct {
	Name      string    `json:"name"`
	Total     int       `json:"total"`
	Sent      int       `json:"sent"`
	Done      int       `json:"done"`
	State     JobState  `json:"state"`
	Message   string    `json:"message,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitzero"`

	lines []string
}

// inFlight is one line sent but not yet answered. index is the job line,
// or -1 for a one-off command.
type inFlight struct {
	length int
	index  int
}

// Machine owns the serial port for as long as the bridge runs. Holding it
// open is the point: opening a serial port toggles DTR, which resets most
// GRBL boards, so a client reconnecting must never cause a reopen.
type Machine struct {
	mu         sync.Mutex
	port       io.ReadWriteCloser
	portName   string
	baud       int
	banner     string
	status     Status
	statusAt   time.Time
	wco        [3]float64
	controller Controller
	job        *Job
	// Character counting: GRBL's receive buffer is 128 bytes; keep the
	// bytes of unanswered lines within it.
	bufferSize int
	flight     []inFlight
	flightLen  int
	next       int // next job line to send
	// Raw output goes to pass-through clients as it arrives.
	sinks map[*rawClient]bool
	// Answers to one-off commands, for the API.
	replies chan string
	// `$n=value` as last reported — read at connect, and refreshed whenever
	// anyone (LightBurn included) asks for `$$`.
	settings map[int]string

	writeMu sync.Mutex
	logf    func(string, ...any)
}

func NewMachine() *Machine {
	return &Machine{
		controller: ControllerNone,
		bufferSize: 127,
		sinks:      map[*rawClient]bool{},
		replies:    make(chan string, 256),
		settings:   map[int]string{},
		logf:       log.Printf,
	}
}

// Attach starts using an open port. It reads until the port fails, then
// detaches and returns.
func (m *Machine) Attach(port io.ReadWriteCloser, name string, baud int, banner string) {
	m.mu.Lock()
	m.port, m.portName, m.baud, m.banner = port, name, baud, banner
	m.settings = map[int]string{}
	m.mu.Unlock()
	m.logf("laser on %s at %d baud: %s", name, baud, banner)

	stop := make(chan struct{})
	go m.poll(stop)
	// Read the machine's settings, as LightBurn does on connect: bed size
	// ($130/$131), full power ($30), laser mode ($32), speeds. GRBL only
	// answers `$$` when it's idle — a lid's safety switch holds it — so
	// keep trying until it does.
	go func() {
		time.Sleep(300 * time.Millisecond)
		for attempt := 0; ; attempt++ {
			if m.readSettings() {
				s := m.Snapshot().Machine
				mode := "unknown"
				if s.LaserMode != nil {
					mode = map[bool]string{true: "on", false: "OFF — the laser stays on during travel moves"}[*s.LaserMode]
				}
				m.logf("bed %g × %g mm, full power S%g, laser mode %s", s.BedWidth, s.BedHeight, s.MaxPower, mode)
				return
			}
			if attempt == 0 {
				m.mu.Lock()
				state := m.status.State
				m.mu.Unlock()
				m.logf("can't read the laser's settings yet (it's %q); trying again until it's free", state)
			}
			select {
			case <-stop:
				return
			case <-time.After(settingsRetry):
			}
		}
	}()
	err := m.read(port)
	close(stop)

	m.mu.Lock()
	m.port = nil
	m.status = Status{}
	m.resetQueueLocked()
	if m.job != nil && m.job.State.active() {
		m.endJobLocked(JobInterrupted, "the laser's serial port went away at line "+strconv.Itoa(m.job.Done+1))
	}
	if m.controller == ControllerJob {
		m.controller = ControllerNone
	}
	m.mu.Unlock()
	m.logf("laser on %s lost: %v", name, err)
}

// Disconnect lets go of the laser's port so the bridge connects again with
// new settings — only while nobody is using the laser.
func (m *Machine) Disconnect() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.port == nil {
		return nil
	}
	if m.controller != ControllerNone || (m.job != nil && m.job.State.active()) {
		return errors.New("a job or another program is using it now")
	}
	m.logf("letting go of the laser on %s for new settings", m.portName)
	return m.port.Close()
}

// settingsRetry: how often `$$` is tried again while the laser can't answer.
var settingsRetry = 5 * time.Second

// readSettings asks for `$$` if the laser is free to answer it, and says
// whether the settings are in. Someone else asking (LightBurn) counts too.
func (m *Machine) readSettings() bool {
	m.mu.Lock()
	have := len(m.settings) > 0
	state := m.status.State
	free := m.controller == ControllerNone && len(m.flight) == 0
	m.mu.Unlock()
	if have {
		return true
	}
	if !free || !(state == "" || state == "Idle" || strings.HasPrefix(state, "Alarm")) {
		return false
	}
	m.Command("$$")
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.settings) > 0
}

// poll asks for status four times a second. `?` only ever produces a status
// report, never an `ok`, so it can't upset anyone's character counting —
// including a pass-through client's.
func (m *Machine) poll(stop chan struct{}) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.realtime(rtStatus)
		}
	}
}

func (m *Machine) read(port io.Reader) error {
	r := bufio.NewReaderSize(port, 4096)
	var line []byte
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			m.mu.Lock()
			for c := range m.sinks {
				c.send(chunk)
			}
			m.mu.Unlock()
			for _, b := range chunk {
				switch {
				case b == '\n':
					if s := strings.TrimSpace(string(line)); s != "" {
						m.handleLine(s)
					}
					line = line[:0]
				case b == '\r':
				case len(line) < 1024:
					line = append(line, b)
				}
			}
		}
		if err != nil {
			return err
		}
	}
}

func (m *Machine) handleLine(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case line == "ok":
		m.ackLocked(0)
	case strings.HasPrefix(line, "error:"):
		code, _ := strconv.Atoi(line[6:])
		m.ackLocked(code)
	case strings.HasPrefix(line, "ALARM:"):
		code, _ := strconv.Atoi(line[6:])
		m.dropQueuedLocked()
		if m.job != nil && m.job.State.active() {
			m.endJobLocked(JobAlarmed, "alarm: "+describe(grblAlarms, code))
		}
	case strings.HasPrefix(line, "$") && strings.Contains(line, "="):
		key, value, _ := strings.Cut(line[1:], "=")
		if n, err := strconv.Atoi(key); err == nil {
			m.settings[n] = value
		}
		if m.controller != ControllerPassthrough {
			m.offerReply(line)
		}
	case strings.HasPrefix(line, "<"):
		if s, wco, ok := parseStatus(line, m.wco); ok {
			m.status, m.wco, m.statusAt = s, wco, time.Now()
		}
	case strings.HasPrefix(strings.ToLower(line), "grbl "):
		// A reset — ours or the board's. Whatever was in flight is gone.
		m.banner = line
		m.resetQueueLocked()
		if m.job != nil && m.job.State.active() {
			m.endJobLocked(JobStopped, "the laser reset")
		}
	default:
		if m.controller != ControllerPassthrough {
			m.offerReply(line)
		}
	}
}

func (m *Machine) offerReply(line string) {
	select {
	case m.replies <- line:
	default:
	}
}

// ackLocked matches an ok/error to the oldest line in flight. While a
// pass-through client is in control the answers are its own, and the
// bridge has nothing in flight.
func (m *Machine) ackLocked(code int) {
	if len(m.flight) == 0 {
		return
	}
	done := m.flight[0]
	m.flight = m.flight[1:]
	m.flightLen -= done.length

	if done.index < 0 {
		if code != 0 {
			m.offerReply("error:" + strconv.Itoa(code) + " (" + describe(grblErrors, code) + ")")
		} else {
			m.offerReply("ok")
		}
		return
	}
	j := m.job
	if j == nil {
		return
	}
	j.Done = done.index + 1
	if code != 0 && j.State.active() {
		// Lines already in GRBL will still run: hold the head, stop
		// sending, and let the operator decide.
		m.writeLocked([]byte{rtHold})
		m.dropQueuedLocked()
		j.State = JobPausedOnError
		j.Message = "line " + strconv.Itoa(done.index+1) + " rejected: " + describe(grblErrors, code) + ". The head is held — stop the job."
		return
	}
	if (j.State == JobRunning || j.State == JobPaused) && j.Done == j.Total {
		m.endJobLocked(JobCompleted, "")
		return
	}
	m.pumpLocked()
}

func (m *Machine) pumpLocked() {
	j := m.job
	if j == nil || m.port == nil || !(j.State == JobRunning || j.State == JobPaused) {
		return
	}
	for m.next < len(j.lines) {
		line := j.lines[m.next]
		n := len(line) + 1
		if m.flightLen+n > m.bufferSize && len(m.flight) > 0 {
			return
		}
		if !m.writeLocked([]byte(line + "\n")) {
			return
		}
		m.flight = append(m.flight, inFlight{n, m.next})
		m.flightLen += n
		m.next++
		j.Sent = m.next
	}
}

func (m *Machine) dropQueuedLocked() {
	if m.job != nil {
		m.next = len(m.job.lines)
	}
}

func (m *Machine) resetQueueLocked() {
	m.flight, m.flightLen = nil, 0
	m.dropQueuedLocked()
}

func (m *Machine) endJobLocked(state JobState, msg string) {
	m.job.State, m.job.Message, m.job.EndedAt = state, msg, time.Now()
	m.job.lines = nil
	if m.controller == ControllerJob {
		m.controller = ControllerNone
	}
	if msg == "" {
		m.logf("job %q %s", m.job.Name, state)
	} else {
		m.logf("job %q %s: %s", m.job.Name, state, msg)
	}
}

// writeLocked sends bytes to the laser. Callers hold m.mu; writes also take
// writeMu so real-time bytes from other goroutines never split a line.
func (m *Machine) writeLocked(b []byte) bool {
	if m.port == nil {
		return false
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	_, err := m.port.Write(b)
	return err == nil
}

func (m *Machine) realtime(b byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeLocked([]byte{b})
}

// MARK: API

var (
	ErrNotConnected = errors.New("no laser is connected to the bridge")
	ErrBusy         = errors.New("the laser is busy")
)

func (m *Machine) busyReason() string {
	switch m.controller {
	case ControllerPassthrough:
		return "another program (LightBurn or Kerf in direct mode) is connected on the raw port"
	case ControllerJob:
		return "a job is running"
	}
	return ""
}

// StartJob takes control and streams the job from the bridge itself, so
// the client that uploaded it can sleep, leave or lose Wi-Fi.
func (m *Machine) StartJob(name string, lines []string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.port == nil {
		return nil, ErrNotConnected
	}
	if m.controller != ControllerNone || len(m.flight) > 0 {
		return nil, errors.New("the laser is busy: " + m.busyReason())
	}
	m.job = &Job{Name: name, Total: len(lines), State: JobRunning, StartedAt: time.Now(), lines: lines}
	m.controller = ControllerJob
	m.next = 0
	m.logf("job %q started: %d lines", name, len(lines))
	m.pumpLocked()
	return m.copyJobLocked(), nil
}

func (m *Machine) Pause() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil && m.job.State == JobRunning {
		m.writeLocked([]byte{rtHold})
		m.job.State = JobPaused
	}
}

func (m *Machine) Resume() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil || m.job.State != JobPaused {
		return errors.New("nothing is paused")
	}
	m.writeLocked([]byte{rtCycle})
	m.job.State = JobRunning
	m.pumpLocked()
	return nil
}

// Stop holds the head, waits (≤ 1 s) for it to stop, then resets: a reset
// while moving loses position, a reset once held doesn't. It works whoever
// is in control — stopping a laser is never refused.
func (m *Machine) Stop() {
	m.mu.Lock()
	m.dropQueuedLocked()
	m.writeLocked([]byte{rtHold})
	m.mu.Unlock()
	for i := 0; i < 10; i++ {
		m.mu.Lock()
		state := m.status.State
		m.mu.Unlock()
		if state == "Hold:0" || state == "Idle" || state == "Alarm" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil && m.job.State.active() {
		m.endJobLocked(JobStopped, "stop pressed at line "+strconv.Itoa(m.job.Done))
	}
	m.writeLocked([]byte{rtReset})
	m.resetQueueLocked()
}

// Command sends one line (jog, $H, $X, $$ …) and waits for its answer.
// Only when nobody else is in control.
func (m *Machine) Command(line string) ([]string, error) {
	line = cleanLine(line)
	if line == "" {
		return nil, errors.New("empty command")
	}
	m.mu.Lock()
	if m.port == nil {
		m.mu.Unlock()
		return nil, ErrNotConnected
	}
	if m.controller != ControllerNone || len(m.flight) > 0 {
		m.mu.Unlock()
		return nil, errors.New("the laser is busy: " + m.busyReason())
	}
	for len(m.replies) > 0 {
		<-m.replies
	}
	m.flight = append(m.flight, inFlight{len(line) + 1, -1})
	m.flightLen += len(line) + 1
	m.writeLocked([]byte(line + "\n"))
	m.mu.Unlock()

	var out []string
	timeout := time.After(5 * time.Second)
	if strings.HasPrefix(line, "$H") {
		timeout = time.After(60 * time.Second) // homing takes a while
	}
	for {
		select {
		case r := <-m.replies:
			out = append(out, r)
			if r == "ok" || strings.HasPrefix(r, "error:") {
				return out, nil
			}
		case <-timeout:
			return out, errors.New("the laser didn't answer")
		}
	}
}

// MachineInfo is what GRBL's settings say about the machine.
type MachineInfo struct {
	BedWidth     float64 `json:"bedWidth,omitempty"`     // $130
	BedHeight    float64 `json:"bedHeight,omitempty"`    // $131
	MaxPower     float64 `json:"maxPower,omitempty"`     // $30: the S value for 100 %
	LaserMode    *bool   `json:"laserMode,omitempty"`    // $32
	Homing       *bool   `json:"homing,omitempty"`       // $22
	MaxRate      float64 `json:"maxRate,omitempty"`      // $110, mm/min
	Acceleration float64 `json:"acceleration,omitempty"` // $120, mm/s²
}

func (m *Machine) infoLocked() MachineInfo {
	num := func(n int) float64 { v, _ := strconv.ParseFloat(m.settings[n], 64); return v }
	flag := func(n int) *bool {
		v, ok := m.settings[n]
		if !ok {
			return nil
		}
		b := v == "1"
		return &b
	}
	return MachineInfo{BedWidth: num(130), BedHeight: num(131), MaxPower: num(30), LaserMode: flag(32),
		Homing: flag(22), MaxRate: num(110), Acceleration: num(120)}
}

type Snapshot struct {
	Machine    MachineInfo    `json:"machine"`
	Settings   map[int]string `json:"settings,omitempty"`
	Connected  bool           `json:"connected"`
	Port       string         `json:"port,omitempty"`
	Baud       int            `json:"baud,omitempty"`
	Banner     string         `json:"banner,omitempty"`
	Status     *Status        `json:"status,omitempty"`
	Controller Controller     `json:"controller"`
	Job        *Job           `json:"job,omitempty"`
	Watchers   int            `json:"watchers"`
}

func (m *Machine) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	settings := make(map[int]string, len(m.settings))
	for k, v := range m.settings {
		settings[k] = v
	}
	s := Snapshot{Connected: m.port != nil, Port: m.portName, Baud: m.baud, Banner: m.banner,
		Controller: m.controller, Job: m.copyJobLocked(), Watchers: len(m.sinks),
		Machine: m.infoLocked(), Settings: settings}
	if m.port != nil && !m.statusAt.IsZero() {
		st := m.status
		s.Status = &st
	}
	return s
}

func (m *Machine) copyJobLocked() *Job {
	if m.job == nil {
		return nil
	}
	j := *m.job
	j.lines = nil
	return &j
}
