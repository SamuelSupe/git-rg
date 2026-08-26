package output

import (
	"fmt"
	"strings"
	"unicode"
)

// EscapeDiagnostic keeps untrusted provider text on one terminal-safe line.
func EscapeDiagnostic(message string) string {
	var escaped strings.Builder
	for _, r := range message {
		switch r {
		case '\n':
			escaped.WriteString(`\n`)
		case '\r':
			escaped.WriteString(`\r`)
		case '\t':
			escaped.WriteString(`\t`)
		default:
			if unicode.IsControl(r) {
				_, _ = fmt.Fprintf(&escaped, `\u%04x`, r)
			} else {
				escaped.WriteRune(r)
			}
		}
	}
	return escaped.String()
}
