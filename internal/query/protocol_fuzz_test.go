package query

import "testing"

const maxFuzzQueryInput = 64 << 10

func FuzzEscapeUnescape(f *testing.F) {
	for _, input := range []string{
		"",
		"plain",
		"all of\\them|mixed /up\n\r\t",
		`unknown\zescape\`,
		"nul\x00byte",
		"Grüße 世界",
		string([]byte{0xff, 0xfe, '\\', 's'}),
	} {
		f.Add(input)
	}

	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > maxFuzzQueryInput {
			t.Skip()
		}

		encoded := escape(input)
		if len(encoded) > 2*len(input) {
			t.Fatalf("escape expanded %d bytes to %d bytes", len(input), len(encoded))
		}
		if got := unescape(encoded); got != input {
			t.Fatalf("unescape(escape(%q)) = %q", input, got)
		}

		// unescape is also called directly on untrusted query tokens. It may
		// remove escape markers, but it must never expand the input.
		if got := unescape(input); len(got) > len(input) {
			t.Fatalf("unescape expanded %d bytes to %d bytes", len(input), len(got))
		}
	})
}
