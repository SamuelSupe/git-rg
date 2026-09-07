package search

import (
	"strings"
	"sync"
	"testing"
)

func TestGlobSetMatchesBasenamesAndRecursivePaths(t *testing.T) {
	set, err := CompileGlobs([]string{"**/*.go", "!vendor/**", "vendor/keep.go"})
	if err != nil {
		t.Fatalf("CompileGlobs() error = %v", err)
	}
	tests := []struct {
		path string
		want bool
	}{
		{path: "main.go", want: true},
		{path: "internal/search/matcher.go", want: true},
		{path: "vendor/dependency.go", want: false},
		{path: "vendor/keep.go", want: true},
		{path: "README.md", want: false},
		{path: "internal/matcher_test.go", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := set.Match(tt.path); got != tt.want {
				t.Fatalf("Match(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestGlobSetLastMatchingRuleWins(t *testing.T) {
	set, err := CompileGlobs([]string{"*.go", "!*_test.go", "internal/*_test.go"})
	if err != nil {
		t.Fatalf("CompileGlobs() error = %v", err)
	}
	if !set.Match("cmd/main.go") {
		t.Fatal("positive basename glob did not match nested Go file")
	}
	if set.Match("internal/search/matcher_test.go") {
		t.Fatal("negative basename glob did not exclude test file")
	}
	if !set.Match("internal/matcher_test.go") {
		t.Fatal("later positive path rule did not restore match")
	}
}

func TestGlobSetWithOnlyExcludesStartsAllowed(t *testing.T) {
	set, err := CompileGlobs([]string{"!vendor/**", "!*.md"})
	if err != nil {
		t.Fatalf("CompileGlobs() error = %v", err)
	}
	for _, tt := range []struct {
		path string
		want bool
	}{{"internal/a.go", true}, {"vendor/a.go", false}, {"README.md", false}} {
		if got := set.Match(tt.path); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestCompileGlobsRejectsInvalidPatterns(t *testing.T) {
	for _, patterns := range [][]string{{""}, {"!"}, {"["}, {"foo/["}} {
		if _, err := CompileGlobs(patterns); err == nil {
			t.Errorf("CompileGlobs(%q) unexpectedly succeeded", patterns)
		}
	}
}

func TestGlobSetDeepMultipleRecursiveSegmentsRejectsNonMatch(t *testing.T) {
	set, err := CompileGlobs([]string{"**/**/**/**/**/needle"})
	if err != nil {
		t.Fatalf("CompileGlobs() error = %v", err)
	}
	path := strings.TrimSuffix(strings.Repeat("segment/", 2048), "/")
	if set.Match(path) {
		t.Fatalf("Match(%q...) = true, want false for a deep non-matching path", path[:32])
	}
}

func TestGlobSetSupportsEscapesClassesAndUnicode(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		match   []string
		reject  []string
	}{
		{
			name:    "character-class",
			pattern: "src/file[0-9].go",
			match:   []string{"src/file7.go"},
			reject:  []string{"src/filex.go", "src/file17.go"},
		},
		{
			name:    "escaped-metacharacter",
			pattern: `literal/\*.go`,
			match:   []string{"literal/*.go"},
			reject:  []string{"literal/x.go"},
		},
		{
			name:    "unicode-rune",
			pattern: "src/猫?.go",
			match:   []string{"src/猫é.go", "src/猫界.go"},
			reject:  []string{"src/猫.go", "src/犬é.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, err := CompileGlobs([]string{tt.pattern})
			if err != nil {
				t.Fatalf("CompileGlobs(%q) error = %v", tt.pattern, err)
			}
			for _, path := range tt.match {
				if got := set.Match(path); !got {
					t.Errorf("Match(%q) = false for pattern %q, want true", path, tt.pattern)
				}
			}
			for _, path := range tt.reject {
				if got := set.Match(path); got {
					t.Errorf("Match(%q) = true for pattern %q, want false", path, tt.pattern)
				}
			}
		})
	}
}

func TestGlobSetRecursiveSegmentMatchesZeroOrMoreLevels(t *testing.T) {
	set, err := CompileGlobs([]string{"src/**/file.go"})
	if err != nil {
		t.Fatalf("CompileGlobs() error = %v", err)
	}
	for _, tt := range []struct {
		path string
		want bool
	}{
		{path: "src/file.go", want: true},
		{path: "src/one/file.go", want: true},
		{path: "src/one/two/file.go", want: true},
		{path: "src/file.txt", want: false},
		{path: "docs/file.go", want: false},
	} {
		if got := set.Match(tt.path); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestGlobSetConcurrentMatchesRemainStable(t *testing.T) {
	set, err := CompileGlobs([]string{"src/**", "!**/*_test.go"})
	if err != nil {
		t.Fatalf("CompileGlobs() error = %v", err)
	}
	tests := []struct {
		path string
		want bool
	}{
		{path: "src/main.go", want: true},
		{path: "src/pkg/猫.txt", want: true},
		{path: "src/pkg/main_test.go", want: false},
		{path: "docs/main.go", want: false},
	}
	type mismatch struct {
		path string
		got  bool
		want bool
	}
	failures := make(chan mismatch, 1)
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 250; iteration++ {
				for _, tt := range tests {
					if got := set.Match(tt.path); got != tt.want {
						select {
						case failures <- mismatch{path: tt.path, got: got, want: tt.want}:
						default:
						}
						return
					}
				}
			}
		}()
	}
	workers.Wait()
	select {
	case failure := <-failures:
		t.Fatalf("concurrent Match(%q) = %v, want %v", failure.path, failure.got, failure.want)
	default:
	}
}
