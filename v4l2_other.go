//go:build !(linux && (arm64 || amd64))

package main

import (
	"errors"
	"time"
)

// The camera is read through Video4Linux2 on 64-bit Linux (a Raspberry Pi
// on a 64-bit OS, or a Linux PC). Other builds have no camera yet.

type cameraFound struct {
	Name, Device string
}

type videoDevice struct {
	width, height int
}

var errNoCameraHere = errors.New("this build of the bridge can't use a camera — it needs 64-bit Linux")

func findCamera(cfg Config) (cameraFound, error) {
	if cfg.CameraDevice == "off" {
		return cameraFound{}, errCameraOff
	}
	return cameraFound{}, errNoCameraHere
}

func openCamera(cameraFound, Config) (*videoDevice, error) { return nil, errNoCameraHere }

func (d *videoDevice) Read(time.Duration) ([]byte, error) { return nil, errNoCameraHere }

func (d *videoDevice) Close() {}

func (d *videoDevice) Focus() (int, error) { return 0, errNoCameraHere }

func (d *videoDevice) SetFocus(int) error { return errNoCameraHere }
