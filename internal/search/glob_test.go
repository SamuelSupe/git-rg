package search

import (
	"strings"
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
