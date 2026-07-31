package logging

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New constructs a zap.Logger configured for either development or production
// use. In dev mode it uses zap.NewDevelopment() (console-friendly, debug
// level, colored). In prod mode it uses zap.NewProduction() (JSON, info level)
// and applies the provided level string (e.g. "debug", "info", "warn",
// "error") if it can be parsed.
func New(devMode bool, level string) (*zap.Logger, error) {
	if devMode {
		return zap.NewDevelopment()
	}

	var lvl zapcore.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}

	prod := zap.NewProductionConfig()
	prod.Level = zap.NewAtomicLevelAt(lvl)
	logger, err := prod.Build()
	if err != nil {
		return nil, fmt.Errorf("building production logger: %w", err)
	}
	return logger, nil
}
