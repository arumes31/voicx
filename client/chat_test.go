// chat_test.go covers the wave-5b client glue: attachment upload/download
// (live, against the Docker stack) and misc units.
package main

import (
	"encoding/base64"
	"testing"
	"time"
)

// TestLiveUploadDownload round-trips a file through the file-transfer glue:
// UploadFile (init + chunks + digest) then DownloadFile with digest
// verification.
func TestLiveUploadDownload(t *testing.T) {
	addr := liveAddr(t)
	channelID := ensureLiveChannel(t)

	alice, _ := newTestBackend(t)
	if err := alice.connect(addr, liveAliceUID, liveAlicePass, ""); err != "" {
		t.Fatalf("alice connect: %s", err)
	}
	defer alice.disconnect()
	app := appWithCM(alice)

	payload := []byte("wave5b attachment round-trip — 0123456789")
	name := "live-upload-test.txt"
	if err := app.UploadFile(channelID, name, base64.StdEncoding.EncodeToString(payload)); err != "" {
		t.Fatalf("UploadFile: %s", err)
	}

	// The server needs a moment to record the file before the download init.
	deadline := time.Now().Add(5 * time.Second)
	var gotB64 string
	var err error
	for {
		gotB64, err = app.DownloadFile(channelID, name)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("DownloadFile: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	got, err := base64.StdEncoding.DecodeString(gotB64)
	if err != nil {
		t.Fatalf("decode download: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("round-trip mismatch: got %q", got)
	}
}
