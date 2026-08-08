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

	"voicx/internal/safecast"
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
	inputSize, err := safecast.IntToUint32(len(in))
	if err != nil {
		return nil, fmt.Errorf("input is too large: %w", err)
	}
	inBlob := windows.DataBlob{Size: inputSize, Data: &in[0]}
	var outBlob windows.DataBlob
	r, _, callErr := proc.Call(
		uintptr(unsafe.Pointer(&inBlob)), // #nosec G103 -- CryptProtectData requires a DATA_BLOB pointer.
		0,                                // szDataDescr / ppszDataDescr
		0,                                // pOptionalEntropy
		0,                                // pvReserved
		0,                                // pPromptStruct
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&outBlob)), // #nosec G103 -- CryptProtectData writes a DATA_BLOB through this pointer.
	)
	if r == 0 {
		return nil, fmt.Errorf("%s: %w", proc.Name, callErr)
	}
	defer func() {
		// #nosec G103 -- DPAPI allocates outBlob.Data with LocalAlloc; LocalFree is the matching release API.
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(outBlob.Data)))
	}()
	// The blob lives in LocalAlloc memory freed above, so it has to be copied.
	// #nosec G103 -- DPAPI returned outBlob.Size bytes at outBlob.Data; the copy completes before LocalFree.
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
