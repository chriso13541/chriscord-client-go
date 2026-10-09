package main

import (
	"syscall"
	"unsafe"
)

// Windows' Data Protection API: encrypts data with a key derived from the
// current Windows user's sign-in, so only that user on this computer can
// decrypt it. No dialog is ever shown.

const rememberSupported = true

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(b []byte) *dataBlob {
	if len(b) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

func (b *dataBlob) bytes() []byte {
	out := make([]byte, b.cbData)
	if b.cbData > 0 {
		copy(out, unsafe.Slice(b.pbData, b.cbData))
	}
	return out
}

const cryptprotectUIForbidden = 0x1

func osProtect(data, entropy []byte) ([]byte, error) {
	var out dataBlob
	r, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(newBlob(data))), 0,
		uintptr(unsafe.Pointer(newBlob(entropy))), 0, 0,
		cryptprotectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}

func osUnprotect(data, entropy []byte) ([]byte, error) {
	var out dataBlob
	r, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(newBlob(data))), 0,
		uintptr(unsafe.Pointer(newBlob(entropy))), 0, 0,
		cryptprotectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	plain := out.bytes()
	if out.cbData > 0 { // wipe Windows' copy before it's freed
		buf := unsafe.Slice(out.pbData, out.cbData)
		for i := range buf {
			buf[i] = 0
		}
	}
	return plain, nil
}
