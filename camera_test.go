package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stripDHT removes every Huffman table segment, as UVC cameras send frames.
func stripDHT(t *testing.T, j []byte) []byte {
	t.Helper()
	out := append([]byte(nil), j[:2]...)
	i := 2
	for i+4 <= len(j) {
		marker := j[i+1]
		if marker == 0xDA {
			return append(out, j[i:]...)
		}
		n := int(j[i+2])<<8 | int(j[i+3])
		if marker != 0xC4 {
			out = append(out, j[i:i+2+n]...)
		}
		i += 2 + n
	}
	t.Fatal("no start of scan")
	return nil
}

func TestFramesWithoutHuffmanTablesDecodeOnceFixed(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 5), 90, 255})
		}
	}
	var buf bytes.Buffer
	// Go's encoder uses the standard tables, so a frame stripped of them
	// and given ours back must decode to the same picture.
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	stripped := stripDHT(t, buf.Bytes())
	if _, err := jpeg.Decode(bytes.NewReader(stripped)); err == nil {
		t.Fatal("a frame with no Huffman tables decoded; the test isn't testing anything")
	}
	fixed := withHuffmanTables(stripped)
	got, err := jpeg.Decode(bytes.NewReader(fixed))
	if err != nil {
		t.Fatalf("fixed frame doesn't decode: %v", err)
	}
	want, _ := jpeg.Decode(bytes.NewReader(buf.Bytes()))
	if got.Bounds() != want.Bounds() {
		t.Fatalf("size %v, want %v", got.Bounds(), want.Bounds())
	}
	r1, g1, b1, _ := got.At(40, 30).RGBA()
	r2, g2, b2, _ := want.At(40, 30).RGBA()
	if r1 != r2 || g1 != g2 || b1 != b2 {
		t.Fatal("fixed frame decodes to a different picture")
	}
}

func TestFramesWithTablesAreLeftAlone(t *testing.T) {
	var buf bytes.Buffer
	jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8)), nil)
	if out := withHuffmanTables(buf.Bytes()); !bytes.Equal(out, buf.Bytes()) {
		t.Fatal("a complete JPEG was changed")
	}
	if out := withHuffmanTables([]byte("not a jpeg")); string(out) != "not a jpeg" {
		t.Fatal("non-JPEG data was changed")
	}
}

func TestCameraTurnedOff(t *testing.T) {
	store := &ConfigStore{cfg: Config{CameraDevice: "off"}}
	srv := httptest.NewServer(NewAPI(NewMachine(), store, NewCamera(store.Get), nil))
	defer srv.Close()

	r, err := http.Get(srv.URL + "/api/camera")
	if err != nil {
		t.Fatal(err)
	}
	var info CameraInfo
	json.NewDecoder(r.Body).Decode(&info)
	r.Body.Close()
	if info.Available || info.Error == "" {
		t.Fatalf("camera off reported as %+v", info)
	}

	r, err = http.Get(srv.URL + "/api/camera/frame")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 503 {
		t.Fatalf("frame with the camera off: %d, want 503", r.StatusCode)
	}
}
