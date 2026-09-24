package proposal

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"strings"

	"github.com/SamuelSupe/git-rg/internal/provider"
)

var ErrBlobConflict = errors.New("file content differs from expected_blob")
var ErrBaseChanged = errors.New("base branch has changed; read the new commit and regenerate the plan")
var ErrBranchConflict = errors.New("proposal branch contains a different change; choose a new branch")

type Prepared struct {
	Plan     Plan
	Snapshot provider.Snapshot
	Changes  []provider.FileChange
	Diff     string
	Message  string
	expected map[string]provider.Entry
}

func ReadFile(ctx context.Context, remote provider.ChangeProvider, snapshot provider.Snapshot, name string) (provider.Entry, string, error) {
	if err := ValidatePath(name); err != nil {
		return provider.Entry{}, "", err
	}
	tree, err := loadTree(ctx, remote, snapshot)
	if err != nil {
		return provider.Entry{}, "", err
	}
	entry, ok := tree[name]
	if !ok {
		return provider.Entry{}, "", fmt.Errorf("file %q does not exist at %s", name, snapshot.Commit)
	}
	content, err := readText(ctx, remote, snapshot, entry)
	return entry, content, err
}

func Prepare(ctx context.Context, remote provider.ChangeProvider, repo provider.Repository, plan Plan) (*Prepared, error) {
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	plan.Changes = sortedChanges(plan.Changes)
	snapshot, err := remote.Resolve(ctx, repo, plan.BaseCommit)
	if err != nil {
		return nil, err
	}
	if snapshot.Commit != plan.BaseCommit {
		return nil, errors.New("provider resolved a different base commit")
	}
	if plan.Branch == snapshot.DefaultBranch {
		return nil, errors.New("proposal branch must not be the default branch")
	}
	tree, err := loadTree(ctx, remote, snapshot)
	if err != nil {
		return nil, err
	}
	expected := make(map[string]provider.Entry, len(tree)+len(plan.Changes))
	for name, entry := range tree {
		expected[name] = entry
	}
	prepared := &Prepared{Plan: plan, Snapshot: snapshot, expected: expected}
	var diff strings.Builder
	oldBytes := 0
	for _, change := range plan.Changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, exists := tree[change.Path]
		old := ""
		if change.Action == "create" {
			if exists {
				return nil, fmt.Errorf("create %q: path already exists", change.Path)
			}
			entry = provider.Entry{Path: change.Path, Mode: "100644"}
		} else {
			if !exists || entry.OID != change.ExpectedBlob {
				return nil, fmt.Errorf("%q: %w", change.Path, ErrBlobConflict)
			}
			old, err = readText(ctx, remote, snapshot, entry)
			if err != nil {
				return nil, err
			}
			oldBytes += len(old)
			if oldBytes > maxContentBytes {
				return nil, errors.New("original file contents exceed 4 MiB in total")
			}
		}
		updated := ""
		if change.Content != nil {
			updated = *change.Content
		}
		if change.Action == "update" && updated == old {
			return nil, fmt.Errorf("update %q does not change its content", change.Path)
		}
		prepared.Changes = append(prepared.Changes, provider.FileChange{Action: change.Action, Path: change.Path, Content: updated, Mode: entry.Mode})
		if change.Action == "delete" {
			delete(expected, change.Path)
		} else {
			entry.OID = blobOID(updated, len(plan.BaseCommit))
			entry.Size = int64(len(updated))
			expected[change.Path] = entry
		}
		writeDiff(&diff, change, entry.Mode, old, updated)
	}
	// A leaf cannot also be the parent directory of another leaf, including a
	// symlink or submodule omitted by the search-oriented tree reader.
	for name := range expected {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if _, exists := expected[parent]; exists {
				return nil, fmt.Errorf("file/directory collision at %q", parent)
			}
		}
	}
	data, _ := json.Marshal(plan)
	fingerprint := sha256.Sum256(append([]byte(repo.CacheNamespace()+"\x00"), data...))
	prepared.Message = strings.TrimRight(plan.CommitMessage, "\n") + "\n\nGit-Rg-Change: " + hex.EncodeToString(fingerprint[:])
	prepared.Diff = diff.String()
	return prepared, nil
}

func loadTree(ctx context.Context, remote provider.ChangeProvider, snapshot provider.Snapshot) (map[string]provider.Entry, error) {
	entries, err := remote.ListChangeTree(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	tree := make(map[string]provider.Entry, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Path == "" || entry.Path == "." || path.Clean(entry.Path) != entry.Path || path.IsAbs(entry.Path) || entry.Path == ".." || strings.HasPrefix(entry.Path, "../") || !validOID(entry.OID) {
			return nil, errors.New("provider returned an invalid tree entry")
		}
		if _, duplicate := tree[entry.Path]; duplicate {
			return nil, fmt.Errorf("duplicate tree entry %q", entry.Path)
		}
		tree[entry.Path] = entry
	}
	return tree, nil
}

func readText(ctx context.Context, remote provider.Provider, snapshot provider.Snapshot, entry provider.Entry) (string, error) {
	if entry.Mode != "100644" && entry.Mode != "100755" {
		return "", fmt.Errorf("%q is not a regular file", entry.Path)
	}
	if entry.Size > MaxFileBytes {
		return "", fmt.Errorf("%q exceeds 1 MiB", entry.Path)
	}
	reader, err := remote.OpenBlob(ctx, snapshot, entry)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, MaxFileBytes+1))
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	content := string(data)
	if err := validateText(content); err != nil {
		return "", fmt.Errorf("%q: %w", entry.Path, err)
	}
	if blobOID(content, len(entry.OID)) != entry.OID {
		return "", fmt.Errorf("%q: downloaded content does not match blob SHA", entry.Path)
	}
	return content, nil
}

func blobOID(content string, oidLength int) string {
	var digest hash.Hash = sha1.New()
	if oidLength == 64 {
		digest = sha256.New()
	}
	fmt.Fprintf(digest, "blob %d\x00", len(content))
	io.WriteString(digest, content)
	return hex.EncodeToString(digest.Sum(nil))
}
