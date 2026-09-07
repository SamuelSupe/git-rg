package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/SamuelSupe/git-rg/internal/provider"
	"github.com/SamuelSupe/git-rg/internal/search"
)

type Renderer struct {
	format  string
	out     io.Writer
	err     io.Writer
	json    *json.Encoder
	buffer  *bufferedWriter
	matched bool
}

func New(format string, stdout, stderr io.Writer) *Renderer {
	encoder := json.NewEncoder(stdout)
	return &Renderer{format: format, out: stdout, err: stderr, json: encoder}
}

// NewBuffered batches NDJSON writes and flushes pending output on a 100 ms timer.
// The caller must Close the renderer to finish pending output and stop its timer.
func NewBuffered(format string, stdout, stderr io.Writer) *Renderer {
	if format != "ndjson" {
		return New(format, stdout, stderr)
	}
	buffer := newBufferedWriter(stdout)
	renderer := New(format, buffer, stderr)
	renderer.buffer = buffer
	return renderer
}

func (r *Renderer) Flush() error {
	if r.buffer != nil {
		return r.buffer.Flush()
	}
	return nil
}

func (r *Renderer) Close() error {
	if r.buffer != nil {
		return r.buffer.Close()
	}
	return nil
}

func (r *Renderer) Emit(event search.Event) error {
	if r.format == "ndjson" {
		if err := event.EncodeJSON(r.json); err != nil {
			return err
		}
		if event.Type != "match" && event.Type != "context" || event.Type == "match" && !r.matched {
			if err := r.Flush(); err != nil {
				return err
			}
		}
		if event.Type == "match" {
			r.matched = true
		}
		if event.Type == "warning" || event.Type == "error" {
			_, _ = fmt.Fprintf(r.err, "git-rg: %s: %s\n", event.Code, EscapeDiagnostic(event.Message))
		}
		return nil
	}

	switch event.Type {
	case "meta":
		if event.CommitInfo == nil {
			return nil
		}
		return writeCommitInfo(r.out, event.Commit, *event.CommitInfo)
	case "match":
		_, err := fmt.Fprintf(r.out, "%s:%d:%d:%s\n", event.Path, event.Line, event.Column, event.Text)
		return err
	case "context":
		_, err := fmt.Fprintf(r.out, "%s-%d-%s\n", event.Path, event.Line, event.Text)
		return err
	case "summary":
		if event.Summary == nil || event.Summary.Complete {
			return nil
		}
		reason := event.Summary.Reason
		if reason == "" {
			reason = "unknown"
		}
		_, err := fmt.Fprintf(r.err, "git-rg: incomplete_results: search results are incomplete (reason=%s, truncated=%t)\n", reason, event.Summary.Truncated)
		return err
	case "warning", "error":
		_, err := fmt.Fprintf(r.err, "git-rg: %s: %s\n", event.Code, EscapeDiagnostic(event.Message))
		return err
	default:
		return nil
	}
}

func writeCommitInfo(w io.Writer, commit string, info provider.CommitInfo) error {
	if _, err := fmt.Fprintf(w, "commit %s\n", commit); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Author: %s\n", formatCommitPerson(info.Author)); err != nil {
		return err
	}
	if info.AuthoredAt != "" {
		if _, err := fmt.Fprintf(w, "AuthorDate: %s\n", info.AuthoredAt); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "Committer: %s\n", formatCommitPerson(info.Committer)); err != nil {
		return err
	}
	if info.CommittedAt != "" {
		if _, err := fmt.Fprintf(w, "CommitDate: %s\n", info.CommittedAt); err != nil {
			return err
		}
	}
	if info.Message != "" {
		if _, err := fmt.Fprintf(w, "Message: %s\n", oneLineCommitMessage(info.Message)); err != nil {
			return err
		}
	}
	return nil
}

func formatCommitPerson(person provider.CommitPerson) string {
	identity := person.Name
	if person.Email != "" {
		if identity != "" {
			identity += " "
		}
		identity += "<" + person.Email + ">"
	}
	if person.Username != "" {
		if identity != "" {
			identity += " "
		}
		identity += "(@" + person.Username + ")"
	}
	if identity == "" {
		return "unknown"
	}
	return identity
}

func oneLineCommitMessage(message string) string {
	message = strings.TrimRight(message, "\r\n")
	message = strings.ReplaceAll(message, "\r\n", "\n")
	message = strings.ReplaceAll(message, "\r", "\\r")
	return strings.ReplaceAll(message, "\n", "\\n")
}
