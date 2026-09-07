package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"time"
)

type gitLab struct {
	client *client
}

func newGitLab(repository Repository, token string, timeout time.Duration) *gitLab {
	return newGitLabWithOptions(repository, token, Options{Timeout: timeout})
}

func newGitLabWithOptions(repository Repository, token string, options Options) *gitLab {
	return &gitLab{client: newClient(options, setGitLabHeaders(token), token)}
}

func (g *gitLab) RequestStats() RequestStats { return g.client.stats() }

func (g *gitLab) Resolve(ctx context.Context, repo Repository, requestedRef string) (Snapshot, error) {
	projectPath := repo.APIBase + "/projects/" + url.PathEscape(repo.Project)
	var projectResponse struct {
		ID            int    `json:"id"`
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := g.client.getJSON(ctx, projectPath, &projectResponse); err != nil {
		return Snapshot{}, fmt.Errorf("resolve GitLab project: %w", err)
	}
	if projectResponse.ID == 0 || projectResponse.DefaultBranch == "" {
		return Snapshot{}, errors.New("GitLab project has no repository default branch")
	}
	resolvedRef := requestedRef
	if resolvedRef == "" {
		resolvedRef = projectResponse.DefaultBranch
	}
	var commitResponse struct {
		ID             string `json:"id"`
		AuthorName     string `json:"author_name"`
		AuthorEmail    string `json:"author_email"`
		AuthoredDate   string `json:"authored_date"`
		CommitterName  string `json:"committer_name"`
		CommitterEmail string `json:"committer_email"`
		CommittedDate  string `json:"committed_date"`
		Message        string `json:"message"`
	}
	commitEndpoint := repo.APIBase + "/projects/" + strconv.Itoa(projectResponse.ID) + "/repository/commits/" + url.PathEscape(resolvedRef)
	if _, err := g.client.getJSON(ctx, commitEndpoint, &commitResponse); err != nil {
		return Snapshot{}, fmt.Errorf("resolve GitLab ref %q: %w", resolvedRef, err)
	}
	if commitResponse.ID == "" {
		return Snapshot{}, errors.New("GitLab returned an incomplete commit response")
	}
	return Snapshot{
		Repository:   repo,
		RequestedRef: requestedRef,
		ResolvedRef:  resolvedRef,
		Commit:       commitResponse.ID,
		CommitInfo: &CommitInfo{
			Author: CommitPerson{
				Name:  commitResponse.AuthorName,
				Email: commitResponse.AuthorEmail,
			},
			Committer: CommitPerson{
				Name:  commitResponse.CommitterName,
				Email: commitResponse.CommitterEmail,
			},
			AuthoredAt:  commitResponse.AuthoredDate,
			CommittedAt: commitResponse.CommittedDate,
			Message:     commitResponse.Message,
		},
		TreeOID:       commitResponse.ID,
		DefaultBranch: projectResponse.DefaultBranch,
		RemoteID:      strconv.Itoa(projectResponse.ID),
	}, nil
}

func (g *gitLab) ListRefs(ctx context.Context, repo Repository, kind RefKind) ([]Ref, error) {
	if err := validateRefKind(kind); err != nil {
		return nil, err
	}
	projectPath := repo.APIBase + "/projects/" + url.PathEscape(repo.Project)
	var projectResponse struct {
		ID int `json:"id"`
	}
	if _, err := g.client.getJSON(ctx, projectPath, &projectResponse); err != nil {
		return nil, fmt.Errorf("resolve GitLab project for refs: %w", err)
	}
	if projectResponse.ID == 0 {
		return nil, errors.New("GitLab returned an incomplete project response")
	}
	base := repo.APIBase + "/projects/" + strconv.Itoa(projectResponse.ID) + "/repository"
	refs := make([]Ref, 0)
	budget := newCollectionBudget("GitLab refs", maxRefs, maxRefMetadataBytes)
	pages := newPaginationBudget("GitLab refs")
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

func (g *gitLab) listRefs(ctx context.Context, endpoint string, kind RefKind, budget *collectionBudget, pages *paginationBudget) ([]Ref, error) {
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
				ID string `json:"id"`
			} `json:"commit"`
		}
		headers, err := g.client.getJSON(ctx, endpoint+"?"+values.Encode(), &response)
		if err != nil {
			return nil, fmt.Errorf("list GitLab %s refs page %d: %w", kind, page, err)
		}
		if err := enforcePageSize("GitLab refs", len(response)); err != nil {
			return nil, err
		}
		for _, item := range response {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := budget.add(item.Name, item.Commit.ID); err != nil {
				return nil, err
			}
			refs = append(refs, Ref{Kind: kind, Name: item.Name, Commit: item.Commit.ID})
		}
		next := headers.Get("X-Next-Page")
		if next == "" && len(response) < 100 {
			break
		}
		if next != "" {
			nextPage, err := strconv.Atoi(next)
			if err != nil || nextPage <= page {
				return nil, fmt.Errorf("GitLab returned invalid X-Next-Page %q", next)
			}
			page = nextPage - 1
		}
	}
	return refs, nil
}

func (g *gitLab) ListTree(ctx context.Context, snapshot Snapshot, requireComplete bool) ([]Entry, bool, error) {
	entries := make([]Entry, 0)
	budget := newCollectionBudget("GitLab tree", maxTreeItems, maxTreeMetadataBytes)
	pages := newPaginationBudget("GitLab tree")
	for page := 1; ; page++ {
		if err := pages.take(); err != nil {
			return nil, false, err
		}
		values := url.Values{}
		values.Set("recursive", "true")
		values.Set("ref", snapshot.Commit)
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))
		var response []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
			Type string `json:"type"`
			Mode string `json:"mode"`
		}
		headers, err := g.client.getJSON(ctx, g.projectEndpoint(snapshot)+"/repository/tree?"+values.Encode(), &response)
		if err != nil {
			return nil, false, fmt.Errorf("list GitLab tree page %d: %w", page, err)
		}
		if err := enforcePageSize("GitLab tree", len(response)); err != nil {
			return nil, false, err
		}
		for _, item := range response {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			if err := budget.add(item.Path, item.ID, item.Type, item.Mode); err != nil {
				return nil, false, err
			}
			if item.Type == "blob" && (item.Mode == "100644" || item.Mode == "100755") {
				entries = append(entries, Entry{Path: item.Path, OID: item.ID, Mode: item.Mode})
			}
		}
		next := headers.Get("X-Next-Page")
		if !requireComplete && (next != "" || len(response) == 100) {
			sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			return entries, false, nil
		}
		if next == "" && len(response) < 100 {
			break
		}
		if next != "" {
			nextPage, err := strconv.Atoi(next)
			if err != nil || nextPage <= page {
				return nil, false, fmt.Errorf("GitLab returned invalid X-Next-Page %q", next)
			}
			page = nextPage - 1
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return entries, true, nil
}

func (g *gitLab) OpenBlob(ctx context.Context, snapshot Snapshot, entry Entry) (io.ReadCloser, error) {
	endpoint := g.projectEndpoint(snapshot) + "/repository/blobs/" + url.PathEscape(entry.OID) + "/raw"
	if entry.OID == "" {
		values := url.Values{}
		values.Set("ref", snapshot.Commit)
		values.Set("lfs", "false")
		endpoint = g.projectEndpoint(snapshot) + "/repository/files/" + url.PathEscape(entry.Path) + "/raw?" + values.Encode()
	}
	resp, err := g.client.get(ctx, endpoint, "application/octet-stream")
	if err != nil {
		return nil, fmt.Errorf("read GitLab blob %q: %w", entry.Path, err)
	}
	return resp.Body, nil
}

func (g *gitLab) OpenArchive(ctx context.Context, snapshot Snapshot) (io.ReadCloser, error) {
	values := url.Values{}
	values.Set("sha", snapshot.Commit)
	values.Set("include_lfs_blobs", "false")
	endpoint := g.projectEndpoint(snapshot) + "/repository/archive.tar.gz?" + values.Encode()
	resp, err := g.client.getRaw(ctx, endpoint, "application/octet-stream")
	if err != nil {
		return nil, fmt.Errorf("download GitLab archive: %w", err)
	}
	return resp.Body, nil
}

func (g *gitLab) SearchCandidates(ctx context.Context, snapshot Snapshot, literal string) ([]string, error) {
	if literal == "" {
		return nil, errors.New("indexed search requires a literal prefix")
	}
	paths := make(map[string]struct{})
	budget := newCollectionBudget("GitLab indexed candidates", maxIndexedCandidates, maxCandidatePathBytes)
	for page := 1; page <= maxIndexedPages; page++ {
		values := url.Values{}
		values.Set("scope", "blobs")
		values.Set("search", literal)
		values.Set("ref", snapshot.Commit)
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))
		var response []struct {
			Path string `json:"path"`
		}
		headers, err := g.client.getJSON(ctx, g.projectEndpoint(snapshot)+"/search?"+values.Encode(), &response)
		if err != nil {
			return nil, fmt.Errorf("search GitLab code index: %w", err)
		}
		if err := enforcePageSize("GitLab indexed candidates", len(response)); err != nil {
			return nil, err
		}
		for _, item := range response {
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
		next := headers.Get("X-Next-Page")
		if next == "" && len(response) < 100 {
			break
		}
		if next != "" {
			nextPage, err := strconv.Atoi(next)
			if err != nil || nextPage <= page {
				return nil, fmt.Errorf("GitLab returned invalid X-Next-Page %q", next)
			}
			page = nextPage - 1
		}
	}
	return sortedKeysContext(ctx, paths)
}

func (g *gitLab) projectEndpoint(snapshot Snapshot) string {
	return snapshot.Repository.APIBase + "/projects/" + snapshot.RemoteID
}
