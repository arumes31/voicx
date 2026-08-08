package main

import (
	"fmt"
	"io"
	"io/fs"
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
	baseName   string
	root       *os.Root
	file       *os.File
	day        string
	maxBytes   int64
	sincePrune int64
	now        func() time.Time
}

func newDailyLogWriter(dir, baseName string) (*dailyLogWriter, error) {
	if !filepath.IsLocal(baseName) || filepath.Base(baseName) != baseName {
		return nil, fmt.Errorf("invalid log file name %q", baseName)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	w := &dailyLogWriter{
		baseName: baseName, root: root,
		maxBytes: maxLogDirectoryBytes, now: time.Now,
	}
	if err := w.open(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return w, nil
}

func (w *dailyLogWriter) open() error {
	now := w.now()
	w.day = now.Format("20060102")
	if info, err := w.root.Stat(w.baseName); err == nil && info.ModTime().Format("20060102") != w.day {
		stem := strings.TrimSuffix(w.baseName, filepath.Ext(w.baseName))
		rotated := fmt.Sprintf("%s-%s%s", stem, info.ModTime().Format("20060102-150405"), filepath.Ext(w.baseName))
		_ = w.root.Rename(w.baseName, rotated)
	}
	f, err := w.root.OpenFile(w.baseName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
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
	if w.file == nil || w.root == nil {
		return 0, os.ErrClosed
	}
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
		w.file = nil
	}
	stem := strings.TrimSuffix(w.baseName, filepath.Ext(w.baseName))
	rotated := fmt.Sprintf("%s-%s%s", stem, w.day, filepath.Ext(w.baseName))
	if _, err := w.root.Stat(rotated); err == nil {
		rotated = fmt.Sprintf("%s-%s-%d%s", stem, w.day, w.now().Unix(), filepath.Ext(w.baseName))
	}
	_ = w.root.Rename(w.baseName, rotated)
	return w.open()
}

type logFile struct {
	name string
	size int64
	mod  time.Time
}

func (w *dailyLogWriter) prune() error {
	entries, err := fs.ReadDir(w.root.FS(), ".")
	if err != nil {
		return err
	}
	var files []logFile
	var total int64
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
		total += info.Size()
		if name != w.baseName {
			files = append(files, logFile{name: name, size: info.Size(), mod: info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, file := range files {
		if total <= w.maxBytes {
			break
		}
		if err := w.root.Remove(file.name); err == nil {
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
	var closeErr error
	if w.file != nil {
		closeErr = w.file.Close()
		w.file = nil
	}
	if w.root != nil {
		if err := w.root.Close(); closeErr == nil {
			closeErr = err
		}
		w.root = nil
	}
	return closeErr
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
