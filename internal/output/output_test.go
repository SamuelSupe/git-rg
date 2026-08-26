package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"git-rg/internal/provider"
	"git-rg/internal/search"
)

func TestRendererNDJSONPreservesSchemaAndReportsWarningsToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	renderer := New("ndjson", &stdout, &stderr)
	events := []search.Event{
		{Type: "meta", SchemaVersion: 1, Provider: "github", Repository: "https://github.com/octocat/Hello-World", Mode: search.ModeExact},
		{Type: "match", Path: "你好.go", Line: 2, Column: 4, Text: "needle <here>"},
		{Type: "warning", Code: "index_unavailable", Message: "temporarily offline"},
		search.NewSummaryEvent(search.Summary{MatchedLines: 1, Complete: true}),
	}
	for _, event := range events {
		if err := renderer.Emit(event); err != nil {
			t.Fatalf("Emit(%q) error = %v", event.Type, err)
		}
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != len(events) {
		t.Fatalf("NDJSON lines = %d, want %d", len(lines), len(events))
	}
	var decoded []search.Event
	for _, line := range lines {
		var event search.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid NDJSON line %q: %v", line, err)
		}
		decoded = append(decoded, event)
	}
	if decoded[0].Type != "meta" || decoded[0].SchemaVersion != 1 || decoded[0].Mode != search.ModeExact {
		t.Fatalf("decoded meta = %#v", decoded[0])
	}
	if decoded[1].Path != "你好.go" || decoded[1].Text != "needle <here>" {
		t.Fatalf("decoded match = %#v", decoded[1])
	}
	if !strings.Contains(stdout.String(), "你好.go") {
		t.Fatalf("NDJSON unexpectedly omitted Unicode path: %q", stdout.String())
	}
	if got := stderr.String(); got != "git-rg: index_unavailable: temporarily offline\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestRendererNDJSONKeepsEmptyMatchAndContextTextFields(t *testing.T) {
	var stdout, stderr bytes.Buffer
	renderer := New("ndjson", &stdout, &stderr)
	for _, event := range []search.Event{
		{Type: "match", Path: "empty.txt", Line: 1, Column: 1},
		{Type: "context", Path: "empty.txt", Line: 2, Context: "after"},
	} {
		if err := renderer.Emit(event); err != nil {
			t.Fatalf("Emit(%q) error = %v", event.Type, err)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("invalid JSON %q: %v", line, err)
		}
		text, ok := raw["text"]
		if !ok {
			t.Fatalf("event %q omitted text field: %s", raw["type"], line)
		}
		if text != "" {
			t.Fatalf("event %q text = %#v, want empty string", raw["type"], text)
		}
	}
}

func TestRendererTextSeparatesMatchesContextsAndDiagnostics(t *testing.T) {
	var stdout, stderr bytes.Buffer
	renderer := New("text", &stdout, &stderr)
	for _, event := range []search.Event{
		{Type: "meta", Provider: "github"},
		{Type: "context", Path: "pkg/a.go", Line: 1, Text: "before"},
		{Type: "match", Path: "pkg/a.go", Line: 2, Column: 7, Text: "needle"},
		{Type: "warning", Code: "cache_read_failed", Message: "bad cache"},
		{Type: "error", Code: "blob_error", Message: "not found"},
		search.NewSummaryEvent(search.Summary{MatchedLines: 1, Complete: true}),
	} {
		if err := renderer.Emit(event); err != nil {
			t.Fatalf("Emit(%q) error = %v", event.Type, err)
		}
	}
	if got, want := stdout.String(), "pkg/a.go-1-before\npkg/a.go:2:7:needle\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "git-rg: cache_read_failed: bad cache\ngit-rg: blob_error: not found\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestRendererTextWritesCommitInfo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	renderer := New("text", &stdout, &stderr)
	event := search.Event{
		Type:   "meta",
		Commit: "commit-123",
		CommitInfo: &provider.CommitInfo{
			Author:      provider.CommitPerson{Name: "Alice", Email: "alice@example.com", Username: "alice"},
			Committer:   provider.CommitPerson{Name: "Bob", Email: "bob@example.com", Username: "bob"},
			AuthoredAt:  "2024-01-02T03:04:05Z",
			CommittedAt: "2024-01-03T04:05:06Z",
			Message:     "add feature\n\nbody\r\ntrailer\rfinal",
		},
	}
	if err := renderer.Emit(event); err != nil {
		t.Fatalf("Emit(meta) error = %v", err)
	}
	want := "commit commit-123\n" +
		"Author: Alice <alice@example.com> (@alice)\n" +
		"AuthorDate: 2024-01-02T03:04:05Z\n" +
		"Committer: Bob <bob@example.com> (@bob)\n" +
		"CommitDate: 2024-01-03T04:05:06Z\n" +
		"Message: add feature\\n\\nbody\\ntrailer\\rfinal\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRendererTextReportsIncompleteSummaryToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	renderer := New("text", &stdout, &stderr)
	if err := renderer.Emit(search.NewSummaryEvent(search.Summary{Complete: false, Reason: "result_limit", Truncated: true})); err != nil {
		t.Fatalf("Emit(incomplete summary) error = %v", err)
	}
	if err := renderer.Emit(search.NewSummaryEvent(search.Summary{Complete: true, Reason: "", Truncated: false})); err != nil {
		t.Fatalf("Emit(complete summary) error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	want := "git-rg: incomplete_results: search results are incomplete (reason=result_limit, truncated=true)\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestRendererEscapesDiagnosticsButPreservesNDJSONMessage(t *testing.T) {
	message := "provider said\nnext\tfield\x1b[31m"

	t.Run("text", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		renderer := New("text", &stdout, &stderr)
		if err := renderer.Emit(search.Event{Type: "error", Code: "remote_error", Message: message}); err != nil {
			t.Fatalf("Emit(error) error = %v", err)
		}
		want := "git-rg: remote_error: provider said\\nnext\\tfield\\u001b[31m\n"
		if stderr.String() != want {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
	})

	t.Run("ndjson", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		renderer := New("ndjson", &stdout, &stderr)
		if err := renderer.Emit(search.Event{Type: "error", Code: "remote_error", Message: message}); err != nil {
			t.Fatalf("Emit(error) error = %v", err)
		}
		var event search.Event
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &event); err != nil {
			t.Fatalf("NDJSON error event = %q: %v", stdout.String(), err)
		}
		if event.Message != message {
			t.Fatalf("decoded message = %q, want original message", event.Message)
		}
		if stderr.String() != "git-rg: remote_error: provider said\\nnext\\tfield\\u001b[31m\n" {
			t.Fatalf("NDJSON stderr = %q, want escaped diagnostic", stderr.String())
		}
	})
}
