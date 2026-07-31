// protocol.go implements the TS3-style text encoding of the ServerQuery
// protocol: value escaping and the error-line format.
package query

import (
	"fmt"
	"strings"
)

// ErrTokenNotFound is returned when a token key does not exist.
var ErrTokenNotFound = fmt.Errorf("token not found")

// Error IDs (TS3-flavored).
const (
	errOK                      = 0
	errUnknownCommand          = 256
	errInvalidParameter        = 512
	errLoginFailed             = 520
	errServerError             = 1024
	errInsufficientPermissions = 2568
	errTooManyConnections      = 1539
)

// escapeReplacer maps the special characters to their TS3-style escapes.
// Backslash must be replaced first (strings.NewReplacer handles this by
// matching left-to-right with earliest match, and our patterns don't overlap).
var escapeReplacer = strings.NewReplacer(
	`\`, `\\`,
	` `, `\s`,
	`|`, `\p`,
	`/`, `\/`,
	"\n", `\n`,
	"\r", `\r`,
	"\t", `\t`,
)

// escape encodes a value for the query protocol.
func escape(s string) string {
	return escapeReplacer.Replace(s)
}

// unescape decodes a TS3-style escaped value. Unknown escape sequences keep
// the character after the backslash.
func unescape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 's':
			b.WriteByte(' ')
		case 'p':
			b.WriteByte('|')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '/', '\\':
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// errorLine formats the terminating status line of a response.
func errorLine(id int, msg string) string {
	return fmt.Sprintf("error id=%d msg=%s\n", id, escape(msg))
}
