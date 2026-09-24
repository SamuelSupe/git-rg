package proposal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Git is a test oracle for the exported preview, not a runtime dependency.
func TestPreviewAppliesToExactFileContents(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is needed to validate unified diffs")
	}
	for _, tc := range []struct{ name, action, path, old, updated string }{
		{"replace", "update", "file.txt", "a\nb\nc\n", "a\nx\nc\n"},
		{"insert", "update", "file.txt", "a\nb\n", "a\nx\nb\n"},
		{"remove", "update", "file.txt", "a\nx\nb\n", "a\nb\n"},
		{"no final newline", "update", "file.txt", "old", "new"},
		{"add newline", "update", "file.txt", "same", "same\n"},
		{"remove newline", "update", "file.txt", "same\n", "same"},
		{"new empty file", "create", "file.txt", "", ""},
		{"delete empty file", "delete", "file.txt", "", ""},
		{"new file", "create", "nested/file.txt", "", "hello\n"},
		{"delete file", "delete", "file.txt", "goodbye", ""},
		{"quoted path", "update", "a 文件.txt", "old\r\n", "new\r\n"},
		{"unicode separator in quoted path", "update", "a \u2028.txt", "old\n", "new\n"},
		{"invisible unicode in quoted path", "update", "a \u200b.txt", "old\n", "new\n"},
		{"quotes in unicode path", "update", "a \"文件\".txt", "old\n", "new\n"},
		{"distant edits", "update", "file.txt", strings.Repeat("a\n", 8) + "old\n" + strings.Repeat("b\n", 8) + "before\n" + strings.Repeat("c\n", 8), strings.Repeat("a\n", 8) + "new\n" + strings.Repeat("b\n", 8) + "after\n" + strings.Repeat("c\n", 8)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, tc.path)
			if tc.action != "create" {
				if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(tc.old), 0644); err != nil {
					t.Fatal(err)
				}
			}
			var diff strings.Builder
			writeDiff(&diff, Change{Action: tc.action, Path: tc.path}, "100644", tc.old, tc.updated)
			command := exec.Command(git, "apply", "--whitespace=nowarn", "-")
			command.Dir, command.Stdin = dir, strings.NewReader(diff.String())
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("git apply: %v: %s\n%s", err, output, diff.String())
			}
			data, err := os.ReadFile(file)
			if tc.action == "delete" {
				if !os.IsNotExist(err) {
					t.Fatalf("deleted file still exists: %v", err)
				}
			} else if err != nil || string(data) != tc.updated {
				t.Fatalf("applied file = %q, error %v; want %q", data, err, tc.updated)
			}
		})
	}
}
