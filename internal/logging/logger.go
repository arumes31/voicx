package logging

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New constructs a zap.Logger configured for either development or production
// use. Development mode is console-friendly and colored; production mode is
// structured JSON. Both modes honor the validated level argument.
func New(devMode bool, level string) (*zap.Logger, error) {
	var lvl zapcore.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}

	config := zap.NewProductionConfig()
	mode := "production"
	if devMode {
		config = zap.NewDevelopmentConfig()
		mode = "development"
	}
	config.Level = zap.NewAtomicLevelAt(lvl)
	logger, err := config.Build()
	if err != nil {
		return nil, fmt.Errorf("building %s logger: %w", mode, err)
	}
	return logger, nil
}
