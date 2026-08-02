// lifetime_test.go covers the temporary-channel lifetime knob (165).
package config

import (
	"os"
	"testing"
)

// cleanConfigDir moves the test into an empty directory so a repository
// config.yaml cannot influence the defaults under test.
func cleanConfigDir(t *testing.T) {
	t.Helper()
	// TempDir first: its own cleanup must be registered before the chdir-back
	// one, or LIFO cleanup tries to delete the directory we still sit in.
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
}

// TestChannelTempLifetimeDefault pins the default to 60 seconds, which is what
// channels.DefaultCleanupDelay is; a drift here silently changes how long
// abandoned temporary channels linger.
func TestChannelTempLifetimeDefault(t *testing.T) {
	cleanConfigDir(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ChannelTempLifetimeSeconds != 60 {
		t.Errorf("ChannelTempLifetimeSeconds = %d, want 60", cfg.ChannelTempLifetimeSeconds)
	}
}

// TestChannelTempLifetimeFromEnv verifies the knob is actually configurable,
// including the zero that means "use the default".
func TestChannelTempLifetimeFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"5", 5},
		{"0", 0},
		{"-1", -1},
	} {
		t.Run(tc.env, func(t *testing.T) {
			cleanConfigDir(t)
			t.Setenv("VOICX_CHANNEL_TEMP_LIFETIME_SECONDS", tc.env)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ChannelTempLifetimeSeconds != tc.want {
				t.Errorf("ChannelTempLifetimeSeconds = %d, want %d", cfg.ChannelTempLifetimeSeconds, tc.want)
			}
		})
	}
}
