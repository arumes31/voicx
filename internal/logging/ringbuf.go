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

// followBuffer is the per-follower queue depth. A follower that falls behind
// loses lines rather than slowing the logger down.
const followBuffer = 256

// ringBuf is a concurrency-safe bounded line buffer with optional live
// followers (223 `logview follow`).
type ringBuf struct {
	mu        sync.Mutex
	lines     []string
	nextSubID uint64
	subs      map[uint64]chan string
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
		for _, sub := range c.buf.subs {
			// Non-blocking: a stalled follower must never block a log write.
			select {
			case sub <- line:
			default:
			}
		}
		c.buf.mu.Unlock()
	}
	return c.Core.Write(ent, fields)
}

// Follow returns a channel of log lines emitted from now on, and a cancel
// function that unregisters it (223). The channel is never closed by the
// logger; the caller stops by calling cancel.
func Follow() (<-chan string, func()) {
	ch := make(chan string, followBuffer)
	globalRing.mu.Lock()
	if globalRing.subs == nil {
		globalRing.subs = make(map[uint64]chan string)
	}
	globalRing.nextSubID++
	id := globalRing.nextSubID
	globalRing.subs[id] = ch
	globalRing.mu.Unlock()
	return ch, func() {
		globalRing.mu.Lock()
		delete(globalRing.subs, id)
		globalRing.mu.Unlock()
	}
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
