//go:build !windows

// keyprotect_other.go is the non-Windows side of hardware-backed key storage
// (354): there is no DPAPI equivalent wired up yet, so the client falls back
// to the plaintext key file rather than refusing to run.
package main

import "errors"

// keyProtectionAvailable reports that no OS key protection is wired up here.
func keyProtectionAvailable() bool { return false }

// keyProtectionName identifies the protection used by files that can be read
// on Windows but not on this platform.
const keyProtectionName = "Windows DPAPI"

// protectBytes is unsupported off Windows.
func protectBytes(_ []byte) ([]byte, error) {
	return nil, errors.New("OS key protection is not available on this platform")
}

// unprotectBytes is unsupported off Windows, so a protected file copied here
// reports why instead of looking corrupt.
func unprotectBytes(_ []byte) ([]byte, error) {
	return nil, errors.New("this identity is protected by Windows DPAPI and cannot be unlocked on this platform")
}
