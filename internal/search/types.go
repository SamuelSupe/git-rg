package search

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SamuelSupe/git-rg/internal/cache"
	"github.com/SamuelSupe/git-rg/internal/provider"
)

type Mode string

const (
	ModeAuto    Mode = "auto"
	ModeExact   Mode = "exact"
	ModeIndexed Mode = "indexed"
)

type Config struct {
	Mode         Mode
	MaxResults   int
	Workers      int
	Globs        []string
	RequestLimit int
}

type Submatch struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

type Event struct {
	Type          string               `json:"type"`
	SchemaVersion int                  `json:"schema_version,omitempty"`
	Provider      string               `json:"provider,omitempty"`
	Repository    string               `json:"repository,omitempty"`
	RequestedRef  string               `json:"requested_ref,omitempty"`
	ResolvedRef   string               `json:"resolved_ref,omitempty"`
	Commit        string               `json:"commit,omitempty"`
	CommitInfo    *provider.CommitInfo `json:"commit_info,omitempty"`
	Mode          Mode                 `json:"mode,omitempty"`
	Path          string               `json:"path,omitempty"`
	Line          int                  `json:"line,omitempty"`
	Column        int                  `json:"column,omitempty"`
	Text          string               `json:"text,omitempty"`
	Context       string               `json:"context,omitempty"`
	Submatches    []Submatch           `json:"submatches,omitempty"`
	Code          string               `json:"code,omitempty"`
	Message       string               `json:"message,omitempty"`
	Summary       *Summary             `json:"summary,omitempty"`
}

func (e Event) MarshalJSON() ([]byte, error) {
	type eventAlias Event
	if e.Type != "match" && e.Type != "context" {
		return json.Marshal(eventAlias(e))
	}
	return json.Marshal(struct {
		eventAlias
		Text string `json:"text"`
	}{eventAlias: eventAlias(e), Text: e.Text})
}

type Summary struct {
	MatchedLines    int                 `json:"matched_lines"`
	MatchedFiles    int                 `json:"matched_files"`
	ScannedFiles    int                 `json:"scanned_files"`
	CacheHits       int                 `json:"cache_hits"`
	DownloadedBytes int64               `json:"downloaded_bytes"`
	SkippedBinary   int                 `json:"skipped_binary"`
	APIRequests     int                 `json:"api_requests"`
	APIRetries      int                 `json:"api_retries"`
	RequestLimit    int                 `json:"request_limit"`
	RateLimit       *provider.RateLimit `json:"rate_limit,omitempty"`
	Transport       string              `json:"transport,omitempty"`
	Complete        bool                `json:"complete"`
	Truncated       bool                `json:"truncated"`
	Reason          string              `json:"reason,omitempty"`
	DurationMS      int64               `json:"duration_ms"`
}

type Emitter func(Event) error

type Runner struct {
	Provider provider.Provider
	Cache    *cache.Cache
	Matcher  *Matcher
	Config   Config
}

type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %v", e.Code, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

func NewMetaEvent(snapshot provider.Snapshot, mode Mode, includeCommitInfo bool) Event {
	var commitInfo *provider.CommitInfo
	if includeCommitInfo {
		commitInfo = snapshot.CommitInfo
	}
	return Event{
		Type:          "meta",
		SchemaVersion: 1,
		Provider:      snapshot.Repository.Provider,
		Repository:    snapshot.Repository.WebURL,
		RequestedRef:  snapshot.RequestedRef,
		ResolvedRef:   snapshot.ResolvedRef,
		Commit:        snapshot.Commit,
		CommitInfo:    commitInfo,
		Mode:          mode,
	}
}

func NewSummaryEvent(summary Summary) Event {
	return Event{Type: "summary", Summary: &summary}
}

func contextError(ctx context.Context) *Error {
	return &Error{Code: "search_cancelled", Err: ctx.Err()}
}
