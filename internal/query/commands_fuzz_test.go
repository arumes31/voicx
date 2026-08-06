package query

import (
	"reflect"
	"strings"
	"testing"
)

func FuzzParseCommand(f *testing.F) {
	for _, line := range []string{
		"",
		"   \t\r\n",
		`LOGIN cHZVQTN4VW91Y0k9LzgrST0= secret\spw`,
		`sendtextmessage targetmode=2 target=3 msg=hello\sworld\p`,
		`command duplicate=first duplicate=last positional`,
		`command empty= trailing\`,
		"ÜBER key=Grüße",
		string([]byte{'c', 'm', 'd', ' ', 0xff, '=', 0xfe}),
	} {
		f.Add(line)
	}

	f.Fuzz(func(t *testing.T, line string) {
		if len(line) > maxFuzzQueryInput {
			t.Skip()
		}

		got := parseCommand(line)
		if again := parseCommand(line); !reflect.DeepEqual(got, again) {
			t.Fatalf("parseCommand is not deterministic: %#v != %#v", got, again)
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			if got.name != "" || len(got.args) != 0 || len(got.positional) != 0 {
				t.Fatalf("parseCommand(whitespace) = %#v, want empty command", got)
			}
			return
		}
		if want := strings.ToLower(fields[0]); got.name != want {
			t.Fatalf("command name = %q, want %q", got.name, want)
		}
		if len(got.args)+len(got.positional) > len(fields)-1 {
			t.Fatalf("parsed %d arguments from %d tokens", len(got.args)+len(got.positional), len(fields)-1)
		}

		totalOutput := 0
		for key, value := range got.args {
			totalOutput += len(key) + len(value)
		}
		for _, value := range got.positional {
			totalOutput += len(value)
		}
		if totalOutput > len(line) {
			t.Fatalf("parsed argument data grew from %d bytes to %d bytes", len(line), totalOutput)
		}

		if got.name == "login" {
			if len(got.args) != 0 || len(got.positional) != len(fields)-1 {
				t.Fatalf("login parse = %#v, want only %d positional arguments", got, len(fields)-1)
			}
			for i, field := range fields[1:] {
				if want := unescape(field); got.positional[i] != want {
					t.Fatalf("login positional[%d] = %q, want %q", i, got.positional[i], want)
				}
			}
		}
	})
}
