//go:build !windows

package main

// SystemCPUPercent is unavailable on non-Windows client builds.
func (a *App) SystemCPUPercent() float64 { return -1 }
