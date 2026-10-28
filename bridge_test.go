package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGRBL is a laser in memory: a 128-byte receive buffer, real-time bytes
// handled at once, one line answered per millisecond so the buffer fills.
type fakeGRBL struct {
	mu       sync.Mutex
	rx       []byte
	received []string
	realtime []byte
	overflow bool
	state    string
	failOn   string
	closed   bool
	out      *io.PipeWriter
	in       *io.PipeReader
	replies  chan string
}

func newFake() *fakeGRBL {
	r, w := io.Pipe()
	f := &fakeGRBL{state: "Idle", in: r, out: w, replies: make(chan string, 4096)}
	go f.run()
	// One writer, so replies arrive in the order GRBL sends them.
	go func() {
		for s := range f.replies {
			if _, err := w.Write([]byte(s + "\r\n")); err != nil {
				return
			}
		}
	}()
	return f
}

func (f *fakeGRBL) Read(p []byte) (int, error) { return f.in.Read(p) }
func (f *fakeGRBL) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return f.out.CloseWithError(io.EOF)
}

func (f *fakeGRBL) reply(s string) {
	select {
	case f.replies <- s:
	default:
	}
}

func (f *fakeGRBL) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range p {
		switch b {
		case rtStatus:
			f.reply("<" + f.state + "|MPos:0.000,0.000,0.000|FS:0,0>")
		case rtHold:
			f.realtime = append(f.realtime, b)
			f.state = "Hold:0"
		case rtCycle:
			f.realtime = append(f.realtime, b)
			f.state = "Idle"
		case rtReset:
			f.realtime = append(f.realtime, b)
			f.rx = nil
			f.state = "Idle"
			f.reply("Grbl 1.1h ['$' for help]")
		default:
			f.rx = append(f.rx, b)
		}
	}
	if len(f.rx) > 128 {
		f.overflow = true
	}
	return len(p), nil
}

func (f *fakeGRBL) run() {
	for {
		time.Sleep(time.Millisecond)
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			return
		}
		i := bytes.IndexByte(f.rx, '\n')
		if i < 0 || f.state == "Hold:0" {
			f.mu.Unlock()
			continue
		}
		line := string(f.rx[:i])
		f.rx = f.rx[i+1:]
		f.received = append(f.received, line)
		if f.failOn != "" && strings.Contains(line, f.failOn) {
			f.reply("error:20")
		} else if line == "$$" {
			for _, kv := range []string{"$22=1", "$30=1000", "$32=1", "$110=6000.000", "$120=1500.000", "$130=200.000", "$131=150.000"} {
				f.reply(kv)
			}
			f.reply("ok")
		} else if line == "$I" {
			f.reply("[VER:1.1h.fake]")
			f.reply("ok")
		} else {
			f.reply("ok")
		}
		f.mu.Unlock()
	}
}

func (f *fakeGRBL) sawRealtime(b byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.IndexByte(f.realtime, b) >= 0
}

func (f *fakeGRBL) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func setup(t *testing.T) (*Machine, *fakeGRBL) {
	t.Helper()
	m := NewMachine()
	// The machine's goroutines outlive each test; t.Logf after a test ends
	// is a race in the testing package, not in the bridge.
	m.logf = func(string, ...any) {}
	f := newFake()
	go m.Attach(f, "fake", 115200, "Grbl 1.1h ['$' for help]")
	// Connected, and the connect-time `$$` answered, so it can't land in
	// the middle of a test.
	waitFor(t, func() bool { return m.Snapshot().Machine.BedWidth > 0 })
	t.Cleanup(func() { f.Close() })
	return m, f
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func jobLines(n int) []string {
	lines := []string{"M4S0"}
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf("G1X%d.25Y%d.5S%d", i%300, i%200, i%1000))
	}
	return append(lines, "M5")
}

func TestJobStreamsWithoutOverflowing(t *testing.T) {
	m, f := setup(t)
	lines := jobLines(2000)
	before := len(f.lines()) // the connect-time $$
	if _, err := m.StartJob("test", lines); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { j := m.Snapshot().Job; return j != nil && j.State == JobCompleted })
	f.mu.Lock()
	overflow := f.overflow
	f.mu.Unlock()
	if overflow {
		t.Fatal("GRBL's receive buffer overflowed")
	}
	if got := f.lines()[before:]; strings.Join(got, "\n") != strings.Join(lines, "\n") {
		t.Fatalf("sent %d lines, want %d in order", len(got), len(lines))
	}
	if c := m.Snapshot().Controller; c != ControllerNone {
		t.Fatalf("controller after job = %s", c)
	}
}

func TestErrorHoldsTheHeadAndStopsSending(t *testing.T) {
	m, f := setup(t)
	f.mu.Lock()
	f.failOn = "X150.25"
	f.mu.Unlock()
	m.StartJob("test", jobLines(1000))
	waitFor(t, func() bool { j := m.Snapshot().Job; return j != nil && j.State == JobPausedOnError })
	if !f.sawRealtime(rtHold) {
		t.Fatal("no feed hold after the error")
	}
	if !strings.Contains(m.Snapshot().Job.Message, "line 152 rejected") {
		t.Fatalf("message: %s", m.Snapshot().Job.Message)
	}
	before := len(f.lines())
	time.Sleep(50 * time.Millisecond)
	if len(f.lines()) > before+10 {
		t.Fatal("kept sending after the error")
	}
	if m.Resume() == nil {
		t.Fatal("a job can't carry on past a rejected line")
	}
}

func TestStopHoldsThenResets(t *testing.T) {
	m, f := setup(t)
	m.StartJob("test", jobLines(5000))
	waitFor(t, func() bool { return m.Snapshot().Job.Done > 50 })
	m.Stop()
	if j := m.Snapshot().Job; j.State != JobStopped {
		t.Fatalf("state %s", j.State)
	}
	f.mu.Lock()
	rt := append([]byte(nil), f.realtime...)
	f.mu.Unlock()
	if len(rt) < 2 || rt[0] != rtHold || rt[1] != rtReset {
		t.Fatalf("realtime bytes %v, want hold then reset", rt)
	}
	n := len(f.lines())
	time.Sleep(50 * time.Millisecond)
	if len(f.lines()) != n {
		t.Fatal("still sending after stop")
	}
}

func TestPauseAndResume(t *testing.T) {
	m, _ := setup(t)
	m.StartJob("test", jobLines(3000))
	waitFor(t, func() bool { return m.Snapshot().Job.Done > 20 })
	m.Pause()
	done := m.Snapshot().Job.Done
	time.Sleep(50 * time.Millisecond)
	if d := m.Snapshot().Job.Done; d-done > 1 {
		t.Fatalf("progressed %d lines while paused", d-done)
	}
	if err := m.Resume(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return m.Snapshot().Job.State == JobCompleted })
}

func TestLostPortInterruptsTheJob(t *testing.T) {
	m, f := setup(t)
	m.StartJob("test", jobLines(5000))
	waitFor(t, func() bool { return m.Snapshot().Job.Done > 100 })
	f.Close()
	waitFor(t, func() bool { return !m.Snapshot().Connected })
	j := m.Snapshot().Job
	if j.State != JobInterrupted || !strings.Contains(j.Message, "serial port went away") {
		t.Fatalf("%s: %s", j.State, j.Message)
	}
	if m.Snapshot().Controller != ControllerNone {
		t.Fatal("controller still held")
	}
}

func TestCommandWaitsForItsAnswer(t *testing.T) {
	m, _ := setup(t)
	waitFor(t, func() bool { return m.Snapshot().Machine.BedWidth > 0 })
	replies, err := m.Command("$I")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(replies, "|") != "[VER:1.1h.fake]|ok" {
		t.Fatalf("replies %v", replies)
	}
}

func TestReadsBedSizeOnConnectLikeLightBurn(t *testing.T) {
	m, _ := setup(t)
	waitFor(t, func() bool { return m.Snapshot().Machine.BedWidth > 0 })
	info := m.Snapshot().Machine
	if info.BedWidth != 200 || info.BedHeight != 150 || info.MaxPower != 1000 || info.MaxRate != 6000 ||
		info.Acceleration != 1500 || info.LaserMode == nil || !*info.LaserMode || info.Homing == nil || !*info.Homing {
		t.Fatalf("%+v", info)
	}
}

func TestStatusIsParsed(t *testing.T) {
	m, _ := setup(t)
	waitFor(t, func() bool { return m.Snapshot().Status != nil })
	if s := m.Snapshot().Status; s.State != "Idle" {
		t.Fatalf("state %q", s.State)
	}
}

// MARK: pass-through

func rawListen(t *testing.T, m *Machine) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go ServeRaw(m, ln)
	return ln.Addr().String()
}

func TestPassThroughOwnsTheLaserAndOthersOnlyWatch(t *testing.T) {
	m, f := setup(t)
	addr := rawListen(t, m)

	owner, _ := net.Dial("tcp", addr)
	defer owner.Close()
	waitFor(t, func() bool { return m.Snapshot().Controller == ControllerPassthrough })

	// The owner talks straight to GRBL and hears its answers.
	owner.Write([]byte("G0X5\n"))
	r := bufio.NewReader(owner)
	waitFor(t, func() bool {
		owner.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		line, _ := r.ReadString('\n')
		return strings.TrimSpace(line) == "ok"
	})

	// Nobody else can start a job or send commands meanwhile.
	if _, err := m.StartJob("x", []string{"G0X1"}); err == nil {
		t.Fatal("job started while LightBurn was in control")
	}
	if _, err := m.Command("$H"); err == nil {
		t.Fatal("command accepted while LightBurn was in control")
	}

	// A second client watches: its G-code is dropped, its feed hold isn't.
	watcher, _ := net.Dial("tcp", addr)
	defer watcher.Close()
	wr := bufio.NewReader(watcher)
	watcher.SetReadDeadline(time.Now().Add(time.Second))
	hello, _ := wr.ReadString('\n')
	if !strings.Contains(hello, "watching only") {
		t.Fatalf("watcher greeting %q", hello)
	}
	watcher.Write([]byte("G0X99\n!"))
	waitFor(t, func() bool { return f.sawRealtime(rtHold) })
	time.Sleep(20 * time.Millisecond)
	for _, l := range f.lines() {
		if l == "G0X99" {
			t.Fatal("a watcher's G-code reached the laser")
		}
	}

	owner.Close()
	waitFor(t, func() bool { return m.Snapshot().Controller == ControllerNone })
}

func TestPassThroughWaitsWhileAJobRuns(t *testing.T) {
	m, _ := setup(t)
	addr := rawListen(t, m)
	m.StartJob("long", jobLines(20000))
	c, _ := net.Dial("tcp", addr)
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(time.Second))
	hello, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.Contains(hello, "a job is running") {
		t.Fatalf("greeting %q", hello)
	}
	if m.Snapshot().Controller != ControllerJob {
		t.Fatal("the job lost control to a raw client")
	}
	m.Stop()
}

// MARK: HTTP

func TestAPIJobLifecycle(t *testing.T) {
	m, _ := setup(t)
	dir := t.TempDir()
	store, _ := LoadConfig(dir + "/config.json")
	srv := httptest.NewServer(NewAPI(m, store, NewCamera(store.Get), nil))
	defer srv.Close()

	post := func(path, body string) (int, map[string]any) {
		r, err := http.Post(srv.URL+path, "text/plain", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out map[string]any
		json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out
	}

	if code, out := post("/api/job?name=empty", "; nothing\n"); code != 400 || !strings.Contains(out["error"].(string), "no G-code") {
		t.Fatalf("empty job: %d %v", code, out)
	}
	code, out := post("/api/job?name=sq.gcode", strings.Join(jobLines(3000), "\n"))
	if code != 201 || out["name"] != "sq.gcode" {
		t.Fatalf("upload: %d %v", code, out)
	}
	if code, out := post("/api/job", "G0X1"); code != 409 || !strings.Contains(out["error"].(string), "a job is running") {
		t.Fatalf("second upload: %d %v", code, out)
	}

	r, _ := http.Get(srv.URL + "/api/status")
	var status struct {
		Machine Snapshot `json:"machine"`
	}
	json.NewDecoder(r.Body).Decode(&status)
	r.Body.Close()
	if status.Machine.Controller != ControllerJob || status.Machine.Job.Total != 3002 {
		t.Fatalf("status %+v", status.Machine)
	}

	if code, _ := post("/api/stop", ""); code != 200 {
		t.Fatalf("stop: %d", code)
	}
	if m.Snapshot().Job.State != JobStopped {
		t.Fatal("not stopped")
	}

	ports, _ := http.Get(srv.URL + "/api/ports")
	pb, _ := io.ReadAll(ports.Body)
	if strings.TrimSpace(string(pb)) == "null" {
		t.Fatal("/api/ports must be a list, even an empty one")
	}

	page, _ := http.Get(srv.URL + "/")
	b, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(b), "Kerf Bridge") {
		t.Fatal("web page not served")
	}
}

// MARK: parsing

func TestPrepareJobCleans(t *testing.T) {
	lines, err := prepareJob("; header\n%\nG21 (mm) G90\r\ng1 x10 y10 f1000 ; move\n\nM5\n")
	if err != nil || strings.Join(lines, "|") != "G21G90|G1X10Y10F1000|M5" {
		t.Fatalf("%v %v", lines, err)
	}
	if _, err := prepareJob("G1X" + strings.Repeat("1", 80)); err == nil {
		t.Fatal("over-long line accepted")
	}
}

func TestParseStatusWorkPosition(t *testing.T) {
	s, wco, ok := parseStatus("<Run|MPos:15.000,25.000,0.000|FS:1200,500|WCO:5.000,10.000,0.000>", [3]float64{})
	if !ok || s.State != "Run" || s.WPos != [3]float64{10, 15, 0} || s.Feed != 1200 || s.Power != 500 {
		t.Fatalf("%+v", s)
	}
	s, _, _ = parseStatus("<Idle|MPos:20,20,0|FS:0,0>", wco)
	if s.WPos != [3]float64{15, 10, 0} {
		t.Fatalf("kept offset: %+v", s.WPos)
	}
}

func TestConfigSavesAtomicallyAndReloads(t *testing.T) {
	path := t.TempDir() + "/c.json"
	s, err := LoadConfig(path)
	if err != nil || s.Get().HTTPAddr != ":8023" || s.Get().RawAddr != ":23" {
		t.Fatalf("%v %+v", err, s.Get())
	}
	s.Update(func(c *Config) { c.SerialPort = "/dev/ttyUSB0"; c.Baud = 115200 })
	again, _ := LoadConfig(path)
	if again.Get().SerialPort != "/dev/ttyUSB0" || again.Get().Baud != 115200 {
		t.Fatalf("%+v", again.Get())
	}
}

func TestSettingsAreReadOnceAHeldLaserIsFree(t *testing.T) {
	settingsRetry = 20 * time.Millisecond
	defer func() { settingsRetry = 5 * time.Second }()
	m := NewMachine()
	m.logf = func(string, ...any) {}
	f := newFake()
	f.mu.Lock()
	f.state = "Hold:0" // the lid's safety switch
	f.mu.Unlock()
	go m.Attach(f, "fake", 115200, "Grbl 1.3a ['$' for help]")
	t.Cleanup(func() { f.Close() })

	waitFor(t, func() bool { st := m.Snapshot().Status; return st != nil && st.State == "Hold:0" })
	time.Sleep(400 * time.Millisecond)
	if m.Snapshot().Machine.BedWidth != 0 {
		t.Fatal("settings read while the laser was held")
	}
	for _, line := range f.lines() {
		if line == "$$" {
			t.Fatal("$$ was sent while the laser was held")
		}
	}
	f.mu.Lock()
	f.state = "Idle" // lid closed
	f.mu.Unlock()
	waitFor(t, func() bool { return m.Snapshot().Machine.BedWidth == 200 })
}
