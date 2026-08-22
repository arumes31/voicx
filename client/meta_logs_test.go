package main

import (
	"archive/zip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

type fakeLogArchiveWriter struct {
	createErr error
	output    io.Writer
	closeErr  error
	closes    int
}

func (w *fakeLogArchiveWriter) Create(string) (io.Writer, error) {
	if w.createErr != nil {
		return nil, w.createErr
	}
	return w.output, nil
}

func (w *fakeLogArchiveWriter) Close() error {
	w.closes++
	return w.closeErr
}

type fakeLogEntry string

func (e fakeLogEntry) Name() string             { return string(e) }
func (fakeLogEntry) IsDir() bool                { return false }
func (fakeLogEntry) Type() fs.FileMode          { return 0 }
func (fakeLogEntry) Info() (fs.FileInfo, error) { return nil, errors.New("not used") }

type failingLogWriter struct{ err error }

func (w failingLogWriter) Write([]byte) (int, error) { return 0, w.err }

func TestExportLogsWritesEveryLogAndOnlyLogs(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open log root: %v", err)
	}
	if err := root.WriteFile("client.log", []byte("client contents"), 0o600); err != nil {
		t.Fatalf("write client log: %v", err)
	}
	if err := root.WriteFile("chat.log", []byte("chat contents"), 0o600); err != nil {
		t.Fatalf("write chat log: %v", err)
	}
	if err := root.WriteFile("ignore.txt", []byte("not a log"), 0o600); err != nil {
		t.Fatalf("write ignored file: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "logs.zip")
	if err := exportLogsTo(root, dest); err != nil {
		t.Fatalf("export logs: %v", err)
	}
	archive, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatalf("open exported zip: %v", err)
	}
	defer func() { _ = archive.Close() }()
	got := map[string]string{}
	for _, entry := range archive.File {
		r, err := entry.Open()
		if err != nil {
			t.Fatalf("open zip entry %q: %v", entry.Name, err)
		}
		buf, err := io.ReadAll(r)
		if err != nil {
			_ = r.Close()
			t.Fatalf("read zip entry %q: %v", entry.Name, err)
		}
		_ = r.Close()
		got[entry.Name] = string(buf)
	}
	if len(got) != 2 || got["client.log"] != "client contents" || got["chat.log"] != "chat contents" {
		t.Fatalf("zip logs = %#v", got)
	}
}

func TestExportLogsReturnsDirectoryReadError(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("open log root: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close log root: %v", err)
	}
	if err := exportLogsTo(root, filepath.Join(t.TempDir(), "logs.zip")); err == nil {
		t.Fatal("export accepted a closed/unreadable log root")
	}
}

func TestExportLogsClosesArchiveAndJoinsEveryFailure(t *testing.T) {
	type stage struct {
		name     string
		readDir  bool
		readFile bool
		create   bool
		write    bool
	}
	for _, test := range []stage{
		{name: "read directory", readDir: true},
		{name: "read file", readFile: true},
		{name: "create zip entry", create: true},
		{name: "write zip entry", write: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			originalNew := newLogArchiveWriter
			originalEntries := readLogEntries
			originalFile := readLogFile
			t.Cleanup(func() {
				newLogArchiveWriter = originalNew
				readLogEntries = originalEntries
				readLogFile = originalFile
			})
			closeErr := errors.New("zip close failure")
			stageErr := errors.New("injected " + test.name + " failure")
			archive := &fakeLogArchiveWriter{output: io.Discard, closeErr: closeErr}
			if test.create {
				archive.createErr = stageErr
			}
			if test.write {
				archive.output = failingLogWriter{err: stageErr}
			}
			newLogArchiveWriter = func(io.Writer) logArchiveWriter { return archive }
			if test.readDir {
				readLogEntries = func(*os.Root) ([]fs.DirEntry, error) { return nil, stageErr }
			} else {
				readLogEntries = func(*os.Root) ([]fs.DirEntry, error) { return []fs.DirEntry{fakeLogEntry("client.log")}, nil }
			}
			if test.readFile {
				readLogFile = func(*os.Root, string) ([]byte, error) { return nil, stageErr }
			} else {
				readLogFile = func(*os.Root, string) ([]byte, error) { return []byte("log"), nil }
			}
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			err = exportLogsTo(root, filepath.Join(t.TempDir(), "logs.zip"))
			if !errors.Is(err, stageErr) || !errors.Is(err, closeErr) {
				t.Fatalf("error = %v, want joined stage and close failures", err)
			}
			if archive.closes != 1 {
				t.Fatalf("zip close count = %d, want exactly one", archive.closes)
			}
		})
	}

	t.Run("zip close failure is not success", func(t *testing.T) {
		originalNew := newLogArchiveWriter
		originalEntries := readLogEntries
		originalFile := readLogFile
		t.Cleanup(func() {
			newLogArchiveWriter = originalNew
			readLogEntries = originalEntries
			readLogFile = originalFile
		})
		closeErr := errors.New("zip close failure")
		archive := &fakeLogArchiveWriter{output: io.Discard, closeErr: closeErr}
		newLogArchiveWriter = func(io.Writer) logArchiveWriter { return archive }
		readLogEntries = func(*os.Root) ([]fs.DirEntry, error) { return nil, nil }
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		err = exportLogsTo(root, filepath.Join(t.TempDir(), "logs.zip"))
		if !errors.Is(err, closeErr) {
			t.Fatalf("zip close failure was lost: %v", err)
		}
		if archive.closes != 1 {
			t.Fatalf("zip close count = %d, want exactly one", archive.closes)
		}
	})
}
