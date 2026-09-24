package proposal

import (
	"fmt"
	"strings"
)

// A single hunk around the changed span gives a bounded, linear-time preview,
// even when an agent replaces most of a file or supplies many repeated lines.
func writeDiff(out *strings.Builder, change Change, mode, old, updated string) {
	oldPath, newPath := "a/"+change.Path, "b/"+change.Path
	fmt.Fprintf(out, "diff --git %s %s\n", quotePath(oldPath), quotePath(newPath))
	switch change.Action {
	case "create":
		fmt.Fprintf(out, "new file mode %s\n", mode)
		oldPath = "/dev/null"
	case "delete":
		fmt.Fprintf(out, "deleted file mode %s\n", mode)
		newPath = "/dev/null"
	}
	fmt.Fprintf(out, "--- %s\n+++ %s\n", quotePath(oldPath), quotePath(newPath))
	before, after := diffLines(old), diffLines(updated)
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix && before[len(before)-suffix-1] == after[len(after)-suffix-1] {
		suffix++
	}
	start := max(0, prefix-3)
	oldEnd, newEnd := len(before)-max(0, suffix-3), len(after)-max(0, suffix-3)
	oldCount, newCount := oldEnd-start, newEnd-start
	if oldCount == 0 && newCount == 0 {
		return
	}
	oldStart, newStart := start+1, start+1
	if oldCount == 0 {
		oldStart = start
	}
	if newCount == 0 {
		newStart = start
	}
	fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	line := func(prefix byte, value string) {
		out.WriteByte(prefix)
		out.WriteString(value)
		if !strings.HasSuffix(value, "\n") {
			out.WriteString("\n\\ No newline at end of file\n")
		}
	}
	for _, value := range before[start:prefix] {
		line(' ', value)
	}
	for _, value := range before[prefix : len(before)-suffix] {
		line('-', value)
	}
	for _, value := range after[prefix : len(after)-suffix] {
		line('+', value)
	}
	for _, value := range before[len(before)-suffix : oldEnd] {
		line(' ', value)
	}
}

func diffLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.SplitAfter(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func quotePath(value string) string {
	if !strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r >= 0x7f || r == '"' || r == '\\' }) {
		return value
	}
	var quoted strings.Builder
	quoted.WriteByte('"')
	for i := 0; i < len(value); i++ {
		b := value[i]
		switch {
		case b == '"' || b == '\\':
			quoted.WriteByte('\\')
			quoted.WriteByte(b)
		case b < ' ' || b >= 0x7f:
			// Git quotes UTF-8 bytes as octal, not Go's Unicode escape sequences.
			fmt.Fprintf(&quoted, "\\%03o", b)
		default:
			quoted.WriteByte(b)
		}
	}
	quoted.WriteByte('"')
	return quoted.String()
}
