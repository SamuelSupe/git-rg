package provider

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrIndexedSearchUnsupported = errors.New("provider does not provide an indexed code search API")

type gitee struct {
	client *client
}

func newGitee(repository Repository, token string, timeout time.Duration) *gitee {
	return newGiteeWithOptions(repository, token, Options{Timeout: timeout})
}

func newGiteeWithOptions(repository Repository, token string, options Options) *gitee {
	return &gitee{client: newClient(options, setGiteeHeaders(token), token)}
}

func (g *gitee) RequestStats() RequestStats { return g.client.stats() }

func (g *gitee) SupportsIndexedSearch() bool { return false }

func (g *gitee) Resolve(ctx context.Context, repo Repository, requestedRef string) (Snapshot, error) {
	base := giteeRepositoryEndpoint(repo)
	var repositoryResponse struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := g.client.getJSON(ctx, base, &repositoryResponse); err != nil {
		return Snapshot{}, fmt.Errorf("resolve Gitee repository: %w", err)
	}
	if repositoryResponse.DefaultBranch == "" {
		return Snapshot{}, errors.New("Gitee repository has no default branch")
	}
	resolvedRef := requestedRef
	if resolvedRef == "" {
		resolvedRef = repositoryResponse.DefaultBranch
	}
	var commitResponse struct {
		SHA     string `json:"sha"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
		Committer *struct {
			Login string `json:"login"`
		} `json:"committer"`
		Commit struct {
			Author struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"author"`
			Committer struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"committer"`
			Message string `json:"message"`
			Tree    struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	if _, err := g.client.getJSON(ctx, base+"/commits/"+url.PathEscape(resolvedRef), &commitResponse); err != nil {
		return Snapshot{}, fmt.Errorf("resolve Gitee ref %q: %w", resolvedRef, err)
	}
	if commitResponse.SHA == "" || commitResponse.Commit.Tree.SHA == "" {
		return Snapshot{}, errors.New("Gitee returned an incomplete commit response")
	}
	info := &CommitInfo{
		Author:      CommitPerson{Name: commitResponse.Commit.Author.Name, Email: commitResponse.Commit.Author.Email},
		Committer:   CommitPerson{Name: commitResponse.Commit.Committer.Name, Email: commitResponse.Commit.Committer.Email},
		AuthoredAt:  commitResponse.Commit.Author.Date,
		CommittedAt: commitResponse.Commit.Committer.Date,
		Message:     commitResponse.Commit.Message,
	}
	if commitResponse.Author != nil {
		info.Author.Username = commitResponse.Author.Login
	}
	if commitResponse.Committer != nil {
		info.Committer.Username = commitResponse.Committer.Login
	}
	parents := make([]string, 0, len(commitResponse.Parents))
	for _, parent := range commitResponse.Parents {
		parents = append(parents, parent.SHA)
	}
	return Snapshot{
		Repository: repo, RequestedRef: requestedRef, ResolvedRef: resolvedRef,
		Commit: commitResponse.SHA, Parents: parents, CommitInfo: info,
		TreeOID: commitResponse.Commit.Tree.SHA, DefaultBranch: repositoryResponse.DefaultBranch,
	}, nil
}

func (g *gitee) ListRefs(ctx context.Context, repo Repository, kind RefKind) ([]Ref, error) {
	if err := validateRefKind(kind); err != nil {
		return nil, err
	}
	base := giteeRepositoryEndpoint(repo)
	refs := make([]Ref, 0)
	budget := newCollectionBudget("Gitee refs", maxRefs, maxRefMetadataBytes)
	pages := newPaginationBudget("Gitee refs")
	if kind == RefKindAll || kind == RefKindBranch {
		branches, err := g.listRefs(ctx, base+"/branches", RefKindBranch, budget, pages)
		if err != nil {
			return nil, err
		}
		refs = append(refs, branches...)
	}
	if kind == RefKindAll || kind == RefKindTag {
		tags, err := g.listRefs(ctx, base+"/tags", RefKindTag, budget, pages)
		if err != nil {
			return nil, err
		}
		refs = append(refs, tags...)
	}
	return normalizeRefsContext(ctx, refs)
}

func (g *gitee) listRefs(ctx context.Context, endpoint string, kind RefKind, budget *collectionBudget, pages *paginationBudget) ([]Ref, error) {
	refs := make([]Ref, 0)
	for page := 1; ; page++ {
		if err := pages.take(); err != nil {
			return nil, err
		}
		values := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		var response []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if _, err := g.client.getJSON(ctx, endpoint+"?"+values.Encode(), &response); err != nil {
			return nil, fmt.Errorf("list Gitee %s refs page %d: %w", kind, page, err)
		}
		if err := enforcePageSize("Gitee refs", len(response)); err != nil {
			return nil, err
		}
		for _, item := range response {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := budget.add(item.Name, item.Commit.SHA); err != nil {
				return nil, err
			}
			refs = append(refs, Ref{Kind: kind, Name: item.Name, Commit: item.Commit.SHA})
		}
		if len(response) < 100 {
			break
		}
	}
	return refs, nil
}

type giteeTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

type giteeTreeResponse struct {
	Truncated bool             `json:"truncated"`
	Tree      []giteeTreeEntry `json:"tree"`
}

func (g *gitee) ListTree(ctx context.Context, snapshot Snapshot, requireComplete bool) ([]Entry, bool, error) {
	return g.listTree(ctx, snapshot, requireComplete, false)
}

func (g *gitee) ListChangeTree(ctx context.Context, snapshot Snapshot) ([]Entry, error) {
	entries, _, err := g.listTree(ctx, snapshot, true, true)
	return entries, err
}

func (g *gitee) listTree(ctx context.Context, snapshot Snapshot, requireComplete, allEntries bool) ([]Entry, bool, error) {
	requests := newPaginationBudget("Gitee tree")
	if err := requests.take(); err != nil {
		return nil, false, err
	}
	var response giteeTreeResponse
	endpoint := giteeRepositoryEndpoint(snapshot.Repository) + "/git/trees/" + url.PathEscape(snapshot.TreeOID) + "?recursive=1"
	if _, err := g.client.getJSON(ctx, endpoint, &response); err != nil {
		return nil, false, fmt.Errorf("list Gitee tree: %w", err)
	}
	budget := newCollectionBudget("Gitee tree", maxTreeItems, maxTreeMetadataBytes)
	if !response.Truncated || !requireComplete {
		entries, err := giteeTreeEntries(ctx, response.Tree, "", budget, allEntries)
		return entries, !response.Truncated && err == nil, err
	}

	type queuedTree struct{ oid, prefix string }
	queue := []queuedTree{{oid: snapshot.TreeOID}}
	entries := make([]Entry, 0)
	for head := 0; head < len(queue); head++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		current := queue[head]
		if err := requests.take(); err != nil {
			return nil, false, err
		}
		var subtree giteeTreeResponse
		endpoint := giteeRepositoryEndpoint(snapshot.Repository) + "/git/trees/" + url.PathEscape(current.oid)
		if _, err := g.client.getJSON(ctx, endpoint, &subtree); err != nil {
			return nil, false, fmt.Errorf("list Gitee subtree %q: %w", current.prefix, err)
		}
		if subtree.Truncated {
			return nil, false, fmt.Errorf("Gitee non-recursive subtree %q was unexpectedly truncated", current.prefix)
		}
		for _, item := range subtree.Tree {
			itemPath := item.Path
			if current.prefix != "" {
				itemPath = current.prefix + "/" + item.Path
			}
			if err := budget.add(itemPath, item.SHA, item.Mode, item.Type); err != nil {
				return nil, false, err
			}
			if item.Type == "tree" {
				queue = append(queue, queuedTree{oid: item.SHA, prefix: itemPath})
			} else if allEntries || item.Type == "blob" && (item.Mode == "100644" || item.Mode == "100755") {
				entries = append(entries, Entry{Path: itemPath, OID: item.SHA, Mode: item.Mode, Size: item.Size})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, true, nil
}

func giteeTreeEntries(ctx context.Context, items []giteeTreeEntry, prefix string, budget *collectionBudget, allEntries bool) ([]Entry, error) {
	entries := make([]Entry, 0, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		itemPath := item.Path
		if prefix != "" {
			itemPath = prefix + "/" + item.Path
		}
		if err := budget.add(itemPath, item.SHA, item.Mode, item.Type); err != nil {
			return nil, err
		}
		if item.Type != "tree" && (allEntries || item.Type == "blob" && (item.Mode == "100644" || item.Mode == "100755")) {
			entries = append(entries, Entry{Path: itemPath, OID: item.SHA, Mode: item.Mode, Size: item.Size})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (g *gitee) OpenBlob(ctx context.Context, snapshot Snapshot, entry Entry) (io.ReadCloser, error) {
	values := url.Values{"ref": {snapshot.Commit}}
	rawEndpoint := giteeRepositoryEndpoint(snapshot.Repository) + "/raw/" + escapeRepositoryPath(entry.Path) + "?" + values.Encode()
	resp, err := g.client.get(ctx, rawEndpoint, "application/octet-stream")
	if err == nil {
		size := entry.Size
		if size == 0 {
			size = resp.ContentLength
		}
		return newGitBlobVerifyingReader(resp.Body, entry.OID, size), nil
	}
	if !isNotFound(err) {
		return nil, fmt.Errorf("read Gitee file %q: %w", entry.Path, err)
	}

	var response struct {
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	endpoint := giteeRepositoryEndpoint(snapshot.Repository) + "/git/blobs/" + url.PathEscape(entry.OID)
	if _, err := g.client.getJSON(ctx, endpoint, &response); err != nil {
		return nil, fmt.Errorf("read Gitee blob %q: %w", entry.Path, err)
	}
	if response.SHA != entry.OID || response.Encoding != "base64" {
		return nil, fmt.Errorf("Gitee returned an incomplete blob response for %q", entry.Path)
	}
	content, err := base64.StdEncoding.DecodeString(response.Content)
	if err != nil {
		return nil, fmt.Errorf("decode Gitee blob %q: %w", entry.Path, err)
	}
	if err := verifyGitBlob(content, entry.OID); err != nil {
		return nil, fmt.Errorf("verify Gitee blob %q: %w", entry.Path, err)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

type gitBlobVerifyingReader struct {
	io.ReadCloser
	digest   hash.Hash
	expected string
	size     int64
	read     int64
	terminal error
}

func newGitBlobVerifyingReader(body io.ReadCloser, expected string, size int64) io.ReadCloser {
	digest := gitObjectHash(expected)
	if digest == nil || size < 0 {
		return body
	}
	fmt.Fprintf(digest, "blob %d\x00", size)
	return &gitBlobVerifyingReader{ReadCloser: body, digest: digest, expected: expected, size: size}
}

func (r *gitBlobVerifyingReader) Read(buffer []byte) (int, error) {
	if r.terminal != nil {
		return 0, r.terminal
	}
	n, err := r.ReadCloser.Read(buffer)
	if n > 0 {
		_, _ = r.digest.Write(buffer[:n])
		r.read += int64(n)
	}
	if err != io.EOF {
		return n, err
	}
	r.terminal = io.EOF
	if r.read != r.size || hex.EncodeToString(r.digest.Sum(nil)) != r.expected {
		r.terminal = errors.New("downloaded content does not match blob SHA")
	}
	if n > 0 {
		return n, nil
	}
	return 0, r.terminal
}

func verifyGitBlob(content []byte, expected string) error {
	digest := gitObjectHash(expected)
	if digest == nil {
		return nil
	}
	fmt.Fprintf(digest, "blob %d\x00", len(content))
	_, _ = digest.Write(content)
	if hex.EncodeToString(digest.Sum(nil)) != expected {
		return errors.New("downloaded content does not match blob SHA")
	}
	return nil
}

func gitObjectHash(expected string) hash.Hash {
	switch len(expected) {
	case sha1.Size * 2:
		return sha1.New()
	case sha256.Size * 2:
		return sha256.New()
	default:
		return nil
	}
}

func (g *gitee) OpenArchive(ctx context.Context, snapshot Snapshot) (io.ReadCloser, error) {
	values := url.Values{"ref": {snapshot.Commit}}
	endpoint := giteeRepositoryEndpoint(snapshot.Repository) + "/tarball?" + values.Encode()
	resp, err := g.client.getRaw(ctx, endpoint, "application/octet-stream")
	if err != nil {
		return nil, fmt.Errorf("download Gitee archive: %w", err)
	}
	return resp.Body, nil
}

func (g *gitee) SearchCandidates(context.Context, Snapshot, string) ([]string, error) {
	return nil, ErrIndexedSearchUnsupported
}

func giteeRepositoryEndpoint(repository Repository) string {
	owner, repo, _ := strings.Cut(repository.Project, "/")
	return repository.APIBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}
