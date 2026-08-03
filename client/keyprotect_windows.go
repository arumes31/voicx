//go:build windows

// keyprotect_windows.go binds the Windows DPAPI (CryptProtectData /
// CryptUnprotectData) used to protect the identity's private key at rest
// (354). The blob is bound to the current Windows user on this machine, so a
// protected file copied elsewhere is unreadable by design.
package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	crypt32                = windows.NewLazySystemDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
)

// cryptProtectUIForbidden fails instead of prompting: this runs on the save
// path of a background write, where a modal would hang the client.
const cryptProtectUIForbidden = 0x1

// keyProtectionAvailable reports whether DPAPI can be called at all. A false
// here selects the plaintext fallback rather than refusing to start (354).
func keyProtectionAvailable() bool {
	return crypt32.Load() == nil &&
		procCryptProtectData.Find() == nil && procCryptUnprotectData.Find() == nil
}

// keyProtectionName is what the UI shows for this platform's protected mode.
const keyProtectionName = "Windows DPAPI (user account)"

// callCrypt runs one of the two DPAPI entry points, which share a signature.
func callCrypt(proc *windows.LazyProc, in []byte) ([]byte, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("empty input")
	}
	inBlob := windows.DataBlob{Size: uint32(len(in)), Data: &in[0]}
	var outBlob windows.DataBlob
	r, _, err := proc.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, // szDataDescr / ppszDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, fmt.Errorf("%s: %w", proc.Name, err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(outBlob.Data)))
	// The blob lives in LocalAlloc memory freed above, so it has to be copied.
	return append([]byte(nil), unsafe.Slice(outBlob.Data, outBlob.Size)...), nil
}

// protectBytes encrypts plain for the current Windows user.
func protectBytes(plain []byte) ([]byte, error) {
	return callCrypt(procCryptProtectData, plain)
}

// unprotectBytes decrypts a blob produced by protectBytes. It fails for a
// blob made by another user or on another machine — the caller turns that
// into a readable message instead of losing the identity (354).
func unprotectBytes(blob []byte) ([]byte, error) {
	return callCrypt(procCryptUnprotectData, blob)
}
