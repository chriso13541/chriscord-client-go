//go:build !windows && !linux

package main

import (
	"errors"
	"os/exec"
)

// No native capture on this system yet; the page's camera path is used.

func cameraInputFormat() string                    { return "" }
func captureProcAttr(cmd *exec.Cmd)                {}
func listCameraDevices() ([]CameraDevice, error)   { return nil, errors.New("not supported here") }
func listCameraModes(string) ([]CameraMode, error) { return nil, errors.New("not supported here") }
func cameraInputArgs(o CameraStart) []string       { return nil }
