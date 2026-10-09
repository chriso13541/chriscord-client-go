package main

import "syscall"

// allowOtherInstanceToFocus lets the copy of Chriscord that's already
// running bring its window to the front. Windows only lets a program take
// the foreground if the one the user just started allows it, and this is
// that program (it hands over to the running copy and exits a moment
// later). Without this the running copy would only flash on the taskbar.
func allowOtherInstanceToFocus() {
	const asfwAny = ^uintptr(0) // ASFW_ANY
	syscall.NewLazyDLL("user32.dll").NewProc("AllowSetForegroundWindow").Call(asfwAny)
}
