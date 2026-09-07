package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SamuelSupe/git-rg/internal/provider"
	"github.com/SamuelSupe/git-rg/internal/search"
)

func TestRendererNDJSONWireEncodingPreservesEventSemantics(t *testing.T) {
	events := []search.Event{
		{Type: "match", Path: "你好<&.go", Line: 7, Column: 3, Text: "<needle>&", Submatches: []search.Submatch{{Start: 1, End: 7, Text: "needle"}}},
		{Type: "context", Path: "空.txt", Line: 8, Text: "", Context: "after"},
		{Type: "warning", Code: "control", Message: "line\nfield\t\x1b<tag>"},
	}
	for _, event := range events {
		want, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("Marshal(%q) error = %v", event.Type, err)
		}
		var encoded bytes.Buffer
		if err := event.EncodeJSON(json.NewEncoder(&encoded)); err != nil {
			t.Fatalf("EncodeJSON(%q) error = %v", event.Type, err)
		}
		if got := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}); !bytes.Equal(got, want) {
			t.Fatalf("EncodeJSON(%q) = %q, MarshalJSON = %q", event.Type, got, want)
		}

		var stdout, stderr bytes.Buffer
		renderer := New("ndjson", &stdout, &stderr)
		if err := renderer.Emit(event); err != nil {
			t.Fatalf("Renderer.Emit(%q) error = %v", event.Type, err)
		}
		if got := bytes.TrimSuffix(stdout.Bytes(), []byte{'\n'}); !bytes.Equal(got, want) {
			t.Fatalf("Renderer.Emit(%q) = %q, MarshalJSON = %q", event.Type, got, want)
		}
		if event.Type == "match" || event.Type == "context" {
			if !bytes.Contains(stdout.Bytes(), []byte(`"text":"`)) {
				t.Fatalf("Renderer.Emit(%q) omitted required text field: %s", event.Type, stdout.String())
			}
		}
	}
}

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

func TestNewBufferedFlushesFirstMatchAndSparseResults(t *testing.T) {
	sink := newObservedWriter()
	renderer := NewBuffered("ndjson", sink, io.Discard)
	defer renderer.Close()

	if err := renderer.Emit(search.Event{Type: "match", Path: "first.txt", Line: 1, Column: 1, Text: "needle"}); err != nil {
		t.Fatalf("Emit(first match) error = %v", err)
	}
	if sink.writeCount() == 0 || !bytes.Contains(sink.snapshot(), []byte(`"type":"match"`)) {
		t.Fatalf("first output = writes=%d data=%q, want synchronously visible match", sink.writeCount(), sink.snapshot())
	}
	firstWrites := sink.writeCount()
	sink.drainNotifications()

	if err := renderer.Emit(search.Event{Type: "context", Path: "first.txt", Line: 2, Text: "after", Context: "after"}); err != nil {
		t.Fatalf("Emit(context) error = %v", err)
	}
	if !sink.waitForWrite(500 * time.Millisecond) {
		t.Fatal("sparse context was not flushed by the timer")
	}
	if sink.writeCount() <= firstWrites || !bytes.Contains(sink.snapshot(), []byte(`"type":"context"`)) {
		t.Fatalf("timer output = writes=%d data=%q, want context event", sink.writeCount(), sink.snapshot())
	}
}

func TestNewBufferedFlushesSummaryAndCloseStopsTimer(t *testing.T) {
	sink := newObservedWriter()
	renderer := NewBuffered("ndjson", sink, io.Discard)
	if err := renderer.Emit(search.Event{Type: "match", Path: "file.txt", Line: 1, Column: 1, Text: "needle"}); err != nil {
		t.Fatalf("Emit(match) error = %v", err)
	}
	if !sink.waitForWrite(300 * time.Millisecond) {
		t.Fatal("match was not flushed")
	}
	if err := renderer.Emit(search.Event{Type: "context", Path: "file.txt", Line: 2, Text: "after", Context: "after"}); err != nil {
		t.Fatalf("Emit(context) error = %v", err)
	}
	beforeSummary := sink.writeCount()
	if err := renderer.Emit(search.NewSummaryEvent(search.Summary{MatchedLines: 1, Complete: true})); err != nil {
		t.Fatalf("Emit(summary) error = %v", err)
	}
	if !sink.waitForWrite(300*time.Millisecond) || sink.writeCount() <= beforeSummary {
		t.Fatal("summary did not flush pending context output")
	}
	if got := countNDJSONLines(sink.snapshot()); got != 3 {
		t.Fatalf("summary output lines = %d, want match, context, summary", got)
	}

	if err := renderer.Emit(search.Event{Type: "context", Path: "file.txt", Line: 3, Text: "pending", Context: "after"}); err != nil {
		t.Fatalf("Emit(pending context) error = %v", err)
	}
	if err := renderer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := countNDJSONLines(sink.snapshot()); got != 4 {
		t.Fatalf("close output lines = %d, want pending context flushed", got)
	}
	sink.drainNotifications()
	closedWrites := sink.writeCount()
	select {
	case <-sink.notify:
		t.Fatalf("timer wrote after Close; writes=%d, want %d", sink.writeCount(), closedWrites)
	case <-time.After(150 * time.Millisecond):
	}
	if sink.writeCount() != closedWrites {
		t.Fatalf("writes after Close = %d, want timer stopped at %d", sink.writeCount(), closedWrites)
	}
}

func TestNewBufferedPropagatesWriterErrors(t *testing.T) {
	writeErr := errors.New("sink failed")

	t.Run("next emit", func(t *testing.T) {
		sink := &failingWriter{err: writeErr, notify: make(chan struct{}, 4)}
		renderer := NewBuffered("ndjson", sink, io.Discard)
		if err := renderer.Emit(search.Event{Type: "match", Path: "file.txt", Line: 1, Column: 1, Text: "needle"}); err != nil {
			t.Fatalf("initial Emit() error = %v", err)
		}
		select {
		case <-sink.notify:
		default:
			t.Fatal("initial match did not reach the underlying writer")
		}
		sink.setFail()
		if err := renderer.Emit(search.Event{Type: "context", Path: "file.txt", Line: 2, Text: "after", Context: "after"}); err != nil {
			t.Fatalf("pending Emit() error = %v", err)
		}
		select {
		case <-sink.notify:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("background timer did not attempt the failed write")
		}
		if err := renderer.Emit(search.Event{Type: "warning", Code: "failed", Message: "flush me"}); !errors.Is(err, writeErr) {
			t.Fatalf("next Emit() error = %v, want %v", err, writeErr)
		}
		if err := renderer.Flush(); !errors.Is(err, writeErr) {
			t.Fatalf("Flush() after timer error = %v, want %v", err, writeErr)
		}
		if err := renderer.Close(); !errors.Is(err, writeErr) {
			t.Fatalf("Close() after timer error = %v, want %v", err, writeErr)
		}
	})

	t.Run("flush and close", func(t *testing.T) {
		sink := &failingWriter{err: writeErr}
		renderer := NewBuffered("ndjson", sink, io.Discard)
		if err := renderer.Emit(search.Event{Type: "context", Path: "file.txt", Line: 1, Text: "pending", Context: "after"}); err != nil {
			t.Fatalf("Emit() error = %v", err)
		}
		sink.setFail()
		if err := renderer.Flush(); !errors.Is(err, writeErr) {
			t.Fatalf("Flush() error = %v, want %v", err, writeErr)
		}
		if err := renderer.Close(); !errors.Is(err, writeErr) {
			t.Fatalf("Close() error = %v, want %v", err, writeErr)
		}
	})

	t.Run("emit after close", func(t *testing.T) {
		sink := &failingWriter{err: writeErr}
		renderer := NewBuffered("ndjson", sink, io.Discard)
		if err := renderer.Close(); err != nil {
			t.Fatalf("initial Close() error = %v", err)
		}
		if err := renderer.Emit(search.Event{Type: "match", Path: "file.txt", Line: 1, Column: 1, Text: "needle"}); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("Emit() after Close error = %v, want io.ErrClosedPipe", err)
		}
	})
}

type observedWriter struct {
	mu     sync.Mutex
	data   bytes.Buffer
	writes int
	notify chan struct{}
}

func newObservedWriter() *observedWriter {
	return &observedWriter{notify: make(chan struct{}, 32)}
}

func (w *observedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if _, err := w.data.Write(data); err != nil {
		return 0, err
	}
	select {
	case w.notify <- struct{}{}:
	default:
	}
	return len(data), nil
}

func (w *observedWriter) snapshot() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.data.Bytes()...)
}

func (w *observedWriter) writeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

func (w *observedWriter) waitForWrite(timeout time.Duration) bool {
	select {
	case <-w.notify:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (w *observedWriter) drainNotifications() {
	for {
		select {
		case <-w.notify:
		default:
			return
		}
	}
}

type failingWriter struct {
	mu     sync.Mutex
	err    error
	fail   bool
	notify chan struct{}
}

func (w *failingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail {
		if w.notify != nil {
			select {
			case w.notify <- struct{}{}:
			default:
			}
		}
		return 0, w.err
	}
	if w.notify != nil {
		select {
		case w.notify <- struct{}{}:
		default:
		}
	}
	return len(data), nil
}

func (w *failingWriter) setFail() {
	w.mu.Lock()
	w.fail = true
	w.mu.Unlock()
}

func countNDJSONLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	return bytes.Count(data, []byte{'\n'})
}
