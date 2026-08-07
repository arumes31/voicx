package main

import (
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"voicx/internal/config"
)

func TestStartServiceReportsEveryExit(t *testing.T) {
	wantErr := errors.New("listener failed")
	exits := make(chan serviceExit, 2)
	startService(exits, "failed service", func() error { return wantErr })
	startService(exits, "clean service", func() error { return nil })

	got := make(map[string]error, 2)
	for range 2 {
		select {
		case exit := <-exits:
			got[exit.name] = exit.err
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for service exit")
		}
	}
	if !errors.Is(got["failed service"], wantErr) {
		t.Fatalf("failed service error = %v, want %v", got["failed service"], wantErr)
	}
	if err, ok := got["clean service"]; !ok || err != nil {
		t.Fatalf("clean service exit = %v, present = %t", err, ok)
	}
}

func TestUnexpectedServiceExitPreservesCause(t *testing.T) {
	cause := errors.New("bind failed")
	err := unexpectedServiceExit(serviceExit{name: "TCP", err: cause})
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "TCP exited unexpectedly") {
		t.Fatalf("unexpectedServiceExit() = %v", err)
	}

	err = unexpectedServiceExit(serviceExit{name: "health"})
	if err == nil || !strings.Contains(err.Error(), "health exited unexpectedly") {
		t.Fatalf("nil-error service exit = %v", err)
	}
}

func TestJoinShutdownError(t *testing.T) {
	prior := errors.New("run failed")
	if got := joinShutdownError(prior, "TCP", net.ErrClosed); !errors.Is(got, prior) {
		t.Fatalf("net.ErrClosed changed prior error: %v", got)
	}

	closeErr := errors.New("close failed")
	got := joinShutdownError(prior, "UDP", closeErr)
	if !errors.Is(got, prior) || !errors.Is(got, closeErr) {
		t.Fatalf("joined shutdown error = %v", got)
	}
	if !strings.Contains(got.Error(), "shutting down UDP") {
		t.Fatalf("joined shutdown error lacks service context: %v", got)
	}
}

func TestRecorderConfigWiring(t *testing.T) {
	input := config.RecordingConfig{
		Enabled:         true,
		Dir:             "recordings-private",
		FFmpegPath:      "ffmpeg-custom",
		Format:          "mp4",
		VideoArgs:       []string{"-c:v", "copy"},
		AudioArgs:       []string{"-c:a", "aac"},
		MaxConcurrent:   9,
		WindowsACLReady: true,
	}
	got := recorderConfig(input)
	if !got.Enabled || got.Dir != input.Dir || got.FFmpegPath != input.FFmpegPath ||
		got.Format != input.Format || got.MaxConcurrent != input.MaxConcurrent ||
		got.WindowsACLReady != input.WindowsACLReady ||
		!reflect.DeepEqual(got.VideoArgs, input.VideoArgs) || !reflect.DeepEqual(got.AudioArgs, input.AudioArgs) {
		t.Fatalf("recorderConfig() = %+v, want all application recording fields", got)
	}
	input.VideoArgs[1] = "mutated"
	input.AudioArgs[1] = "mutated"
	if got.VideoArgs[1] != "copy" || got.AudioArgs[1] != "aac" {
		t.Fatal("recorderConfig retained mutable application argument slices")
	}
}
