//go:build windows

package main

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var cpuSample struct {
	sync.Mutex
	idle, kernel, user uint64
}

var getSystemTimes = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetSystemTimes")

func filetimeValue(t windows.Filetime) uint64 {
	return uint64(t.HighDateTime)<<32 | uint64(t.LowDateTime)
}

// SystemCPUPercent returns aggregate host CPU utilization since the previous
// sample. The first sample returns -1 because it has no interval yet.
func (a *App) SystemCPUPercent() float64 {
	var idle, kernel, user windows.Filetime
	// #nosec G103 -- GetSystemTimes requires three writable FILETIME pointers.
	ok, _, _ := getSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if ok == 0 {
		return -1
	}
	i, k, u := filetimeValue(idle), filetimeValue(kernel), filetimeValue(user)
	cpuSample.Lock()
	defer cpuSample.Unlock()
	if cpuSample.kernel == 0 && cpuSample.user == 0 {
		cpuSample.idle, cpuSample.kernel, cpuSample.user = i, k, u
		return -1
	}
	idleDelta, totalDelta := i-cpuSample.idle, (k-cpuSample.kernel)+(u-cpuSample.user)
	cpuSample.idle, cpuSample.kernel, cpuSample.user = i, k, u
	if totalDelta == 0 || idleDelta > totalDelta {
		return 0
	}
	return 100 * float64(totalDelta-idleDelta) / float64(totalDelta)
}
