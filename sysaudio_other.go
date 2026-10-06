//go:build !windows

package main

import "errors"

func startSystemAudio(pid uint32, exclude bool) error {
	return errors.New("sharing sound isn't available on this system yet")
}
func readSystemAudio(out []float32) int { return 0 }
func systemAudioAvailable() int        { return 0 }
func stopSystemAudio()                 {}
