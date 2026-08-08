package filetransfer

import (
	"strings"
	"testing"
)

func BenchmarkSanitizeName(b *testing.B) {
	for _, test := range []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "small_valid", input: "a.txt"},
		{name: "medium_valid_64B", input: strings.Repeat("a", 64)},
		{name: "maximum_segment_255B", input: strings.Repeat("a", 255)},
		{name: "invalid_traversal", input: "../outside.txt", wantErr: true},
		{name: "invalid_windows_path", input: `C:\Windows\win.ini`, wantErr: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.SetBytes(int64(len(test.input)))
			b.ReportAllocs()

			var (
				output string
				err    error
			)
			for b.Loop() {
				output, err = sanitizeName(test.input)
			}
			if test.wantErr {
				if err == nil {
					b.Fatalf("sanitizeName(%q) succeeded", test.input)
				}
				return
			}
			if err != nil || output != test.input {
				b.Fatalf("sanitizeName(%q) = (%q, %v)", test.input, output, err)
			}
		})
	}
}

func BenchmarkSanitizeFolder(b *testing.B) {
	segment := strings.Repeat("s", 16)
	for _, test := range []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "small_valid", input: "docs"},
		{name: "medium_valid_8_segments", input: strings.Repeat(segment+"/", 7) + segment},
		{name: "large_valid_32_segments", input: strings.Repeat(segment+"/", 31) + segment},
		{name: "invalid_traversal", input: "docs/../outside", wantErr: true},
		{name: "invalid_windows_separator", input: `docs\private`, wantErr: true},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.SetBytes(int64(len(test.input)))
			b.ReportAllocs()

			var (
				output string
				err    error
			)
			for b.Loop() {
				output, err = sanitizeFolder(test.input)
			}
			if test.wantErr {
				if err == nil {
					b.Fatalf("sanitizeFolder(%q) succeeded", test.input)
				}
				return
			}
			if err != nil || output != test.input {
				b.Fatalf("sanitizeFolder(%q) = (%q, %v)", test.input, output, err)
			}
		})
	}
}
