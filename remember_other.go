//go:build !windows && !linux

package main

import "errors"

// Staying signed in uses Windows' Data Protection API
// (remember_windows.go) or Linux's keyring (remember_linux.go); on macOS
// (the keychain) it isn't wired up yet, so the option is hidden and the
// passphrase is always asked for.

const rememberSupported = false

var errNoRemember = errors.New("not available on this system")

func osProtect(data, entropy []byte) ([]byte, error)   { return nil, errNoRemember }
func osUnprotect(data, entropy []byte) ([]byte, error) { return nil, errNoRemember }

func rememberDescription() string { return "" }
