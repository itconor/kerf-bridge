//go:build linux && (arm64 || amd64)

package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Video4Linux2, spoken directly: open the camera, ask for MJPEG, map a few
// buffers and pass each frame on as it comes. No ffmpeg, no cgo. The struct
// layouts and ioctl numbers below are the 64-bit ones (arm64 and amd64);
// 32-bit builds get v4l2_other.go.

const (
	v4l2BufTypeVideoCapture = 1
	v4l2MemoryMmap          = 1
	v4l2CapVideoCapture     = 0x00000001
	v4l2CapDeviceCaps       = 0x80000000
	v4l2PixFmtMJPEG         = 'M' | 'J'<<8 | 'P'<<16 | 'G'<<24

	v4l2CidFocusAbsolute = 0x009a090a
	v4l2CidFocusAuto     = 0x009a090c

	vidiocQueryCap  = 0x80685600
	vidiocSFmt      = 0xC0D05605
	vidiocReqBufs   = 0xC0145608
	vidiocQueryBuf  = 0xC0585609
	vidiocQBuf      = 0xC058560F
	vidiocDQBuf     = 0xC0585611
	vidiocStreamOn  = 0x40045612
	vidiocStreamOff = 0x40045613
	vidiocSCtrl     = 0xC008561C
	vidiocGCtrl     = 0xC008561B
)

type v4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

type v4l2PixFormat struct {
	Width, Height, PixelFormat, Field, BytesPerLine, SizeImage uint32
	Colorspace, Priv, Flags, YcbcrEnc, Quantization, XferFunc  uint32
}

type v4l2Format struct {
	Type uint32
	_    uint32 // the union is 8-aligned on 64-bit
	Pix  v4l2PixFormat
	_    [152]byte
}

type v4l2RequestBuffers struct {
	Count, Type, Memory, Capabilities uint32
	Flags                             uint8
	_                                 [3]uint8
}

type v4l2Buffer struct {
	Index, Type, BytesUsed, Flags, Field uint32
	_                                    uint32
	TimestampSec, TimestampUsec          int64
	Timecode                             [16]byte
	Sequence, Memory                     uint32
	Offset                               uint32 // m.offset; the union is 8 bytes
	_                                    uint32
	Length, Reserved2, Reserved          uint32
	_                                    uint32
}

type v4l2Control struct {
	ID    uint32
	Value int32
}

// Compile-time size checks against the kernel's structs.
var (
	_ [104 - unsafe.Sizeof(v4l2Capability{})]byte
	_ [unsafe.Sizeof(v4l2Capability{}) - 104]byte
	_ [208 - unsafe.Sizeof(v4l2Format{})]byte
	_ [unsafe.Sizeof(v4l2Format{}) - 208]byte
	_ [20 - unsafe.Sizeof(v4l2RequestBuffers{})]byte
	_ [unsafe.Sizeof(v4l2RequestBuffers{}) - 20]byte
	_ [88 - unsafe.Sizeof(v4l2Buffer{})]byte
	_ [unsafe.Sizeof(v4l2Buffer{}) - 88]byte
)

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

type cameraFound struct {
	Name, Device string
}

// findCamera picks the camera in the settings, or the first USB camera that
// captures video. "off" turns it off.
func findCamera(cfg Config) (cameraFound, error) {
	switch cfg.CameraDevice {
	case "off":
		return cameraFound{}, errCameraOff
	case "":
	default:
		name, err := captureName(cfg.CameraDevice)
		if err != nil {
			return cameraFound{}, fmt.Errorf("the camera %s can't be used: %v", cfg.CameraDevice, err)
		}
		return cameraFound{name, cfg.CameraDevice}, nil
	}
	cams, err := listCameras()
	if len(cams) == 0 {
		return cameraFound{}, err
	}
	return cams[0], nil
}

// listCameras finds the USB cameras that capture video: by id first, named
// by their stable /dev/v4l/by-id path (a /dev/videoN number can change
// with the order they're found at boot), then any other /dev/videoN. A
// camera shows several nodes; only the capture one answers captureName.
func listCameras() ([]cameraFound, error) {
	type node struct{ dev, id string }
	var nodes []node
	ids, _ := filepath.Glob("/dev/v4l/by-id/*-video-index*")
	sort.Strings(ids)
	for _, id := range ids {
		if dev, err := filepath.EvalSymlinks(id); err == nil {
			nodes = append(nodes, node{dev, id})
		}
	}
	all, _ := filepath.Glob("/dev/video*")
	sort.Slice(all, func(i, j int) bool { return len(all[i]) < len(all[j]) || len(all[i]) == len(all[j]) && all[i] < all[j] })
	for _, dev := range all {
		nodes = append(nodes, node{dev, ""})
	}
	seen := map[string]bool{}
	var denied error
	var cams []cameraFound
	for _, n := range nodes {
		if seen[n.dev] {
			continue
		}
		seen[n.dev] = true
		name, err := captureName(n.dev)
		if os.Getenv("KERF_CAMDEBUG") != "" {
			log.Printf("camera debug: %s: %q %v", n.dev, name, err)
		}
		if errors.Is(err, unix.EACCES) {
			denied = err
		}
		if err == nil {
			dev := n.dev
			if n.id != "" {
				dev = n.id
			}
			cams = append(cams, cameraFound{name, dev})
		}
	}
	if len(cams) > 0 {
		return cams, nil
	}
	if denied != nil {
		return nil, errors.New("a camera is plugged in but the bridge isn't allowed to open it — add the bridge's user to the video group")
	}
	return nil, errors.New("no camera is plugged in")
}

// captureName opens a node and returns its name if it's a USB camera that
// captures video (the Pi's own codec nodes are skipped).
func captureName(dev string) (string, error) {
	fd, err := unix.Open(dev, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	var cap v4l2Capability
	if err := ioctl(fd, vidiocQueryCap, unsafe.Pointer(&cap)); err != nil {
		return "", err
	}
	caps := cap.Capabilities
	if caps&v4l2CapDeviceCaps != 0 {
		caps = cap.DeviceCaps
	}
	if caps&v4l2CapVideoCapture == 0 {
		return "", errors.New("not a capture device")
	}
	if driver := cstr(cap.Driver[:]); driver != "uvcvideo" {
		return "", fmt.Errorf("%s isn't a USB camera", driver)
	}
	name := cstr(cap.Card[:])
	// "4k Webcam: USB Camera" — the part before the colon is the model.
	if i := strings.Index(name, ":"); i > 0 {
		name = strings.TrimSpace(name[:i])
	}
	return name, nil
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

type videoDevice struct {
	fd            int
	buffers       [][]byte
	width, height int
}

// openCamera starts the camera streaming MJPEG at the size in the settings
// (it picks the nearest size it has).
func openCamera(found cameraFound, cfg Config) (*videoDevice, error) {
	fd, err := unix.Open(found.Device, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.EACCES) {
			return nil, errors.New("the bridge isn't allowed to open the camera — add the bridge's user to the video group")
		}
		if errors.Is(err, unix.EBUSY) {
			return nil, errors.New("another program is using the camera")
		}
		return nil, err
	}
	d := &videoDevice{fd: fd}
	fail := func(what string, err error) (*videoDevice, error) {
		d.Close()
		if errors.Is(err, unix.EBUSY) {
			return nil, errors.New("another program is using the camera")
		}
		return nil, fmt.Errorf("the camera wouldn't %s: %v", what, err)
	}

	w, h := cfg.CameraWidth, cfg.CameraHeight
	if w <= 0 || h <= 0 {
		w, h = 1600, 1200
	}
	f := v4l2Format{Type: v4l2BufTypeVideoCapture}
	f.Pix = v4l2PixFormat{Width: uint32(w), Height: uint32(h), PixelFormat: v4l2PixFmtMJPEG}
	if err := ioctl(fd, vidiocSFmt, unsafe.Pointer(&f)); err != nil {
		return fail("start", err)
	}
	if f.Pix.PixelFormat != v4l2PixFmtMJPEG {
		d.Close()
		return nil, errors.New("this camera doesn't send MJPEG, which the bridge needs")
	}
	d.width, d.height = int(f.Pix.Width), int(f.Pix.Height)

	// A fixed focus keeps the picture's scale the same from day to day,
	// which lining work up with the camera relies on.
	if cfg.CameraFocus >= 0 {
		d.SetFocus(cfg.CameraFocus)
	}

	req := v4l2RequestBuffers{Count: 4, Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
	if err := ioctl(fd, vidiocReqBufs, unsafe.Pointer(&req)); err != nil {
		return fail("give buffers", err)
	}
	for i := uint32(0); i < req.Count; i++ {
		b := v4l2Buffer{Index: i, Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
		if err := ioctl(fd, vidiocQueryBuf, unsafe.Pointer(&b)); err != nil {
			return fail("give buffers", err)
		}
		mem, err := unix.Mmap(fd, int64(b.Offset), int(b.Length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return fail("map buffers", err)
		}
		d.buffers = append(d.buffers, mem)
		if err := ioctl(fd, vidiocQBuf, unsafe.Pointer(&b)); err != nil {
			return fail("queue buffers", err)
		}
	}
	typ := uint32(v4l2BufTypeVideoCapture)
	if err := ioctl(fd, vidiocStreamOn, unsafe.Pointer(&typ)); err != nil {
		return fail("start streaming", err)
	}
	return d, nil
}

// Read waits up to timeout for the next frame; nil with no error means none
// came in time.
func (d *videoDevice) Read(timeout time.Duration) ([]byte, error) {
	fds := []unix.PollFd{{Fd: int32(d.fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(timeout/time.Millisecond))
	if err == unix.EINTR || n == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fds[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
		return nil, errors.New("the camera was unplugged")
	}
	b := v4l2Buffer{Type: v4l2BufTypeVideoCapture, Memory: v4l2MemoryMmap}
	if err := ioctl(d.fd, vidiocDQBuf, unsafe.Pointer(&b)); err != nil {
		if err == unix.EAGAIN {
			return nil, nil
		}
		if err == unix.ENODEV {
			return nil, errors.New("the camera was unplugged")
		}
		return nil, err
	}
	var frame []byte
	if int(b.Index) < len(d.buffers) && int(b.BytesUsed) <= len(d.buffers[b.Index]) {
		frame = append([]byte(nil), d.buffers[b.Index][:b.BytesUsed]...)
	}
	if err := ioctl(d.fd, vidiocQBuf, unsafe.Pointer(&b)); err != nil {
		return nil, err
	}
	// A frame cut short (USB hiccup) has no end-of-image marker; skip it.
	if len(frame) < 4 || frame[0] != 0xFF || frame[1] != 0xD8 {
		return nil, nil
	}
	return frame, nil
}

// Focus is the lens position now — where autofocus has settled, if on.
func (d *videoDevice) Focus() (int, error) {
	ctrl := v4l2Control{ID: v4l2CidFocusAbsolute}
	if err := ioctl(d.fd, vidiocGCtrl, unsafe.Pointer(&ctrl)); err != nil {
		return 0, fmt.Errorf("this camera won't report its focus: %v", err)
	}
	return int(ctrl.Value), nil
}

// SetFocus fixes the focus at v, or turns autofocus back on for v < 0.
func (d *videoDevice) SetFocus(v int) error {
	if v < 0 {
		ctrl := v4l2Control{ID: v4l2CidFocusAuto, Value: 1}
		return ioctl(d.fd, vidiocSCtrl, unsafe.Pointer(&ctrl))
	}
	ctrl := v4l2Control{ID: v4l2CidFocusAuto, Value: 0}
	if err := ioctl(d.fd, vidiocSCtrl, unsafe.Pointer(&ctrl)); err != nil {
		return fmt.Errorf("this camera won't turn autofocus off: %v", err)
	}
	ctrl = v4l2Control{ID: v4l2CidFocusAbsolute, Value: int32(v)}
	if err := ioctl(d.fd, vidiocSCtrl, unsafe.Pointer(&ctrl)); err != nil {
		return fmt.Errorf("this camera won't take a fixed focus: %v", err)
	}
	return nil
}

func (d *videoDevice) Close() {
	typ := uint32(v4l2BufTypeVideoCapture)
	ioctl(d.fd, vidiocStreamOff, unsafe.Pointer(&typ))
	for _, b := range d.buffers {
		unix.Munmap(b)
	}
	d.buffers = nil
	unix.Close(d.fd)
}
