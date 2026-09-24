package provider

import (
	"context"
	"errors"
)

var ErrWriteDisabled = errors.New("remote writes are disabled; use propose --enable-write to opt in")

// FileChange contains validated UTF-8 text. Mode is preserved for existing files.
type FileChange struct {
	Action  string
	Path    string
	Content string
	Mode    string
}

type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Head   string `json:"head_commit"`
	Draft  bool   `json:"draft"`
	State  string `json:"state"`
}

// ChangeProvider keeps optional write operations out of the search contract.
// BranchHead returns an empty SHA only when the branch does not exist.
// CreateChange creates a new branch; it must never force-update an existing ref.
type ChangeProvider interface {
	Provider
	ListChangeTree(context.Context, Snapshot) ([]Entry, error)
	BranchHead(context.Context, Snapshot, string) (string, error)
	CreateChange(context.Context, Snapshot, string, string, []FileChange) (string, error)
	FindPullRequest(context.Context, Snapshot, string, string) (*PullRequest, error)
	CreatePullRequest(context.Context, Snapshot, string, string, string, string) (PullRequest, error)
}

func isNotFound(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == 404
}

func ValidBranchName(name string) bool {
	return validRefName(name) && name != "HEAD" && name[0] != '-' && len(name) <= 240
}
