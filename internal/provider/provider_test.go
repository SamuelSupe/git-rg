package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitHubResolveUsesDefaultOrRequestedRefAndAuth(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.EscapedPath())
		mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer github-secret" {
			t.Errorf("Authorization = %q, want Bearer github-secret", got)
		}
		if got := r.Header.Get("User-Agent"); got != "git-rg/1" {
			t.Errorf("User-Agent = %q, want git-rg/1", got)
		}
		switch r.URL.EscapedPath() {
		case "/repos/octocat/Hello-World":
			writeJSON(t, w, map[string]any{"default_branch": "main"})
		case "/repos/octocat/Hello-World/commits/main":
			writeJSON(t, w, map[string]any{
				"sha":       "commit-main",
				"author":    map[string]any{"login": "octocat"},
				"committer": map[string]any{"login": "hubot"},
				"commit": map[string]any{
					"author":    map[string]any{"name": "The Octocat", "email": "octocat@example.com", "date": "2024-01-02T03:04:05Z"},
					"committer": map[string]any{"name": "Hubot", "email": "hubot@example.com", "date": "2024-01-03T04:05:06Z"},
					"message":   "main commit\n\nbody",
					"tree":      map[string]any{"sha": "tree-main"},
				},
			})
		case "/repos/octocat/Hello-World/commits/feature%2Funicode":
			writeJSON(t, w, map[string]any{"sha": "commit-feature", "commit": map[string]any{"tree": map[string]any{"sha": "tree-feature"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := Repository{Provider: "github", Host: "github.com", Project: "octocat/Hello-World", APIBase: server.URL, WebURL: "https://github.com/octocat/Hello-World"}
	remote := newGitHub(repo, "github-secret", time.Second)

	got, err := remote.Resolve(context.Background(), repo, "")
	if err != nil {
		t.Fatalf("Resolve(default) error = %v", err)
	}
	if got.ResolvedRef != "main" || got.Commit != "commit-main" || got.TreeOID != "tree-main" || got.DefaultBranch != "main" {
		t.Fatalf("Resolve(default) = %#v", got)
	}
	wantInfo := CommitInfo{
		Author:      CommitPerson{Name: "The Octocat", Email: "octocat@example.com", Username: "octocat"},
		Committer:   CommitPerson{Name: "Hubot", Email: "hubot@example.com", Username: "hubot"},
		AuthoredAt:  "2024-01-02T03:04:05Z",
		CommittedAt: "2024-01-03T04:05:06Z",
		Message:     "main commit\n\nbody",
	}
	if got.CommitInfo == nil || *got.CommitInfo != wantInfo {
		t.Fatalf("Resolve(default) commit info = %#v, want %#v", got.CommitInfo, wantInfo)
	}

	got, err = remote.Resolve(context.Background(), repo, "feature/unicode")
	if err != nil {
		t.Fatalf("Resolve(requested) error = %v", err)
	}
	if got.RequestedRef != "feature/unicode" || got.ResolvedRef != "feature/unicode" || got.Commit != "commit-feature" || got.TreeOID != "tree-feature" {
		t.Fatalf("Resolve(requested) = %#v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	wantRequests := []string{
		"GET /repos/octocat/Hello-World",
		"GET /repos/octocat/Hello-World/commits/main",
		"GET /repos/octocat/Hello-World",
		"GET /repos/octocat/Hello-World/commits/feature%2Funicode",
	}
	if strings.Join(requests, "\n") != strings.Join(wantRequests, "\n") {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
}

func TestGitHubListTreeRecursesAfterTruncation(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		switch {
		case r.URL.RawQuery == "recursive=1":
			writeJSON(t, w, map[string]any{
				"truncated": true,
				"tree":      []map[string]any{{"path": "ignored-by-recursive-response", "type": "blob", "mode": "100644", "sha": "ignored"}},
			})
		case strings.HasSuffix(r.URL.EscapedPath(), "/git/trees/root"):
			writeJSON(t, w, map[string]any{
				"truncated": false,
				"tree": []map[string]any{
					{"path": "README.md", "type": "blob", "mode": "100644", "sha": "readme", "size": 11},
					{"path": "bin", "type": "blob", "mode": "100755", "sha": "bin", "size": 7},
					{"path": "vendor", "type": "blob", "mode": "120000", "sha": "ignored-mode", "size": 3},
					{"path": "pkg", "type": "tree", "mode": "040000", "sha": "pkg-tree"},
				},
			})
		case strings.HasSuffix(r.URL.EscapedPath(), "/git/trees/pkg-tree"):
			writeJSON(t, w, map[string]any{
				"truncated": false,
				"tree": []map[string]any{
					{"path": "z.go", "type": "blob", "mode": "100644", "sha": "z"},
					{"path": "nested", "type": "tree", "mode": "040000", "sha": "nested-tree"},
				},
			})
		case strings.HasSuffix(r.URL.EscapedPath(), "/git/trees/nested-tree"):
			writeJSON(t, w, map[string]any{
				"truncated": false,
				"tree":      []map[string]any{{"path": "a.go", "type": "blob", "mode": "100644", "sha": "a"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := Repository{Provider: "github", Project: "octocat/Hello-World", APIBase: server.URL}
	remote := newGitHub(repo, "", time.Second)
	entries, complete, err := remote.ListTree(context.Background(), Snapshot{Repository: repo, TreeOID: "root"}, true)
	if err != nil {
		t.Fatalf("ListTree() error = %v", err)
	}
	if !complete {
		t.Fatal("ListTree(requireComplete=true) returned incomplete")
	}
	want := []Entry{
		{Path: "README.md", OID: "readme", Mode: "100644", Size: 11},
		{Path: "bin", OID: "bin", Mode: "100755", Size: 7},
		{Path: "pkg/nested/a.go", OID: "a", Mode: "100644"},
		{Path: "pkg/z.go", OID: "z", Mode: "100644"},
	}
	if fmt.Sprint(entries) != fmt.Sprint(want) {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
	wantRequests := []string{
		"/repos/octocat/Hello-World/git/trees/root?recursive=1",
		"/repos/octocat/Hello-World/git/trees/root?",
		"/repos/octocat/Hello-World/git/trees/pkg-tree?",
		"/repos/octocat/Hello-World/git/trees/nested-tree?",
	}
	if strings.Join(requests, "\n") != strings.Join(wantRequests, "\n") {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
}

func TestGitHubListTreeRejectsTruncatedSubtree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "recursive=1" {
			writeJSON(t, w, map[string]any{"truncated": true})
			return
		}
		writeJSON(t, w, map[string]any{"truncated": true, "tree": []map[string]any{{"path": "pkg", "type": "tree", "sha": "pkg"}}})
	}))
	defer server.Close()
	repo := Repository{Provider: "github", Project: "octocat/Hello-World", APIBase: server.URL}
	_, complete, err := newGitHub(repo, "", time.Second).ListTree(context.Background(), Snapshot{Repository: repo, TreeOID: "root"}, true)
	if err == nil || !strings.Contains(err.Error(), "unexpectedly truncated") {
		t.Fatalf("ListTree() error = %v, want unexpected subtree truncation", err)
	}
	if complete {
		t.Fatal("ListTree() reported complete after subtree truncation")
	}
}

func TestGitHubTruncatedTreeReturnsOnePartialResponseWhenIncompleteIsAllowed(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.RawQuery != "recursive=1" {
			t.Errorf("unexpected follow-up request %s", r.URL.String())
		}
		writeJSON(t, w, map[string]any{
			"truncated": true,
			"tree":      []map[string]any{{"path": "partial.go", "type": "blob", "mode": "100644", "sha": "partial"}},
		})
	}))
	defer server.Close()
	repo := Repository{Provider: "github", Project: "octocat/Hello-World", APIBase: server.URL}
	entries, complete, err := newGitHub(repo, "", time.Second).ListTree(context.Background(), Snapshot{Repository: repo, TreeOID: "root"}, false)
	if err != nil {
		t.Fatalf("ListTree(requireComplete=false) error = %v", err)
	}
	if complete || requests != 1 || len(entries) != 1 || entries[0].Path != "partial.go" {
		t.Fatalf("complete/requests/entries = %v/%d/%#v, want false/1/partial.go", complete, requests, entries)
	}
}

func TestGitHubSearchCandidatesPaginatesDeduplicatesAndSorts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != `"needle" repo:octocat/Hello-World` {
			t.Errorf("q = %q", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q", got)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		items := make([]map[string]string, 0)
		if page == 1 {
			for i := 0; i < 100; i++ {
				items = append(items, map[string]string{"path": fmt.Sprintf("pkg/%03d.go", i)})
			}
		} else if page == 2 {
			items = []map[string]string{{"path": "pkg/050.go"}, {"path": "README.md"}, {"path": ""}}
		}
		writeJSON(t, w, map[string]any{"items": items})
	}))
	defer server.Close()
	repo := Repository{Provider: "github", Project: "octocat/Hello-World", APIBase: server.URL}
	got, err := newGitHub(repo, "", time.Second).SearchCandidates(context.Background(), Snapshot{Repository: repo}, "needle")
	if err != nil {
		t.Fatalf("SearchCandidates() error = %v", err)
	}
	if len(got) != 101 || got[0] != "README.md" || got[len(got)-1] != "pkg/099.go" {
		t.Fatalf("SearchCandidates() len/ordering = len %d, first %q, last %q", len(got), got[0], got[len(got)-1])
	}
}

func TestGitHubOpenBlobEnforcesSizeAndAcceptHeader(t *testing.T) {
	var gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		_, _ = io.WriteString(w, "hello\n")
	}))
	defer server.Close()
	repo := Repository{Provider: "github", Project: "octocat/Hello-World", APIBase: server.URL}
	remote := newGitHub(repo, "", time.Second)
	snapshot := Snapshot{Repository: repo}
	reader, err := remote.OpenBlob(context.Background(), snapshot, Entry{Path: "README.md", OID: "blob", Size: 5})
	if err != nil {
		t.Fatalf("OpenBlob() error = %v", err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()
	if string(data) != "hello\n" || gotAccept != "application/vnd.github.raw+json" {
		t.Fatalf("body/Accept = %q/%q", data, gotAccept)
	}
	if _, err := remote.OpenBlob(context.Background(), snapshot, Entry{Path: "large.bin", OID: "large", Size: MaxGitHubBlobSize + 1}); err == nil || !strings.Contains(err.Error(), "API limit") {
		t.Fatalf("large OpenBlob() error = %v, want API limit", err)
	}
}

func TestGitHubOpenRawBlobUsesImmutableCommitAndEscapedPath(t *testing.T) {
	var gotPath, gotQuery, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		gotAccept = r.Header.Get("Accept")
		_, _ = io.WriteString(w, "raw github\n")
	}))
	defer server.Close()
	repo := Repository{Provider: "github", Project: "octocat/Hello-World", APIBase: server.URL}
	snapshot := Snapshot{Repository: repo, Commit: "immutable/commit"}
	reader, err := newGitHub(repo, "", time.Second).OpenBlob(context.Background(), snapshot, Entry{Path: "dir/space name#.txt"})
	if err != nil {
		t.Fatalf("OpenBlob(raw) error = %v", err)
	}
	body, readErr := io.ReadAll(reader)
	reader.Close()
	if readErr != nil || string(body) != "raw github\n" {
		t.Fatalf("raw body/error = %q/%v", body, readErr)
	}
	wantPath := "/repos/octocat/Hello-World/contents/dir/space%20name%23.txt"
	if gotPath != wantPath || gotQuery != "ref=immutable%2Fcommit" || gotAccept != "application/vnd.github.raw+json" {
		t.Fatalf("raw path/query/Accept = %q/%q/%q, want %q/ref=immutable%%2Fcommit/application/vnd.github.raw+json", gotPath, gotQuery, gotAccept, wantPath)
	}
}

func TestGitHubOpenArchiveUsesImmutableCommit(t *testing.T) {
	var gotPath, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAccept = r.Header.Get("Accept")
		_, _ = io.WriteString(w, "archive-by-commit")
	}))
	defer server.Close()
	repo := Repository{Provider: "github", Project: "octocat/Hello-World", APIBase: server.URL}
	snapshot := Snapshot{Repository: repo, ResolvedRef: "main", Commit: "immutable-commit"}
	reader, err := newGitHub(repo, "", time.Second).OpenArchive(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("OpenArchive() error = %v", err)
	}
	body, readErr := io.ReadAll(reader)
	reader.Close()
	if readErr != nil || string(body) != "archive-by-commit" {
		t.Fatalf("archive body/error = %q/%v", body, readErr)
	}
	wantPath := "/repos/octocat/Hello-World/tarball/immutable-commit"
	if gotPath != wantPath || gotAccept != "application/vnd.github+json" {
		t.Fatalf("path/Accept = %q/%q, want %q/application/vnd.github+json", gotPath, gotAccept, wantPath)
	}
}

func TestGitLabResolveAndListTreePagination(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "gitlab-secret" {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		switch {
		case r.URL.EscapedPath() == "/projects/gitlab-org%2Fgitlab-test":
			writeJSON(t, w, map[string]any{"id": 42, "default_branch": "main"})
		case r.URL.EscapedPath() == "/projects/42/repository/commits/release%2F1":
			writeJSON(t, w, map[string]any{
				"id":              "commit-42",
				"author_name":     "GitLab Author",
				"author_email":    "author@gitlab.example",
				"authored_date":   "2024-02-03T04:05:06Z",
				"committer_name":  "GitLab Committer",
				"committer_email": "committer@gitlab.example",
				"committed_date":  "2024-02-04T05:06:07Z",
				"message":         "release commit\n\nnotes",
			})
		case r.URL.EscapedPath() == "/projects/42/repository/tree":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page == 1 {
				w.Header().Set("X-Next-Page", "3")
				items := make([]map[string]string, 0, 100)
				for i := 0; i < 100; i++ {
					items = append(items, map[string]string{"id": fmt.Sprintf("oid-%03d", i), "path": fmt.Sprintf("pkg/%03d.go", i), "type": "blob", "mode": "100644"})
				}
				writeJSON(t, w, items)
			} else if page == 3 {
				writeJSON(t, w, []map[string]string{
					{"id": "script", "path": "script.sh", "type": "blob", "mode": "100755"},
					{"id": "tree", "path": "dir", "type": "tree", "mode": "040000"},
				})
			} else {
				t.Errorf("unexpected tree page %d", page)
				http.Error(w, "unexpected page", http.StatusInternalServerError)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := Repository{Provider: "gitlab", Host: "gitlab.com", Project: "gitlab-org/gitlab-test", APIBase: server.URL, WebURL: "https://gitlab.com/gitlab-org/gitlab-test"}
	remote := newGitLab(repo, "gitlab-secret", time.Second)
	snapshot, err := remote.Resolve(context.Background(), repo, "release/1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if snapshot.RemoteID != "42" || snapshot.Commit != "commit-42" || snapshot.TreeOID != "commit-42" || snapshot.ResolvedRef != "release/1" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	wantInfo := CommitInfo{
		Author:      CommitPerson{Name: "GitLab Author", Email: "author@gitlab.example"},
		Committer:   CommitPerson{Name: "GitLab Committer", Email: "committer@gitlab.example"},
		AuthoredAt:  "2024-02-03T04:05:06Z",
		CommittedAt: "2024-02-04T05:06:07Z",
		Message:     "release commit\n\nnotes",
	}
	if snapshot.CommitInfo == nil || *snapshot.CommitInfo != wantInfo {
		t.Fatalf("snapshot commit info = %#v, want %#v", snapshot.CommitInfo, wantInfo)
	}
	entries, complete, err := remote.ListTree(context.Background(), snapshot, true)
	if err != nil {
		t.Fatalf("ListTree() error = %v", err)
	}
	if !complete {
		t.Fatal("ListTree(requireComplete=true) returned incomplete")
	}
	if len(entries) != 101 || entries[0].Path != "pkg/000.go" || entries[len(entries)-1].Path != "script.sh" {
		t.Fatalf("entries len/ordering = %d/%q/%q", len(entries), entries[0].Path, entries[len(entries)-1].Path)
	}
	if !sort.SliceIsSorted(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path }) {
		t.Fatal("entries are not sorted by path")
	}
	wantRequests := []string{
		"/projects/gitlab-org%2Fgitlab-test?",
		"/projects/42/repository/commits/release%2F1?",
		"/projects/42/repository/tree?page=1&per_page=100&recursive=true&ref=commit-42",
		"/projects/42/repository/tree?page=3&per_page=100&recursive=true&ref=commit-42",
	}
	if strings.Join(requests, "\n") != strings.Join(wantRequests, "\n") {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
}

func TestGitLabListTreeRejectsInvalidNextPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Page", "not-a-number")
		writeJSON(t, w, []map[string]string{{"id": "oid", "path": "a.go", "type": "blob", "mode": "100644"}})
	}))
	defer server.Close()
	repo := Repository{Provider: "gitlab", Project: "group/repo", APIBase: server.URL}
	_, complete, err := newGitLab(repo, "", time.Second).ListTree(context.Background(), Snapshot{Repository: repo, RemoteID: "1", Commit: "commit"}, true)
	if err == nil || !strings.Contains(err.Error(), "invalid X-Next-Page") {
		t.Fatalf("ListTree() error = %v, want invalid X-Next-Page", err)
	}
	if complete {
		t.Fatal("ListTree() reported complete after invalid pagination")
	}
}

func TestGitLabTreeReturnsOnlyFirstPageWhenIncompleteIsAllowed(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("X-Next-Page", "2")
		items := make([]map[string]string, 0, 100)
		for i := 0; i < 100; i++ {
			items = append(items, map[string]string{"id": fmt.Sprintf("oid-%03d", i), "path": fmt.Sprintf("pkg/%03d.go", i), "type": "blob", "mode": "100644"})
		}
		writeJSON(t, w, items)
	}))
	defer server.Close()
	repo := Repository{Provider: "gitlab", Project: "group/repo", APIBase: server.URL}
	entries, complete, err := newGitLab(repo, "", time.Second).ListTree(context.Background(), Snapshot{Repository: repo, RemoteID: "1", Commit: "commit"}, false)
	if err != nil {
		t.Fatalf("ListTree(requireComplete=false) error = %v", err)
	}
	if complete || requests != 1 || len(entries) != 100 {
		t.Fatalf("complete/requests/entries = %v/%d/%d, want false/1/100", complete, requests, len(entries))
	}
}

func TestGitLabOpenArchiveUsesImmutableCommitWithoutLFSBlobs(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.Query()
		_, _ = io.WriteString(w, "archive-by-commit")
	}))
	defer server.Close()
	repo := Repository{Provider: "gitlab", Project: "group/repo", APIBase: server.URL}
	snapshot := Snapshot{Repository: repo, RemoteID: "42", ResolvedRef: "main", Commit: "immutable-commit"}
	reader, err := newGitLab(repo, "", time.Second).OpenArchive(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("OpenArchive() error = %v", err)
	}
	body, readErr := io.ReadAll(reader)
	reader.Close()
	if readErr != nil || string(body) != "archive-by-commit" {
		t.Fatalf("archive body/error = %q/%v", body, readErr)
	}
	if gotPath != "/projects/42/repository/archive.tar.gz" {
		t.Fatalf("archive path = %q", gotPath)
	}
	if gotQuery.Get("sha") != "immutable-commit" || gotQuery.Get("include_lfs_blobs") != "false" {
		t.Fatalf("archive query = %v, want immutable sha and include_lfs_blobs=false", gotQuery)
	}
}

func TestGitLabOpenRawBlobUsesImmutableCommitAndEscapedPath(t *testing.T) {
	var gotPath, gotQuery, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		gotAccept = r.Header.Get("Accept")
		_, _ = io.WriteString(w, "raw gitlab\n")
	}))
	defer server.Close()
	repo := Repository{Provider: "gitlab", Project: "group/repo", APIBase: server.URL}
	snapshot := Snapshot{Repository: repo, RemoteID: "42", Commit: "immutable/commit"}
	reader, err := newGitLab(repo, "", time.Second).OpenBlob(context.Background(), snapshot, Entry{Path: "dir/space name.txt"})
	if err != nil {
		t.Fatalf("OpenBlob(raw) error = %v", err)
	}
	body, readErr := io.ReadAll(reader)
	reader.Close()
	if readErr != nil || string(body) != "raw gitlab\n" {
		t.Fatalf("raw body/error = %q/%v", body, readErr)
	}
	wantPath := "/projects/42/repository/files/dir%2Fspace%20name.txt/raw"
	if gotPath != wantPath || gotQuery != "lfs=false&ref=immutable%2Fcommit" || gotAccept != "application/octet-stream" {
		t.Fatalf("raw path/query/Accept = %q/%q/%q, want %q/lfs=false&ref=immutable%%2Fcommit/application/octet-stream", gotPath, gotQuery, gotAccept, wantPath)
	}
}

func TestGitLabSearchCandidatesPaginatesAndUsesImmutableCommit(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.RawQuery)
		if r.URL.Query().Get("scope") != "blobs" || r.URL.Query().Get("search") != "needle" || r.URL.Query().Get("ref") != "commit-42" {
			t.Errorf("query = %v", r.URL.Query())
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 1 {
			w.Header().Set("X-Next-Page", "2")
			items := make([]map[string]string, 0, 100)
			for i := 0; i < 100; i++ {
				items = append(items, map[string]string{"path": fmt.Sprintf("pkg/%03d.go", i)})
			}
			writeJSON(t, w, items)
			return
		}
		writeJSON(t, w, []map[string]string{{"path": "pkg/000.go"}, {"path": "README.md"}})
	}))
	defer server.Close()
	repo := Repository{Provider: "gitlab", Project: "group/repo", APIBase: server.URL}
	got, err := newGitLab(repo, "", time.Second).SearchCandidates(context.Background(), Snapshot{Repository: repo, Commit: "commit-42", ResolvedRef: "release", RemoteID: "1"}, "needle")
	if err != nil {
		t.Fatalf("SearchCandidates() error = %v", err)
	}
	if len(got) != 101 || got[0] != "README.md" || got[len(got)-1] != "pkg/099.go" {
		t.Fatalf("SearchCandidates() len/ordering = %d/%q/%q", len(got), got[0], got[len(got)-1])
	}
	if len(pages) != 2 {
		t.Fatalf("requested pages = %v", pages)
	}
}

func TestProviderRejectsOversizedSinglePages(t *testing.T) {
	t.Run("GitHub refs", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, githubRefItems("branch", maxPageItems+1))
		}))
		defer server.Close()
		repo := Repository{Provider: "github", Project: "octocat/Hello-World", APIBase: server.URL}
		_, err := newGitHub(repo, "", time.Second).ListRefs(context.Background(), repo, RefKindBranch)
		requireProviderResourceLimit(t, err)
	})

	t.Run("GitHub indexed search", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			items := make([]map[string]string, 0, maxPageItems+1)
			for i := 0; i < maxPageItems+1; i++ {
				items = append(items, map[string]string{"path": fmt.Sprintf("file-%03d.txt", i)})
			}
			writeJSON(t, w, map[string]any{"items": items})
		}))
		defer server.Close()
		repo := Repository{Provider: "github", Project: "octocat/Hello-World", APIBase: server.URL}
		_, err := newGitHub(repo, "", time.Second).SearchCandidates(context.Background(), Snapshot{Repository: repo}, "needle")
		requireProviderResourceLimit(t, err)
	})
}

func TestGitLabIndexedSearchStopsAtTenPages(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < maxIndexedPages {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
		writeJSON(t, w, []map[string]string{})
	}))
	defer server.Close()
	repo := Repository{Provider: "gitlab", Project: "group/repo", APIBase: server.URL}
	got, err := newGitLab(repo, "", time.Second).SearchCandidates(context.Background(), Snapshot{Repository: repo, Commit: "commit", RemoteID: "1"}, "needle")
	if err != nil {
		t.Fatalf("SearchCandidates() error = %v, want bounded ten-page result", err)
	}
	if len(got) != 0 || requests != maxIndexedPages {
		t.Fatalf("result/request count = %v/%d, want empty result after %d pages", got, requests, maxIndexedPages)
	}
}

func TestProviderBudgetsRejectCollectionAndPaginationLimits(t *testing.T) {
	collection := newCollectionBudget("test collection", 1, 32)
	if err := collection.add("first"); err != nil {
		t.Fatalf("first collection item error = %v", err)
	}
	requireProviderResourceLimit(t, collection.add("second"))

	metadata := newCollectionBudget("test metadata", 10, 3)
	if err := metadata.add("abc"); err != nil {
		t.Fatalf("metadata collection item error = %v", err)
	}
	requireProviderResourceLimit(t, metadata.add("d"))

	oversized := newCollectionBudget("test path", 10, int64(MaxRepositoryPathBytes)+1)
	longValue := strings.Repeat("p", MaxRepositoryPathBytes+1)
	requireProviderResourceLimit(t, oversized.add(longValue))

	pages := &paginationBudget{resource: "test pagination", max: 1}
	if err := pages.take(); err != nil {
		t.Fatalf("first pagination request error = %v", err)
	}
	requireProviderResourceLimit(t, pages.take())
}

func TestNormalizeRefsRejectsControlInjection(t *testing.T) {
	for _, name := range []string{"main\tbranch", "main\nbranch", "main\x1b[31m"} {
		t.Run(fmt.Sprintf("name-%x", []byte(name)), func(t *testing.T) {
			_, err := normalizeRefs([]Ref{{Kind: RefKindBranch, Name: name, Commit: "commit"}})
			if err == nil {
				t.Fatalf("normalizeRefs(%q) succeeded, want control rejection", name)
			}
		})
	}
	if _, err := normalizeRefs([]Ref{{Kind: RefKindTag, Name: "release", Commit: "commit\n"}}); err == nil {
		t.Fatal("normalizeRefs() accepted control in commit")
	}
}

func requireProviderResourceLimit(t *testing.T, err error) {
	t.Helper()
	var limitErr *ResourceLimitError
	if err == nil || !errors.As(err, &limitErr) {
		t.Fatalf("error = %v, want ResourceLimitError", err)
	}
}

func TestTokenPrecedence(t *testing.T) {
	t.Setenv("GITRG_TOKEN", "generic")
	t.Setenv("GITHUB_TOKEN", "github")
	t.Setenv("GH_TOKEN", "gh")
	t.Setenv("GITLAB_TOKEN", "gitlab")
	githubCloud := Repository{Provider: "github", APIBase: "https://api.github.com"}
	githubEnterprise := Repository{Provider: "github", APIBase: "https://ghe.example.test/api/v3"}
	gitlabCloud := Repository{Provider: "gitlab", APIBase: "https://gitlab.com/api/v4"}
	gitlabSelfHosted := Repository{Provider: "gitlab", APIBase: "https://git.example.test/api/v4"}
	if got := tokenFor(githubCloud); got != "generic" {
		t.Fatalf("tokenFor(github) = %q, want generic", got)
	}
	t.Setenv("GITRG_TOKEN", "")
	if got := tokenFor(githubCloud); got != "github" {
		t.Fatalf("tokenFor(github) = %q, want github", got)
	}
	t.Setenv("GITHUB_TOKEN", "")
	if got := tokenFor(githubCloud); got != "gh" {
		t.Fatalf("tokenFor(github) = %q, want gh", got)
	}
	if got := tokenFor(gitlabCloud); got != "gitlab" {
		t.Fatalf("tokenFor(gitlab) = %q, want gitlab", got)
	}
	if got := tokenFor(githubEnterprise); got != "" {
		t.Fatalf("tokenFor(githubEnterprise) = %q, want empty", got)
	}
	if got := tokenFor(gitlabSelfHosted); got != "" {
		t.Fatalf("tokenFor(gitlabSelfHosted) = %q, want empty", got)
	}
}

func TestGitHubListRefsPaginatesDeduplicatesAndSorts(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		page := r.URL.Query().Get("page")
		switch r.URL.EscapedPath() {
		case "/repos/octocat/Hello-World/branches":
			switch page {
			case "1":
				items := githubRefItems("branch", 98)
				items = append(items, githubRefItem("shared", "commit-shared"), githubRefItem("shared", "commit-shared"))
				writeJSON(t, w, items)
			case "2":
				writeJSON(t, w, []map[string]any{
					githubRefItem("shared", "commit-shared"),
					githubRefItem("zeta", "commit-zeta"),
				})
			default:
				t.Errorf("unexpected GitHub branches page %q", page)
				http.Error(w, "unexpected page", http.StatusInternalServerError)
			}
		case "/repos/octocat/Hello-World/tags":
			switch page {
			case "1":
				items := githubRefItems("tag", 98)
				items = append(items, githubRefItem("shared-tag", "commit-shared"), githubRefItem("shared-tag", "commit-shared"))
				writeJSON(t, w, items)
			case "2":
				writeJSON(t, w, []map[string]any{
					githubRefItem("shared-tag", "commit-shared"),
					githubRefItem("v2", "commit-v2"),
				})
			default:
				t.Errorf("unexpected GitHub tags page %q", page)
				http.Error(w, "unexpected page", http.StatusInternalServerError)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := Repository{Provider: "github", Project: "octocat/Hello-World", APIBase: server.URL}
	got, err := newGitHub(repo, "", time.Second).ListRefs(context.Background(), repo, RefKindAll)
	if err != nil {
		t.Fatalf("ListRefs() error = %v", err)
	}
	if len(got) != 200 {
		t.Fatalf("ListRefs() len = %d, want 200", len(got))
	}
	if got[0] != (Ref{Kind: RefKindBranch, Name: "branch-000", Commit: "commit-000"}) ||
		got[99] != (Ref{Kind: RefKindBranch, Name: "zeta", Commit: "commit-zeta"}) ||
		got[100] != (Ref{Kind: RefKindTag, Name: "shared-tag", Commit: "commit-shared"}) ||
		got[199] != (Ref{Kind: RefKindTag, Name: "v2", Commit: "commit-v2"}) {
		t.Fatalf("ListRefs() boundary ordering = %#v ... %#v, want stable branch/tag ordering", got[0], got[199])
	}
	if countRef(got, RefKindBranch, "shared") != 1 || countRef(got, RefKindTag, "shared-tag") != 1 {
		t.Fatalf("ListRefs() duplicate counts = branch shared %d/tag shared-tag %d, want one each", countRef(got, RefKindBranch, "shared"), countRef(got, RefKindTag, "shared-tag"))
	}
	wantRequests := []string{
		"/repos/octocat/Hello-World/branches?page=1&per_page=100",
		"/repos/octocat/Hello-World/branches?page=2&per_page=100",
		"/repos/octocat/Hello-World/tags?page=1&per_page=100",
		"/repos/octocat/Hello-World/tags?page=2&per_page=100",
	}
	if strings.Join(requests, "\n") != strings.Join(wantRequests, "\n") {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
}

func TestGitLabListRefsResolvesProjectAndUsesNextPage(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		switch r.URL.EscapedPath() {
		case "/projects/gitlab-org%2Fgitlab-test":
			writeJSON(t, w, map[string]any{"id": 42})
		case "/projects/42/repository/branches":
			page := r.URL.Query().Get("page")
			switch page {
			case "1":
				w.Header().Set("X-Next-Page", "3")
				writeJSON(t, w, gitlabRefItems("branch", 100))
			case "3":
				writeJSON(t, w, []map[string]any{gitlabRefItem("release", "commit-release")})
			default:
				t.Errorf("unexpected GitLab branches page %q", page)
				http.Error(w, "unexpected page", http.StatusInternalServerError)
			}
		case "/projects/42/repository/tags":
			writeJSON(t, w, []map[string]any{gitlabRefItem("v1", "commit-release")})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := Repository{Provider: "gitlab", Project: "gitlab-org/gitlab-test", APIBase: server.URL}
	got, err := newGitLab(repo, "", time.Second).ListRefs(context.Background(), repo, RefKindAll)
	if err != nil {
		t.Fatalf("ListRefs() error = %v", err)
	}
	if len(got) != 102 || got[100] != (Ref{Kind: RefKindBranch, Name: "release", Commit: "commit-release"}) || got[101] != (Ref{Kind: RefKindTag, Name: "v1", Commit: "commit-release"}) {
		t.Fatalf("ListRefs() len/boundaries = %d/%#v/%#v, want 102/release/v1", len(got), got[100], got[101])
	}
	wantRequests := []string{
		"/projects/gitlab-org%2Fgitlab-test?",
		"/projects/42/repository/branches?page=1&per_page=100",
		"/projects/42/repository/branches?page=3&per_page=100",
		"/projects/42/repository/tags?page=1&per_page=100",
	}
	if strings.Join(requests, "\n") != strings.Join(wantRequests, "\n") {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
}

func githubRefItems(prefix string, count int) []map[string]any {
	items := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		items = append(items, githubRefItem(fmt.Sprintf("%s-%03d", prefix, i), fmt.Sprintf("commit-%03d", i)))
	}
	return items
}

func githubRefItem(name, commit string) map[string]any {
	return map[string]any{"name": name, "commit": map[string]any{"sha": commit}}
}

func gitlabRefItems(prefix string, count int) []map[string]any {
	items := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		items = append(items, gitlabRefItem(fmt.Sprintf("%s-%03d", prefix, i), fmt.Sprintf("commit-%03d", i)))
	}
	return items
}

func gitlabRefItem(name, commit string) map[string]any {
	return map[string]any{"name": name, "commit": map[string]any{"id": commit}}
}

func countRef(refs []Ref, kind RefKind, name string) int {
	count := 0
	for _, ref := range refs {
		if ref.Kind == kind && ref.Name == name {
			count++
		}
	}
	return count
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode JSON response: %v", err)
	}
}
