package query

import "testing"

// TestEscapeUnescapeRoundTrip verifies escape/unescape are inverse for the
// special characters and for ordinary text.
func TestEscapeUnescapeRoundTrip(t *testing.T) {
	cases := []string{
		"",
		"plain",
		"with space",
		"pipe|char",
		"slash/char",
		"back\\slash",
		"new\nline\rcarriage\ttab",
		"all of\\them|mixed /up\n",
	}
	for _, in := range cases {
		if got := unescape(escape(in)); got != in {
			t.Errorf("round-trip of %q = %q", in, got)
		}
	}
}

// TestEscape verifies the exact escape sequences.
func TestEscape(t *testing.T) {
	cases := map[string]string{
		"a b":      `a\sb`,
		"a|b":      `a\pb`,
		"a/b":      `a\/b`,
		"a\\b":     `a\\b`,
		"a\nb":     `a\nb`,
		"a\rb":     `a\rb`,
		"a\tb":     `a\tb`,
		" spaced ": `\sspaced\s`,
	}
	for in, want := range cases {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUnescape verifies decoding, including unknown escapes and a trailing
// backslash.
func TestUnescape(t *testing.T) {
	cases := map[string]string{
		`a\sb`:      "a b",
		`a\pb`:      "a|b",
		`a\/b`:      "a/b",
		`a\\b`:      `a\b`,
		`a\nb`:      "a\nb",
		`a\zb`:      "azb", // unknown escape: keeps the character
		`trailing\`: `trailing\`,
		`\\s`:       `\s`, // escaped backslash followed by 's'
	}
	for in, want := range cases {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseCommand verifies command-line parsing with key=value pairs,
// positional args, and escaped values.
func TestParseCommand(t *testing.T) {
	cmd := parseCommand(`LOGIN admin-uid secret\spw`)
	if cmd.name != "login" {
		t.Errorf("name = %q, want login", cmd.name)
	}
	if len(cmd.positional) != 2 || cmd.positional[0] != "admin-uid" || cmd.positional[1] != "secret pw" {
		t.Errorf("positional = %v", cmd.positional)
	}

	// BUG-4: a base64 unique ID containing +, /, and trailing = must parse as
	// two positional args, not a key=value pair.
	uid := "cHZVQTN4VW91Y0k9LzgrST0="
	cmd = parseCommand("login " + uid + " hunter2")
	if len(cmd.positional) != 2 || cmd.positional[0] != uid || cmd.positional[1] != "hunter2" {
		t.Errorf("base64 login positional = %v, want [%s hunter2]", cmd.positional, uid)
	}

	cmd = parseCommand(`sendtextmessage targetmode=2 target=3 msg=hello\sworld|`)
	if cmd.name != "sendtextmessage" {
		t.Errorf("name = %q", cmd.name)
	}
	if cmd.args["targetmode"] != "2" || cmd.args["target"] != "3" || cmd.args["msg"] != "hello world|" {
		t.Errorf("args = %v", cmd.args)
	}
}

// TestErrorLine verifies the status line format and escaping.
func TestErrorLine(t *testing.T) {
	if got := errorLine(0, "ok"); got != "error id=0 msg=ok\n" {
		t.Errorf("errorLine(0) = %q", got)
	}
	if got := errorLine(2568, "not logged in"); got != `error id=2568 msg=not\slogged\sin`+"\n" {
		t.Errorf("errorLine(2568) = %q", got)
	}
}
