package provider

import (
	"context"
	"fmt"
	"io"
	"time"
)

const MaxGitHubBlobSize int64 = 100 << 20

type Repository struct {
	Provider string
	Host     string
	Project  string
	WebURL   string
	APIBase  string
}

func (r Repository) CacheNamespace() string {
	return r.Provider + "\x00" + r.APIBase + "\x00" + r.Project
}

type CommitPerson struct {
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	Username string `json:"username,omitempty"`
}

type CommitInfo struct {
	Author      CommitPerson `json:"author"`
	Committer   CommitPerson `json:"committer"`
	AuthoredAt  string       `json:"authored_at,omitempty"`
	CommittedAt string       `json:"committed_at,omitempty"`
	Message     string       `json:"message,omitempty"`
}

type RefKind string

const (
	RefKindAll    RefKind = "all"
	RefKindBranch RefKind = "branch"
	RefKindTag    RefKind = "tag"
)

type Ref struct {
	Kind   RefKind
	Name   string
	Commit string
}

type Snapshot struct {
	Repository    Repository
	RequestedRef  string
	ResolvedRef   string
	Commit        string
	CommitInfo    *CommitInfo
	TreeOID       string
	DefaultBranch string
	RemoteID      string
}

type Entry struct {
	Path string `json:"path"`
	OID  string `json:"oid"`
	Mode string `json:"mode"`
	Size int64  `json:"size,omitempty"`
}

type RateLimit struct {
	Limit     int    `json:"limit"`
	Remaining int    `json:"remaining"`
	Reset     int64  `json:"reset,omitempty"`
	Resource  string `json:"resource,omitempty"`
	Name      string `json:"name,omitempty"`
}

type RequestStats struct {
	Requests     int
	Retries      int
	RequestLimit int
	RateLimit    *RateLimit
}

type Options struct {
	Token        string
	Timeout      time.Duration
	RequestLimit int
}

type RequestBudgetError struct {
	Limit int
}

func (e *RequestBudgetError) Error() string {
	return fmt.Sprintf("remote request budget of %d exhausted", e.Limit)
}

type Provider interface {
	Resolve(context.Context, Repository, string) (Snapshot, error)
	ListTree(context.Context, Snapshot, bool) ([]Entry, bool, error)
	OpenBlob(context.Context, Snapshot, Entry) (io.ReadCloser, error)
	OpenArchive(context.Context, Snapshot) (io.ReadCloser, error)
	SearchCandidates(context.Context, Snapshot, string) ([]string, error)
	RequestStats() RequestStats
}

type RefLister interface {
	ListRefs(context.Context, Repository, RefKind) ([]Ref, error)
}
