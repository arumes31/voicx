// ringbuf.go implements an in-memory ring buffer of recent log entries
// (wave 10a, 223 logview): a zapcore.Core wrapper teeing formatted log lines
// into a bounded slice, exposed to the ServerQuery backend.
package logging

import (
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// ringCapacity bounds the buffered lines.
const ringCapacity = 500

// ringBuf is a concurrency-safe bounded line buffer.
type ringBuf struct {
	mu    sync.Mutex
	lines []string
}

// ringCore is a zapcore.Core that encodes entries to lines and appends them
// to the ring. Everything else (level filtering, output) stays with the
// primary core via zapcore.NewTee.
type ringCore struct {
	zapcore.Core
	buf *ringBuf
	enc zapcore.Encoder
}

var globalRing = &ringBuf{}

// Tee returns a zap.Option that tees every log entry into the global
// ring buffer (formatted with the same console encoder style).
func Tee() zap.Option {
	encCfg := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
	return zap.WrapCore(func(core zapcore.Core) zapcore.Core {
		return &ringCore{Core: core, buf: globalRing, enc: zapcore.NewConsoleEncoder(encCfg)}
	})
}

// Check implements zapcore.Core.
func (c *ringCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		ce = ce.AddCore(ent, c)
	}
	return c.Core.Check(ent, ce)
}

// Write implements zapcore.Core.
func (c *ringCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	if buf, err := c.enc.EncodeEntry(ent, fields); err == nil {
		line := strings.TrimRight(buf.String(), "\n")
		c.buf.mu.Lock()
		c.buf.lines = append(c.buf.lines, line)
		if len(c.buf.lines) > ringCapacity {
			c.buf.lines = c.buf.lines[len(c.buf.lines)-ringCapacity:]
		}
		c.buf.mu.Unlock()
	}
	return c.Core.Write(ent, fields)
}

// Recent returns the last n lines, newest last, optionally filtered to lines
// containing filter (case-insensitive; "" = all).
func Recent(n int, filter string) []string {
	globalRing.mu.Lock()
	defer globalRing.mu.Unlock()
	if n <= 0 {
		n = 50
	}
	filter = strings.ToLower(filter)
	var out []string
	for i := len(globalRing.lines) - 1; i >= 0 && len(out) < n; i-- {
		line := globalRing.lines[i]
		if filter != "" && !strings.Contains(strings.ToLower(line), filter) {
			continue
		}
		out = append(out, line)
	}
	// Reverse to chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
