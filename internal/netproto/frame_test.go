package netproto

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestReadFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		wire        []byte
		want        *Frame
		wantErr     error
		wantErrText string
	}{
		{
			name: "payload",
			wire: []byte{0, 0, 0, 5, 0x12, 0x34, 'a', 'b', 'c'},
			want: &Frame{Type: 0x1234, Payload: []byte("abc")},
		},
		{
			name: "empty payload",
			wire: []byte{0, 0, 0, 2, 0xab, 0xcd},
			want: &Frame{Type: 0xabcd, Payload: []byte{}},
		},
		{
			name:    "truncated length prefix",
			wire:    []byte{0, 0},
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:        "length omits message type",
			wire:        []byte{0, 0, 0, 1},
			wantErrText: "invalid frame length 1",
		},
		{
			name:    "announced frame too large",
			wire:    []byte{0, 0x10, 0, 1},
			wantErr: ErrFrameTooLarge,
		},
		{
			name:    "truncated body",
			wire:    []byte{0, 0, 0, 5, 0x12, 0x34, 'a'},
			wantErr: io.ErrUnexpectedEOF,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := ReadFrame(bytes.NewReader(test.wire))
			switch {
			case test.wantErr != nil && !errors.Is(err, test.wantErr):
				t.Fatalf("ReadFrame() error = %v, want %v", err, test.wantErr)
			case test.wantErrText != "" && (err == nil || !strings.Contains(err.Error(), test.wantErrText)):
				t.Fatalf("ReadFrame() error = %v, want text %q", err, test.wantErrText)
			case test.wantErr == nil && test.wantErrText == "" && err != nil:
				t.Fatalf("ReadFrame() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("ReadFrame() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestWriteFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		frame   *Frame
		want    []byte
		wantErr error
	}{
		{
			name:  "payload",
			frame: &Frame{Type: 0x1234, Payload: []byte("abc")},
			want:  []byte{0, 0, 0, 5, 0x12, 0x34, 'a', 'b', 'c'},
		},
		{
			name:  "empty payload",
			frame: &Frame{Type: 0xabcd},
			want:  []byte{0, 0, 0, 2, 0xab, 0xcd},
		},
		{
			name:    "nil frame",
			wantErr: errors.New("netproto: nil frame"),
		},
		{
			name:    "payload too large",
			frame:   &Frame{Payload: make([]byte, MaxPayloadSize+1)},
			wantErr: ErrFrameTooLarge,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var dst bytes.Buffer
			err := WriteFrame(&dst, test.frame)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) && (err == nil || err.Error() != test.wantErr.Error()) {
					t.Fatalf("WriteFrame() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("WriteFrame() error = %v", err)
			}
			if got := dst.Bytes(); !bytes.Equal(got, test.want) {
				t.Errorf("WriteFrame() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWriteFramePropagatesWriterErrors(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("write failed")
	tests := []struct {
		name       string
		failOnCall int
		wantText   string
	}{
		{name: "header", failOnCall: 1, wantText: "writing header"},
		{name: "payload", failOnCall: 2, wantText: "writing payload"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			writer := &failOnWriteCall{call: test.failOnCall, err: sentinel}
			err := WriteFrame(writer, &Frame{Type: 1, Payload: []byte("payload")})
			if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("WriteFrame() error = %v, want wrapped %v with %q", err, sentinel, test.wantText)
			}
		})
	}
}

type failOnWriteCall struct {
	writes int
	call   int
	err    error
}

func (w *failOnWriteCall) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.call {
		return 0, w.err
	}
	return len(p), nil
}
