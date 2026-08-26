package search

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestMatcherFixedIgnoreCaseAndUnicodeWordBoundaries(t *testing.T) {
	matcher, err := NewMatcher(MatcherConfig{Pattern: "go", Fixed: true, IgnoreCase: true, Word: true})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	if got := matcher.LiteralPrefix(); got != "go" {
		t.Fatalf("LiteralPrefix() = %q, want go", got)
	}
	result, err := matcher.Scan("unicode.txt", strings.NewReader("猫 Go go_ go! 世界\n"), 0)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if result.Matches != 1 {
		t.Fatalf("Matches = %d, events=%#v, want one matching line", result.Matches, result.Events)
	}
	want := []Event{
		{
			Type: "match", Path: "unicode.txt", Line: 1, Column: 5, Text: "猫 Go go_ go! 世界",
			Submatches: []Submatch{{Start: 4, End: 6, Text: "Go"}, {Start: 11, End: 13, Text: "go"}},
		},
	}
	if !reflect.DeepEqual(result.Events, want) {
		t.Fatalf("Events = %#v, want %#v", result.Events, want)
	}
}

func TestMatcherRegexLiteralPrefixAndMultipleMatches(t *testing.T) {
	matcher, err := NewMatcher(MatcherConfig{Pattern: "prefix-[0-9]+"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	if got := matcher.LiteralPrefix(); got != "prefix-" {
		t.Fatalf("LiteralPrefix() = %q, want prefix-", got)
	}
	result, err := matcher.Scan("numbers.txt", strings.NewReader("prefix-1 x prefix-22\r\nlast\n"), 0)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if result.Matches != 1 || len(result.Events) != 1 {
		t.Fatalf("result = %#v, want one match event", result)
	}
	if got := result.Events[0].Submatches; !reflect.DeepEqual(got, []Submatch{{Start: 0, End: 8, Text: "prefix-1"}, {Start: 11, End: 20, Text: "prefix-22"}}) {
		t.Fatalf("Submatches = %#v", got)
	}
}

func TestMatcherContextDoesNotDuplicateLines(t *testing.T) {
	matcher, err := NewMatcher(MatcherConfig{Pattern: `match`, Before: 2, After: 2})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	result, err := matcher.Scan("file.txt", strings.NewReader("one\ntwo\nmatch three\nfour\nmatch five\nsix\nseven\n"), 0)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if result.Matches != 2 {
		t.Fatalf("Matches = %d, want 2", result.Matches)
	}
	want := []struct {
		typ, context string
		line         int
	}{
		{typ: "context", context: "before", line: 1},
		{typ: "context", context: "before", line: 2},
		{typ: "match", line: 3},
		{typ: "context", context: "after", line: 4},
		{typ: "match", line: 5},
		{typ: "context", context: "after", line: 6},
		{typ: "context", context: "after", line: 7},
	}
	if len(result.Events) != len(want) {
		t.Fatalf("event count = %d, want %d (%#v)", len(result.Events), len(want), result.Events)
	}
	for i, event := range result.Events {
		if event.Type != want[i].typ || event.Context != want[i].context || event.Line != want[i].line {
			t.Errorf("event %d = %#v, want type/context/line %q/%q/%d", i, event, want[i].typ, want[i].context, want[i].line)
		}
	}
}

func TestMatcherMaxMatchesStopsAtRequestedMatch(t *testing.T) {
	matcher, err := NewMatcher(MatcherConfig{Pattern: "x", After: 2})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	result, err := matcher.Scan("file.txt", strings.NewReader("x\nafter\nx\n"), 1)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if !result.HitLimit || result.Matches != 1 || len(result.Events) != 1 || result.Events[0].Type != "match" {
		t.Fatalf("result = %#v, want one match and HitLimit", result)
	}
}

func TestMatcherReportsReaderErrors(t *testing.T) {
	matcher, err := NewMatcher(MatcherConfig{Pattern: "x"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	wantErr := errors.New("boom")
	_, err = matcher.Scan("broken.txt", errorReader{err: wantErr}, 0)
	if err == nil || !strings.Contains(err.Error(), `read "broken.txt"`) || !errors.Is(err, wantErr) {
		t.Fatalf("Scan() error = %v, want wrapped reader error", err)
	}
}

func TestMatcherRejectsInvalidPattern(t *testing.T) {
	if _, err := NewMatcher(MatcherConfig{Pattern: "["}); err == nil {
		t.Fatal("NewMatcher() unexpectedly accepted invalid regexp")
	}
}

func TestMatcherRejectsSubmatchResourceLimit(t *testing.T) {
	matcher, err := NewMatcher(MatcherConfig{Pattern: "x"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	_, err = matcher.Scan("many-matches.txt", strings.NewReader(strings.Repeat("x", maxSubmatchesPerLine+1)), 0)
	if err == nil || !errors.Is(err, errSubmatchLimit) || ErrorCode(err) != "resource_limit" {
		t.Fatalf("Scan() error = %v, want errSubmatchLimit/resource_limit", err)
	}
}

func TestMatcherScanEmitContextStopsBeforeReadingCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	matcher, err := NewMatcher(MatcherConfig{Pattern: "x"})
	if err != nil {
		t.Fatalf("NewMatcher() error = %v", err)
	}
	_, err = matcher.ScanEmitContext(ctx, "canceled.txt", errorReader{err: errors.New("reader should not be called")}, 0, func(Event) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ScanEmitContext() error = %v, want context.Canceled", err)
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

var _ io.Reader = errorReader{}
