package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Camera serves a USB camera pointed at the bed: live, for watching a burn,
// and as stills, for lining the work up. It runs only while someone is
// looking — the first request opens it, and it closes again after
// cameraIdle with no requests — so it costs nothing the rest of the time.
//
// Frames are the camera's own JPEGs (MJPEG), passed on untouched apart from
// the Huffman tables many cameras leave out, which browsers and iOS need.
type Camera struct {
	cfg func() Config
	// Saves a setting the camera changes (its focus); nil in -snapshot.
	save func(func(*Config)) error

	mu       sync.Mutex
	frame    []byte
	seq      uint64
	at       time.Time
	next     chan struct{} // closed when a new frame arrives
	running  bool
	lastWant time.Time
	openedAt time.Time
	failedAt time.Time
	err      string
	info     CameraInfo
	dev      *videoDevice
}

// CameraInfo is what GET /api/camera reports.
type CameraInfo struct {
	Available bool   `json:"available"`
	Name      string `json:"name,omitempty"`
	Device    string `json:"device,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Running   bool   `json:"running"`
	Error     string `json:"error,omitempty"`
	// The fixed focus; absent while autofocus is on.
	Focus *int `json:"focus,omitempty"`
}

// cameraIdle: how long the camera stays open after the last request.
const cameraIdle = 15 * time.Second

// cameraWarmUp: frames dropped after opening while exposure settles.
const cameraWarmUp = 1500 * time.Millisecond

// cameraRetry: after a failure, how long before opening it is tried again.
const cameraRetry = 3 * time.Second

// errCameraOff is returned when the settings turn the camera off.
var errCameraOff = errors.New("the camera is turned off in the bridge's settings")

func NewCamera(cfg func() Config) *Camera {
	return &Camera{cfg: cfg, next: make(chan struct{})}
}

// Info describes the camera without opening it to stream.
func (c *Camera) Info() CameraInfo {
	c.mu.Lock()
	info, running, err := c.info, c.running, c.err
	c.mu.Unlock()
	if !running {
		found, ferr := findCamera(c.cfg())
		switch {
		case ferr != nil:
			return CameraInfo{Error: ferr.Error()}
		default:
			info.Available, info.Name, info.Device = true, found.Name, found.Device
		}
	}
	info.Running = running
	info.Error = err
	if f := c.cfg().CameraFocus; f >= 0 {
		info.Focus = &f
	}
	return info
}

// cameraSettle: how long autofocus is given before "lock where it is" reads
// the focus, after the camera opens.
const cameraSettle = 2500 * time.Millisecond

// SetFocus fixes the focus at value, or with lock fixes it where autofocus
// has settled, or with value < 0 turns autofocus back on. It applies now
// and is saved, so the camera opens with it from then on.
func (c *Camera) SetFocus(lock bool, value int) (int, error) {
	if _, _, err := c.Frame(0, 6*time.Second); err != nil {
		return 0, err
	}
	c.mu.Lock()
	dev, since := c.dev, c.openedAt
	c.mu.Unlock()
	if dev == nil {
		return 0, errors.New("the camera closed; try again")
	}
	if lock {
		// Autofocus needs a moment to find the bed: after opening, or after
		// being turned back on from a fixed focus.
		wait := cameraSettle - time.Since(since)
		if c.cfg().CameraFocus >= 0 {
			if err := dev.SetFocus(-1); err != nil {
				return 0, err
			}
			wait = cameraSettle
		}
		if wait > 0 {
			time.Sleep(wait)
		}
		f, err := dev.Focus()
		if err != nil {
			return 0, err
		}
		value = f
	}
	if err := dev.SetFocus(value); err != nil {
		return 0, err
	}
	if value < 0 {
		value = -1
	}
	if c.save != nil {
		if err := c.save(func(cfg *Config) { cfg.CameraFocus = value }); err != nil {
			return value, fmt.Errorf("the focus is set but couldn't be saved: %v", err)
		}
	}
	log.Printf("camera: focus %s", map[bool]string{true: "automatic", false: strconv.Itoa(value)}[value < 0])
	return value, nil
}

// Frame returns the first frame newer than after, waiting up to timeout,
// and opens the camera if it isn't running.
func (c *Camera) Frame(after uint64, timeout time.Duration) ([]byte, uint64, error) {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		c.lastWant = time.Now()
		if !c.running {
			if !c.failedAt.IsZero() && time.Since(c.failedAt) < cameraRetry {
				err := c.err
				c.mu.Unlock()
				return nil, 0, errors.New(err)
			}
			c.running = true
			c.err = ""
			go c.run()
		}
		if c.seq > after && c.frame != nil && time.Since(c.at) < 2*time.Second {
			f, s := c.frame, c.seq
			c.mu.Unlock()
			return f, s, nil
		}
		wait := c.next
		c.mu.Unlock()

		left := time.Until(deadline)
		if left <= 0 {
			c.mu.Lock()
			err := c.err
			c.mu.Unlock()
			if err == "" {
				err = "the camera didn't send a picture in time"
			}
			return nil, 0, errors.New(err)
		}
		select {
		case <-wait:
		case <-time.After(left):
		}
	}
}

// run streams until nobody has asked for a frame for cameraIdle.
func (c *Camera) run() {
	cfg := c.cfg()
	found, err := findCamera(cfg)
	var dev *videoDevice
	if err == nil {
		dev, err = openCamera(found, cfg)
	}
	if err != nil {
		c.fail(err)
		return
	}
	log.Printf("camera: %s on %s at %d × %d", found.Name, found.Device, dev.width, dev.height)
	c.mu.Lock()
	c.info = CameraInfo{Available: true, Name: found.Name, Device: found.Device, Width: dev.width, Height: dev.height}
	c.dev, c.openedAt = dev, time.Now()
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.dev = nil
		c.mu.Unlock()
		dev.Close()
	}()
	// The first frames come before the camera has set its exposure: black.
	warm := time.Now().Add(cameraWarmUp)
	for {
		c.mu.Lock()
		idle := time.Since(c.lastWant) > cameraIdle
		c.mu.Unlock()
		if idle {
			log.Printf("camera: closed, nobody watching")
			c.mu.Lock()
			c.running = false
			c.frame = nil
			c.mu.Unlock()
			return
		}
		jpeg, err := dev.Read(time.Second)
		if err != nil {
			c.fail(err)
			return
		}
		if jpeg == nil || time.Now().Before(warm) {
			continue // none within the second, or still settling
		}
		jpeg = withHuffmanTables(jpeg)
		c.mu.Lock()
		c.frame, c.at = jpeg, time.Now()
		c.seq++
		close(c.next)
		c.next = make(chan struct{})
		c.mu.Unlock()
	}
}

func (c *Camera) fail(err error) {
	if !errors.Is(err, errCameraOff) {
		log.Printf("camera: %v", err)
	}
	c.mu.Lock()
	c.running = false
	c.frame = nil
	c.err = err.Error()
	c.failedAt = time.Now()
	close(c.next)
	c.next = make(chan struct{})
	c.mu.Unlock()
}

// cameraRoutes adds the camera's endpoints:
//
//	GET /api/camera              what camera there is
//	GET /api/camera/frame?after= the next JPEG after frame N (X-Frame: its number)
//	GET /api/camera/stream       MJPEG for an <img> on the web page
func cameraRoutes(mux *http.ServeMux, cam *Camera) {
	mux.HandleFunc("GET /api/camera", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, cam.Info())
	})

	// {"lock": true} fixes the focus where autofocus has it now; {"focus": N}
	// fixes it at N; {"focus": -1} turns autofocus back on.
	mux.HandleFunc("POST /api/camera/focus", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Lock  bool `json:"lock"`
			Focus *int `json:"focus"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil || (!req.Lock && req.Focus == nil) {
			writeError(w, 400, `send {"lock": true} or {"focus": N}`)
			return
		}
		value := -1
		if req.Focus != nil {
			value = *req.Focus
		}
		if _, err := cam.SetFocus(req.Lock, value); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, cam.Info())
	})

	mux.HandleFunc("GET /api/camera/frame", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		// The first frame after opening takes a moment; later ones don't.
		jpeg, seq, err := cam.Frame(after, 4*time.Second)
		if err != nil {
			writeError(w, 503, err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame", strconv.FormatUint(seq, 10))
		w.Write(jpeg)
	})

	mux.HandleFunc("GET /api/camera/stream", func(w http.ResponseWriter, r *http.Request) {
		const boundary = "kerfframe"
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
		w.Header().Set("Cache-Control", "no-store")
		flusher, _ := w.(http.Flusher)
		var seq uint64
		// Ten frames a second is plenty to watch a burn and spares the Wi-Fi.
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			jpeg, s, err := cam.Frame(seq, 4*time.Second)
			if err != nil {
				return
			}
			seq = s
			fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", boundary, len(jpeg))
			if _, err := w.Write(jpeg); err != nil {
				return
			}
			w.Write([]byte("\r\n"))
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
			}
		}
	})
}

// withHuffmanTables adds the standard JPEG Huffman tables to a frame that
// has none. UVC cameras leave them out of MJPEG frames to save bandwidth
// (the spec says decoders assume the standard ones); browsers and iOS
// refuse such a frame on its own.
func withHuffmanTables(jpeg []byte) []byte {
	if len(jpeg) < 4 || jpeg[0] != 0xFF || jpeg[1] != 0xD8 {
		return jpeg
	}
	// Walk the segments up to the start of scan.
	i := 2
	for i+4 <= len(jpeg) {
		if jpeg[i] != 0xFF {
			return jpeg
		}
		marker := jpeg[i+1]
		if marker == 0xC4 {
			return jpeg // has its own tables
		}
		if marker == 0xDA {
			out := make([]byte, 0, len(jpeg)+len(standardDHT))
			out = append(out, jpeg[:i]...)
			out = append(out, standardDHT...)
			return append(out, jpeg[i:]...)
		}
		length := int(jpeg[i+2])<<8 | int(jpeg[i+3])
		i += 2 + length
	}
	return jpeg
}

// standardDHT is one DHT segment holding the four tables of the JPEG
// spec's Annex K.3 (luminance and chrominance, DC and AC).
var standardDHT = func() []byte {
	type table struct {
		class, id byte
		bits      [16]byte
		vals      []byte
	}
	seq := func(n int) []byte {
		v := make([]byte, n)
		for i := range v {
			v[i] = byte(i)
		}
		return v
	}
	tables := []table{
		{0, 0, [16]byte{0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0}, seq(12)},
		{1, 0, [16]byte{0, 2, 1, 3, 3, 2, 4, 3, 5, 5, 4, 4, 0, 0, 1, 0x7d}, []byte{
			0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12, 0x21, 0x31, 0x41, 0x06, 0x13, 0x51, 0x61, 0x07,
			0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xa1, 0x08, 0x23, 0x42, 0xb1, 0xc1, 0x15, 0x52, 0xd1, 0xf0,
			0x24, 0x33, 0x62, 0x72, 0x82, 0x09, 0x0a, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x25, 0x26, 0x27, 0x28,
			0x29, 0x2a, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49,
			0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69,
			0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
			0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7,
			0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3, 0xc4, 0xc5,
			0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xe1, 0xe2,
			0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
			0xf9, 0xfa}},
		{0, 1, [16]byte{0, 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0}, seq(12)},
		{1, 1, [16]byte{0, 2, 1, 2, 4, 4, 3, 4, 7, 5, 4, 4, 0, 1, 2, 0x77}, []byte{
			0x00, 0x01, 0x02, 0x03, 0x11, 0x04, 0x05, 0x21, 0x31, 0x06, 0x12, 0x41, 0x51, 0x07, 0x61, 0x71,
			0x13, 0x22, 0x32, 0x81, 0x08, 0x14, 0x42, 0x91, 0xa1, 0xb1, 0xc1, 0x09, 0x23, 0x33, 0x52, 0xf0,
			0x15, 0x62, 0x72, 0xd1, 0x0a, 0x16, 0x24, 0x34, 0xe1, 0x25, 0xf1, 0x17, 0x18, 0x19, 0x1a, 0x26,
			0x27, 0x28, 0x29, 0x2a, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48,
			0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68,
			0x69, 0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
			0x88, 0x89, 0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5,
			0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3,
			0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda,
			0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
			0xf9, 0xfa}},
	}
	var body bytes.Buffer
	for _, t := range tables {
		body.WriteByte(t.class<<4 | t.id)
		body.Write(t.bits[:])
		body.Write(t.vals)
	}
	n := body.Len() + 2
	return append([]byte{0xFF, 0xC4, byte(n >> 8), byte(n)}, body.Bytes()...)
}()
