package logging

import (
	"testing"
)

// TestNewDev verifies that New(true, "debug") returns a non-nil development
// logger that does not panic when Info is called.
func TestNewDev(t *testing.T) {
	logger, err := New(true, "debug")
	if err != nil {
		t.Fatalf("New(true, debug) returned error: %v", err)
	}
	if logger == nil {
		t.Fatal("New(true, debug) returned nil logger")
	}
	// Should not panic.
	logger.Info("dev logger info message")
	_ = logger.Sync()
}

// TestNewProd verifies that New(false, "info") returns a non-nil production
// logger.
func TestNewProd(t *testing.T) {
	logger, err := New(false, "info")
	if err != nil {
		t.Fatalf("New(false, info) returned error: %v", err)
	}
	if logger == nil {
		t.Fatal("New(false, info) returned nil logger")
	}
	logger.Info("prod logger info message")
	_ = logger.Sync()
}

// TestNewProdInvalidLevel verifies that New(false, "invalid-level") returns
// an error.
func TestNewProdInvalidLevel(t *testing.T) {
	logger, err := New(false, "invalid-level")
	if err == nil {
		t.Fatal("New(false, invalid-level) expected error, got nil")
	}
	if logger != nil {
		t.Errorf("New(false, invalid-level) expected nil logger, got %v", logger)
	}
}

// TestNewProdLevels is a table-driven test that verifies several valid log
// levels are accepted by the production logger constructor.
func TestNewProdLevels(t *testing.T) {
	levels := []string{"debug", "info", "warn", "error", "dpanic", "panic", "fatal"}
	for _, lvl := range levels {
		lvl := lvl
		t.Run(lvl, func(t *testing.T) {
			logger, err := New(false, lvl)
			if err != nil {
				t.Fatalf("New(false, %q) returned error: %v", lvl, err)
			}
			if logger == nil {
				t.Fatalf("New(false, %q) returned nil logger", lvl)
			}
			_ = logger.Sync()
		})
	}
}
