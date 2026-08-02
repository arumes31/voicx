package main

import "testing"

func TestHotkeyRegStopIsIdempotent(t *testing.T) {
	cancel := make(chan struct{})
	reg := &hotkeyReg{cancel: cancel}

	reg.stop()
	reg.stop()

	select {
	case <-cancel:
	default:
		t.Fatal("stop did not close the cancellation channel")
	}
}
