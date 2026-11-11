package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGiteeReadOperations(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer gitee-secret" || r.Header.Get("User-Agent") != "git-rg/1" {
			t.Errorf("headers = Authorization %q, User-Agent %q", r.Header.Get("Authorization"), r.Header.Get("User-Agent"))
		}
		switch r.URL.EscapedPath() {
		case "/repos/o/r":
			writeJSON(t, w, map[string]any{"default_branch": "main"})
		case "/repos/o/r/commits/main":
			writeJSON(t, w, map[string]any{
				"sha": "commit", "parents": []map[string]string{{"sha": "parent"}},
				"author": map[string]string{"login": "author-user"}, "committer": map[string]string{"login": "committer-user"},
				"commit": map[string]any{
					"author":    map[string]string{"name": "Author", "email": "a@example.test", "date": "2026-01-02T03:04:05Z"},
					"committer": map[string]string{"name": "Committer", "email": "c@example.test", "date": "2026-01-03T03:04:05Z"},
					"message":   "message", "tree": map[string]string{"sha": "tree"},
				},
			})
		case "/repos/o/r/git/trees/tree":
			if r.URL.Query().Get("recursive") != "1" {
				t.Errorf("recursive = %q", r.URL.Query().Get("recursive"))
			}
			writeJSON(t, w, map[string]any{"truncated": false, "tree": []map[string]any{
				{"path": "script.sh", "mode": "100755", "type": "blob", "sha": "blob", "size": 6},
				{"path": "link", "mode": "120000", "type": "blob", "sha": "link", "size": 3},
				{"path": "pkg", "mode": "40000", "type": "tree", "sha": "subtree"},
			}})
		case "/repos/o/r/raw/script.sh":
			if r.URL.Query().Get("ref") != "commit" {
				t.Errorf("raw ref = %q", r.URL.Query().Get("ref"))
			}
			_, _ = io.WriteString(w, "hello\n")
		case "/repos/o/r/tarball":
			if r.URL.Query().Get("ref") != "commit" || r.Header.Get("Accept-Encoding") != "identity" {
				t.Errorf("archive query/encoding = %q/%q", r.URL.RawQuery, r.Header.Get("Accept-Encoding"))
			}
			_, _ = io.WriteString(w, "archive")
		case "/repos/o/r/branches":
			writeJSON(t, w, []map[string]any{{"name": "main", "commit": map[string]string{"sha": "commit"}}})
		case "/repos/o/r/tags":
			writeJSON(t, w, []map[string]any{{"name": "v1", "commit": map[string]string{"sha": "tag-commit"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := Repository{Provider: "gitee", Host: "gitee.com", Project: "o/r", WebURL: "https://gitee.com/o/r", APIBase: server.URL}
	remote := newGitee(repo, "gitee-secret", time.Second)
	snapshot, err := remote.Resolve(context.Background(), repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Commit != "commit" || snapshot.TreeOID != "tree" || snapshot.DefaultBranch != "main" || !reflect.DeepEqual(snapshot.Parents, []string{"parent"}) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	wantInfo := CommitInfo{
		Author:     CommitPerson{Name: "Author", Email: "a@example.test", Username: "author-user"},
		Committer:  CommitPerson{Name: "Committer", Email: "c@example.test", Username: "committer-user"},
		AuthoredAt: "2026-01-02T03:04:05Z", CommittedAt: "2026-01-03T03:04:05Z", Message: "message",
	}
	if snapshot.CommitInfo == nil || *snapshot.CommitInfo != wantInfo {
		t.Fatalf("commit info = %#v, want %#v", snapshot.CommitInfo, wantInfo)
	}
	entries, complete, err := remote.ListTree(context.Background(), snapshot, true)
	if err != nil || !complete || !reflect.DeepEqual(entries, []Entry{{Path: "script.sh", OID: "blob", Mode: "100755", Size: 6}}) {
		t.Fatalf("tree = %#v/%t/%v", entries, complete, err)
	}
	allEntries, err := remote.ListChangeTree(context.Background(), snapshot)
	if err != nil || len(allEntries) != 2 || allEntries[0].Path != "link" || allEntries[1].Path != "script.sh" {
		t.Fatalf("change tree = %#v/%v", allEntries, err)
	}
	reader, err := remote.OpenBlob(context.Background(), snapshot, entries[0])
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()
	if string(data) != "hello\n" {
		t.Fatalf("blob = %q", data)
	}
	archive, err := remote.OpenArchive(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(archive)
	archive.Close()
	if string(data) != "archive" {
		t.Fatalf("archive = %q", data)
	}
	refs, err := remote.ListRefs(context.Background(), repo, RefKindAll)
	if err != nil || !reflect.DeepEqual(refs, []Ref{{Kind: RefKindBranch, Name: "main", Commit: "commit"}, {Kind: RefKindTag, Name: "v1", Commit: "tag-commit"}}) {
		t.Fatalf("refs = %#v/%v", refs, err)
	}
	if _, err := remote.SearchCandidates(context.Background(), snapshot, "needle"); !errors.Is(err, ErrIndexedSearchUnsupported) {
		t.Fatalf("SearchCandidates() error = %v", err)
	}
	if len(requests) != 8 {
		t.Fatalf("requests = %v", requests)
	}
}

func TestGiteeWriteOperationsAndIssue(t *testing.T) {
	var commitBody, pullBody, issueBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer write-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.Method + " " + r.URL.EscapedPath() {
		case "GET /repos/o/r/branches/missing":
			http.NotFound(w, r)
		case "GET /repos/o/r/branches/agent%2Ffix":
			writeJSON(t, w, map[string]any{"commit": map[string]string{"sha": "new-commit"}})
		case "POST /repos/o/r/commits":
			decodeMap(t, r, &commitBody)
			writeJSON(t, w, map[string]string{"sha": "new-commit"})
		case "GET /repos/o/r/pulls":
			if r.URL.Query().Get("head") != "agent/fix" || r.URL.Query().Get("base") != "main" || r.URL.Query().Get("state") != "all" {
				t.Errorf("pull query = %q", r.URL.RawQuery)
			}
			writeJSON(t, w, []map[string]any{{
				"number": 9, "html_url": "https://gitee.com/o/r/pulls/9", "state": "open", "draft": true,
				"head": map[string]any{"sha": "new-commit", "ref": "agent/fix", "repo": map[string]string{"full_name": "o/r"}},
				"base": map[string]string{"ref": "main"},
			}})
		case "POST /repos/o/r/pulls":
			decodeMap(t, r, &pullBody)
			writeJSON(t, w, map[string]any{"number": 9, "html_url": "https://gitee.com/o/r/pulls/9", "state": "open", "draft": true, "head": map[string]string{"sha": "new-commit"}})
		case "POST /repos/o/issues":
			decodeMap(t, r, &issueBody)
			writeJSON(t, w, map[string]string{"number": "IABC42", "html_url": "https://gitee.com/o/r/issues/IABC42", "state": "open"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	repo := Repository{Provider: "gitee", Project: "o/r", APIBase: server.URL}
	remote := newGiteeWithOptions(repo, "write-token", Options{EnableWrite: true, Timeout: time.Second})
	snapshot := Snapshot{Repository: repo, Commit: "base-commit", TreeOID: "base-tree", DefaultBranch: "main"}
	if head, err := remote.BranchHead(context.Background(), snapshot, "missing"); err != nil || head != "" {
		t.Fatalf("missing branch = %q/%v", head, err)
	}
	if head, err := remote.BranchHead(context.Background(), snapshot, "agent/fix"); err != nil || head != "new-commit" {
		t.Fatalf("branch = %q/%v", head, err)
	}
	commit, err := remote.CreateChange(context.Background(), snapshot, "agent/fix", "main", "message", []FileChange{
		{Action: "update", Path: "script.sh", Content: "new\n", Mode: "100755"},
		{Action: "create", Path: "new.txt", Content: "added\n", Mode: "100644"},
		{Action: "delete", Path: "old.txt", Mode: "100644"},
	})
	if err != nil || commit != "new-commit" {
		t.Fatalf("CreateChange() = %q/%v", commit, err)
	}
	if commitBody["branch"] != "agent/fix" || commitBody["start_branch"] != "main" || commitBody["message"] != "message" {
		t.Fatalf("commit body = %#v", commitBody)
	}
	actions, ok := commitBody["actions"].([]any)
	if !ok || len(actions) != 3 || actions[0].(map[string]any)["action"] != "delete" || actions[1].(map[string]any)["execute_filemode"] != true {
		t.Fatalf("actions = %#v", commitBody["actions"])
	}
	pr, err := remote.FindPullRequest(context.Background(), snapshot, "agent/fix", "main")
	if err != nil || pr == nil || pr.Number != 9 || pr.Head != "new-commit" || !pr.Draft {
		t.Fatalf("FindPullRequest() = %#v/%v", pr, err)
	}
	created, err := remote.CreatePullRequest(context.Background(), snapshot, "agent/fix", "main", "Title", "Body")
	if err != nil || created.Number != 9 || !created.Draft || pullBody["draft"] != true {
		t.Fatalf("CreatePullRequest() = %#v/%v, body %#v", created, err, pullBody)
	}
	issue, err := remote.CreateIssue(context.Background(), repo, "Issue", "Description")
	if err != nil || issue.Identifier != "IABC42" || issue.Number != 0 || issue.State != "open" {
		t.Fatalf("CreateIssue() = %#v/%v", issue, err)
	}
	if issueBody["repo"] != "r" || issueBody["title"] != "Issue" || issueBody["body"] != "Description" {
		t.Fatalf("issue body = %#v", issueBody)
	}
	encoded, _ := json.Marshal(issue)
	if strings.Contains(string(encoded), "number") || !strings.Contains(string(encoded), `"identifier":"IABC42"`) {
		t.Fatalf("issue JSON = %s", encoded)
	}
}

func TestGiteeRejectsInvalidBlobResponse(t *testing.T) {
	for _, response := range []map[string]string{
		{"sha": "other", "encoding": "base64", "content": "aGVsbG8="},
		{"sha": "blob", "encoding": "utf-8", "content": "hello"},
		{"sha": "blob", "encoding": "base64", "content": "not base64"},
	} {
		t.Run(fmt.Sprint(response), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/raw/") {
					http.NotFound(w, r)
					return
				}
				writeJSON(t, w, response)
			}))
			defer server.Close()
			repo := Repository{Project: "o/r", APIBase: server.URL}
			if _, err := newGitee(repo, "", time.Second).OpenBlob(context.Background(), Snapshot{Repository: repo}, Entry{Path: "a", OID: "blob"}); err == nil {
				t.Fatal("OpenBlob() unexpectedly succeeded")
			}
		})
	}
}

func TestGiteeVerifiesRawAndFallbackBlobContent(t *testing.T) {
	const helloOID = "b6fc4c620b67d95f953a5c1c1230aaab5db5a1b0"
	for _, rawAvailable := range []bool{true, false} {
		t.Run(fmt.Sprintf("raw=%t", rawAvailable), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/raw/") {
					if !rawAvailable {
						http.NotFound(w, r)
						return
					}
					_, _ = io.WriteString(w, "world")
					return
				}
				writeJSON(t, w, map[string]string{
					"sha": helloOID, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("world")),
				})
			}))
			defer server.Close()
			repo := Repository{Project: "o/r", APIBase: server.URL}
			reader, err := newGitee(repo, "", time.Second).OpenBlob(context.Background(), Snapshot{Repository: repo, Commit: "commit"}, Entry{Path: "a", OID: helloOID, Size: 5})
			if rawAvailable {
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.ReadAll(reader)
				reader.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "does not match blob SHA") {
				t.Fatalf("verification error = %v", err)
			}
		})
	}
}

func decodeMap(t *testing.T, r *http.Request, out *map[string]any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}
