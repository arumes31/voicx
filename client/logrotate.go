package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxLogDirectoryBytes int64 = 50 << 20
	pruneCheckBytes      int64 = 1 << 20
)

type dailyLogWriter struct {
	mu         sync.Mutex
	dir        string
	baseName   string
	file       *os.File
	day        string
	maxBytes   int64
	sincePrune int64
	now        func() time.Time
}

func newDailyLogWriter(dir, baseName string) (*dailyLogWriter, error) {
	w := &dailyLogWriter{dir: dir, baseName: baseName, maxBytes: maxLogDirectoryBytes, now: time.Now}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *dailyLogWriter) open() error {
	if err := os.MkdirAll(w.dir, 0o750); err != nil {
		return err
	}
	now := w.now()
	w.day = now.Format("20060102")
	path := filepath.Join(w.dir, w.baseName)
	if info, err := os.Stat(path); err == nil && info.ModTime().Format("20060102") != w.day {
		stem := strings.TrimSuffix(w.baseName, filepath.Ext(w.baseName))
		rotated := filepath.Join(w.dir, fmt.Sprintf("%s-%s%s", stem, info.ModTime().Format("20060102-150405"), filepath.Ext(w.baseName)))
		_ = os.Rename(path, rotated)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.file = f
	// Retention cleanup must not make an otherwise usable log writer fail.
	// Rotation and the periodic write threshold will retry pruning later.
	_ = w.prune()
	return nil
}

func (w *dailyLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.now().Format("20060102") != w.day {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	if err == nil {
		w.sincePrune += int64(n)
	}
	if err == nil && w.sincePrune >= pruneCheckBytes {
		w.sincePrune = 0
		_ = w.prune()
	}
	return n, err
}

func (w *dailyLogWriter) rotate() error {
	if w.file != nil {
		_ = w.file.Close()
	}
	path := filepath.Join(w.dir, w.baseName)
	stem := strings.TrimSuffix(w.baseName, filepath.Ext(w.baseName))
	rotated := filepath.Join(w.dir, fmt.Sprintf("%s-%s%s", stem, w.day, filepath.Ext(w.baseName)))
	if _, err := os.Stat(rotated); err == nil {
		rotated = filepath.Join(w.dir, fmt.Sprintf("%s-%s-%d%s", stem, w.day, w.now().Unix(), filepath.Ext(w.baseName)))
	}
	_ = os.Rename(path, rotated)
	return w.open()
}

type logFile struct {
	path string
	size int64
	mod  time.Time
}

func (w *dailyLogWriter) prune() error {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return err
	}
	var files []logFile
	var total int64
	active := filepath.Join(w.dir, w.baseName)
	ext := filepath.Ext(w.baseName)
	stem := strings.TrimSuffix(w.baseName, ext)
	for _, entry := range entries {
		name := entry.Name()
		owned := name == w.baseName || isRotatedLogName(name, stem, ext)
		if entry.IsDir() || !owned {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(w.dir, entry.Name())
		total += info.Size()
		if path != active {
			files = append(files, logFile{path: path, size: info.Size(), mod: info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, file := range files {
		if total <= w.maxBytes {
			break
		}
		if err := os.Remove(file.path); err == nil {
			total -= file.size
		}
	}
	return nil
}

func isRotatedLogName(name, stem, ext string) bool {
	if !strings.HasPrefix(name, stem+"-") || !strings.HasSuffix(name, ext) {
		return false
	}
	suffix := strings.TrimSuffix(strings.TrimPrefix(name, stem+"-"), ext)
	parts := strings.Split(suffix, "-")
	if len(parts) < 1 || len(parts) > 2 || len(parts[0]) != 8 || !allDigits(parts[0]) {
		return false
	}
	return len(parts) == 1 || (parts[1] != "" && allDigits(parts[1]))
}

func allDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (w *dailyLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}

var (
	chatLogMu      sync.Mutex
	chatLogWriters = map[string]*dailyLogWriter{}
)

func appendDailyLog(dir, name, line string) {
	chatLogMu.Lock()
	defer chatLogMu.Unlock()
	key := filepath.Clean(dir) + "\x00" + name
	w := chatLogWriters[key]
	if w == nil {
		var err error
		w, err = newDailyLogWriter(dir, name)
		if err != nil {
			return
		}
		chatLogWriters[key] = w
	}
	_, _ = io.WriteString(w, line)
}

func closeDailyLogs() {
	chatLogMu.Lock()
	defer chatLogMu.Unlock()
	for key, writer := range chatLogWriters {
		_ = writer.Close()
		delete(chatLogWriters, key)
	}
}
