package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type gitHub struct {
	client *client
}

func newGitHub(repository Repository, token string, timeout time.Duration) *gitHub {
	return newGitHubWithOptions(repository, token, Options{Timeout: timeout})
}

func newGitHubWithOptions(repository Repository, token string, options Options) *gitHub {
	return &gitHub{client: newClient(options, setGitHubHeaders(token), token)}
}

func (g *gitHub) RequestStats() RequestStats { return g.client.stats() }

func (g *gitHub) Resolve(ctx context.Context, repo Repository, requestedRef string) (Snapshot, error) {
	owner, name, _ := strings.Cut(repo.Project, "/")
	base := repo.APIBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	var repositoryResponse struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := g.client.getJSON(ctx, base, &repositoryResponse); err != nil {
		return Snapshot{}, fmt.Errorf("resolve GitHub repository: %w", err)
	}
	if repositoryResponse.DefaultBranch == "" {
		return Snapshot{}, errors.New("GitHub repository has no default branch")
	}
	resolvedRef := requestedRef
	if resolvedRef == "" {
		resolvedRef = repositoryResponse.DefaultBranch
	}
	var commitResponse struct {
		SHA    string `json:"sha"`
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
		return Snapshot{}, fmt.Errorf("resolve GitHub ref %q: %w", resolvedRef, err)
	}
	if commitResponse.SHA == "" || commitResponse.Commit.Tree.SHA == "" {
		return Snapshot{}, errors.New("GitHub returned an incomplete commit response")
	}
	commitInfo := &CommitInfo{
		Author: CommitPerson{
			Name:  commitResponse.Commit.Author.Name,
			Email: commitResponse.Commit.Author.Email,
		},
		Committer: CommitPerson{
			Name:  commitResponse.Commit.Committer.Name,
			Email: commitResponse.Commit.Committer.Email,
		},
		AuthoredAt:  commitResponse.Commit.Author.Date,
		CommittedAt: commitResponse.Commit.Committer.Date,
		Message:     commitResponse.Commit.Message,
	}
	if commitResponse.Author != nil {
		commitInfo.Author.Username = commitResponse.Author.Login
	}
	if commitResponse.Committer != nil {
		commitInfo.Committer.Username = commitResponse.Committer.Login
	}
	return Snapshot{
		Repository:    repo,
		RequestedRef:  requestedRef,
		ResolvedRef:   resolvedRef,
		Commit:        commitResponse.SHA,
		CommitInfo:    commitInfo,
		TreeOID:       commitResponse.Commit.Tree.SHA,
		DefaultBranch: repositoryResponse.DefaultBranch,
	}, nil
}

func (g *gitHub) ListRefs(ctx context.Context, repo Repository, kind RefKind) ([]Ref, error) {
	if err := validateRefKind(kind); err != nil {
		return nil, err
	}
	owner, name, _ := strings.Cut(repo.Project, "/")
	base := repo.APIBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	refs := make([]Ref, 0)
	budget := newCollectionBudget("GitHub refs", maxRefs, maxRefMetadataBytes)
	pages := newPaginationBudget("GitHub refs")
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

func (g *gitHub) listRefs(ctx context.Context, endpoint string, kind RefKind, budget *collectionBudget, pages *paginationBudget) ([]Ref, error) {
	refs := make([]Ref, 0)
	for page := 1; ; page++ {
		if err := pages.take(); err != nil {
			return nil, err
		}
		values := url.Values{}
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))
		var response []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if _, err := g.client.getJSON(ctx, endpoint+"?"+values.Encode(), &response); err != nil {
			return nil, fmt.Errorf("list GitHub %s refs page %d: %w", kind, page, err)
		}
		if err := enforcePageSize("GitHub refs", len(response)); err != nil {
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

func (g *gitHub) ListTree(ctx context.Context, snapshot Snapshot, requireComplete bool) ([]Entry, bool, error) {
	endpoint := g.treeEndpoint(snapshot, snapshot.TreeOID) + "?recursive=1"
	requests := newPaginationBudget("GitHub tree")
	if err := requests.take(); err != nil {
		return nil, false, err
	}
	var response gitHubTreeResponse
	if _, err := g.client.getJSON(ctx, endpoint, &response); err != nil {
		return nil, false, fmt.Errorf("list GitHub tree: %w", err)
	}
	budget := newCollectionBudget("GitHub tree", maxTreeItems, maxTreeMetadataBytes)
	if !response.Truncated {
		entries, err := githubEntriesContext(ctx, response.Tree, "", budget)
		return entries, err == nil, err
	}
	if !requireComplete {
		entries, err := githubEntriesContext(ctx, response.Tree, "", budget)
		return entries, false, err
	}
	response.Tree = nil

	type queuedTree struct {
		oid    string
		prefix string
	}
	queue := []queuedTree{{oid: snapshot.TreeOID}}
	entries := make([]Entry, 0, len(response.Tree))
	for head := 0; head < len(queue); head++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		current := queue[head]
		queue[head] = queuedTree{}
		if err := requests.take(); err != nil {
			return nil, false, err
		}
		var subtree gitHubTreeResponse
		if _, err := g.client.getJSON(ctx, g.treeEndpoint(snapshot, current.oid), &subtree); err != nil {
			return nil, false, fmt.Errorf("list GitHub subtree %q: %w", current.prefix, err)
		}
		if subtree.Truncated {
			return nil, false, fmt.Errorf("GitHub non-recursive subtree %q was unexpectedly truncated", current.prefix)
		}
		for _, item := range subtree.Tree {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			itemPath := item.Path
			if current.prefix != "" {
				itemPath = current.prefix + "/" + item.Path
			}
			if err := budget.add(itemPath, item.SHA, item.Mode, item.Type); err != nil {
				return nil, false, err
			}
			switch item.Type {
			case "tree":
				queue = append(queue, queuedTree{oid: item.SHA, prefix: itemPath})
			case "blob":
				if item.Mode == "100644" || item.Mode == "100755" {
					entries = append(entries, Entry{Path: itemPath, OID: item.SHA, Mode: item.Mode, Size: item.Size})
				}
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return entries, true, nil
}

func (g *gitHub) OpenBlob(ctx context.Context, snapshot Snapshot, entry Entry) (io.ReadCloser, error) {
	if entry.Size > MaxGitHubBlobSize {
		return nil, fmt.Errorf("GitHub blob %q is %d bytes; the API limit is %d bytes", entry.Path, entry.Size, MaxGitHubBlobSize)
	}
	owner, name, _ := strings.Cut(snapshot.Repository.Project, "/")
	base := snapshot.Repository.APIBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	endpoint := base + "/git/blobs/" + url.PathEscape(entry.OID)
	if entry.OID == "" {
		values := url.Values{}
		values.Set("ref", snapshot.Commit)
		endpoint = base + "/contents/" + escapeRepositoryPath(entry.Path) + "?" + values.Encode()
	}
	resp, err := g.client.get(ctx, endpoint, "application/vnd.github.raw+json")
	if err != nil {
		return nil, fmt.Errorf("read GitHub blob %q: %w", entry.Path, err)
	}
	return resp.Body, nil
}

func escapeRepositoryPath(value string) string {
	parts := strings.Split(value, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func (g *gitHub) OpenArchive(ctx context.Context, snapshot Snapshot) (io.ReadCloser, error) {
	owner, name, _ := strings.Cut(snapshot.Repository.Project, "/")
	endpoint := snapshot.Repository.APIBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/tarball/" + url.PathEscape(snapshot.Commit)
	resp, err := g.client.getRaw(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("download GitHub archive: %w", err)
	}
	return resp.Body, nil
}

func (g *gitHub) SearchCandidates(ctx context.Context, snapshot Snapshot, literal string) ([]string, error) {
	if literal == "" {
		return nil, errors.New("indexed search requires a literal prefix")
	}
	query := strconv.Quote(literal) + " repo:" + snapshot.Repository.Project
	paths := make(map[string]struct{})
	budget := newCollectionBudget("GitHub indexed candidates", maxIndexedCandidates, maxCandidatePathBytes)
	for page := 1; page <= maxIndexedPages; page++ {
		values := url.Values{}
		values.Set("q", query)
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))
		var response struct {
			Items []struct {
				Path string `json:"path"`
			} `json:"items"`
		}
		if _, err := g.client.getJSON(ctx, snapshot.Repository.APIBase+"/search/code?"+values.Encode(), &response); err != nil {
			return nil, fmt.Errorf("search GitHub code index: %w", err)
		}
		if err := enforcePageSize("GitHub indexed candidates", len(response.Items)); err != nil {
			return nil, err
		}
		for _, item := range response.Items {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := budget.add(item.Path); err != nil {
				return nil, err
			}
			if item.Path != "" {
				paths[item.Path] = struct{}{}
			}
		}
		if len(response.Items) < 100 {
			break
		}
	}
	return sortedKeysContext(ctx, paths)
}

func (g *gitHub) treeEndpoint(snapshot Snapshot, oid string) string {
	owner, name, _ := strings.Cut(snapshot.Repository.Project, "/")
	return snapshot.Repository.APIBase + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/git/trees/" + url.PathEscape(oid)
}

type gitHubTreeResponse struct {
	Truncated bool             `json:"truncated"`
	Tree      []gitHubTreeItem `json:"tree"`
}

type gitHubTreeItem struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

func githubEntriesContext(ctx context.Context, items []gitHubTreeItem, prefix string, budget *collectionBudget) ([]Entry, error) {
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
		if item.Type != "blob" || item.Mode != "100644" && item.Mode != "100755" {
			continue
		}
		entries = append(entries, Entry{Path: itemPath, OID: item.SHA, Mode: item.Mode, Size: item.Size})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func sortedKeysContext(ctx context.Context, values map[string]struct{}) ([]string, error) {
	result := make([]string, 0, len(values))
	for value := range values {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	sort.Strings(result)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
