package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SamuelSupe/git-rg/internal/provider"
	"github.com/SamuelSupe/git-rg/internal/search"
)

func TestRunInvalidArgumentsDefaultToNDJSONErrorEvents(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code string
	}{
		{name: "unknown flag", args: []string{"--unknown", "needle", "github:octocat/Hello-World"}, code: "invalid_arguments"},
		{name: "invalid mode", args: []string{"--mode", "bogus", "needle", "github:octocat/Hello-World"}, code: "invalid_mode"},
		{name: "invalid format", args: []string{"--format", "xml", "needle", "github:octocat/Hello-World"}, code: "invalid_format"},
		{name: "missing positional", args: []string{"needle"}, code: "invalid_arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if status := Run(tt.args, &stdout, &stderr); status != 2 {
				t.Fatalf("Run() status = %d, want 2; stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}
			var event search.Event
			if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &event); err != nil {
				t.Fatalf("stdout is not one NDJSON event: %q (%v)", stdout.String(), err)
			}
			if event.Type != "error" || event.Code != tt.code || event.Message == "" {
				t.Fatalf("error event = %#v, want type=error code=%q", event, tt.code)
			}
		})
	}
}

func TestRunTextDiagnosticsStayOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := Run([]string{"--format", "text", "--mode", "bogus", "needle", "github:octocat/Hello-World"}, &stdout, &stderr)
	if status != 2 {
		t.Fatalf("Run() status = %d, want 2", status)
	}
	if stdout.Len() != 0 {
		t.Fatalf("text stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "git-rg: invalid_mode: unsupported --mode \"bogus\"") {
		t.Fatalf("text stderr = %q, want invalid_mode diagnostic", got)
	}
}

func TestRunAuthWarningIsConsistentForSearchAndRefs(t *testing.T) {
	t.Setenv("GITRG_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	searchServer := newCommitInfoTestServer(t)
	defer searchServer.Close()
	var searchStdout, searchStderr bytes.Buffer
	if status := Run([]string{"--api-base", searchServer.URL, "--no-cache", "--mode", "exact", "needle", "github:octocat/Hello-World"}, &searchStdout, &searchStderr); status != 0 {
		t.Fatalf("search status = %d, stdout=%q stderr=%q", status, searchStdout.String(), searchStderr.String())
	}
	searchLines := strings.Split(strings.TrimSpace(searchStdout.String()), "\n")
	if len(searchLines) < 2 {
		t.Fatalf("search output = %q, want warning before meta", searchStdout.String())
	}
	var searchWarning search.Event
	if err := json.Unmarshal([]byte(searchLines[0]), &searchWarning); err != nil {
		t.Fatalf("search warning is not JSON: %v", err)
	}
	if searchWarning.Type != "warning" || searchWarning.Code != "auth_unavailable" {
		t.Fatalf("search first event = %#v, want auth_unavailable warning", searchWarning)
	}
	if !strings.Contains(searchStderr.String(), "git-rg: auth_unavailable:") {
		t.Fatalf("search stderr = %q, want auth_unavailable diagnostic", searchStderr.String())
	}

	refsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/repos/octocat/Hello-World/branches", "/repos/octocat/Hello-World/tags":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer refsServer.Close()
	var refsStdout, refsStderr bytes.Buffer
	if status := Run([]string{"refs", "--api-base", refsServer.URL, "github:octocat/Hello-World"}, &refsStdout, &refsStderr); status != 0 {
		t.Fatalf("refs status = %d, stdout=%q stderr=%q", status, refsStdout.String(), refsStderr.String())
	}
	refsLines := strings.Split(strings.TrimSpace(refsStdout.String()), "\n")
	if len(refsLines) < 2 {
		t.Fatalf("refs output = %q, want warning before meta", refsStdout.String())
	}
	var refsWarning refsEvent
	if err := json.Unmarshal([]byte(refsLines[0]), &refsWarning); err != nil {
		t.Fatalf("refs warning is not JSON: %v", err)
	}
	if refsWarning.Type != "warning" || refsWarning.Code != "auth_unavailable" {
		t.Fatalf("refs first event = %#v, want auth_unavailable warning", refsWarning)
	}
	if !strings.Contains(refsStderr.String(), "git-rg: auth_unavailable:") {
		t.Fatalf("refs stderr = %q, want auth_unavailable diagnostic", refsStderr.String())
	}
}

func TestRunAuthEnvSuppressesAutomaticWarningForSearchAndRefs(t *testing.T) {
	t.Setenv("GITRG_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	searchServer := newCommitInfoTestServer(t)
	defer searchServer.Close()
	var searchStdout, searchStderr bytes.Buffer
	if status := Run([]string{"--api-base", searchServer.URL, "--auth", "env", "--no-cache", "--mode", "exact", "needle", "github:octocat/Hello-World"}, &searchStdout, &searchStderr); status != 0 {
		t.Fatalf("search status = %d, stdout=%q stderr=%q", status, searchStdout.String(), searchStderr.String())
	}
	if strings.Contains(searchStdout.String(), "auth_unavailable") || searchStderr.Len() != 0 {
		t.Fatalf("search auth env output = stdout %q/stderr %q, want no auth warning", searchStdout.String(), searchStderr.String())
	}

	refsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/repos/octocat/Hello-World/branches", "/repos/octocat/Hello-World/tags":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer refsServer.Close()
	var refsStdout, refsStderr bytes.Buffer
	if status := Run([]string{"refs", "--api-base", refsServer.URL, "--auth", "env", "github:octocat/Hello-World"}, &refsStdout, &refsStderr); status != 0 {
		t.Fatalf("refs status = %d, stdout=%q stderr=%q", status, refsStdout.String(), refsStderr.String())
	}
	if strings.Contains(refsStdout.String(), "auth_unavailable") || refsStderr.Len() != 0 {
		t.Fatalf("refs auth env output = stdout %q/stderr %q, want no auth warning", refsStdout.String(), refsStderr.String())
	}
}

func TestRunSearchPassesConfiguredTokenAndDoesNotRetryUnauthorized(t *testing.T) {
	t.Setenv("GITRG_TOKEN", "known-test-token")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer known-test-token" {
			t.Errorf("Authorization = %q, want Bearer known-test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"unauthorized"}`)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	status := Run([]string{"--api-base", server.URL, "--auth", "env", "--no-cache", "needle", "github:octocat/Hello-World"}, &stdout, &stderr)
	if status != 2 {
		t.Fatalf("search status = %d, stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("unauthorized search requests = %d, want one request without anonymous retry", got)
	}
}

func TestRunRefsPassesConfiguredTokenAndDoesNotRetryUnauthorized(t *testing.T) {
	t.Setenv("GITRG_TOKEN", "known-test-token")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer known-test-token" {
			t.Errorf("Authorization = %q, want Bearer known-test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"unauthorized"}`)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	status := Run([]string{"refs", "--api-base", server.URL, "--auth", "env", "github:octocat/Hello-World"}, &stdout, &stderr)
	if status != 2 {
		t.Fatalf("refs status = %d, stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("unauthorized refs requests = %d, want one request without anonymous retry", got)
	}
}

func TestRunPreflightErrorsDoNotContactRemote(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	tests := []struct {
		name string
		args []string
		code string
	}{
		{
			name: "invalid glob",
			args: []string{"--api-base", server.URL, "--glob", "[", "needle", "github:octocat/Hello-World"},
			code: "invalid_glob",
		},
		{
			name: "indexed pattern without literal prefix",
			args: []string{"--api-base", server.URL, "--mode", "indexed", ".*needle", "github:octocat/Hello-World"},
			code: "indexed_pattern_unsupported",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if status := Run(tt.args, &stdout, &stderr); status != 2 {
				t.Fatalf("Run() status = %d, want 2; stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}
			var event search.Event
			if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &event); err != nil {
				t.Fatalf("stdout is not one NDJSON event: %q (%v)", stdout.String(), err)
			}
			if event.Type != "error" || event.Code != tt.code {
				t.Fatalf("error event = %#v, want code=%q", event, tt.code)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("preflight error requests = %d, want zero", requests)
	}
}

func TestRunVersionPrintsAndSkipsRepositoryArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if status := RunVersion([]string{"--version"}, &stdout, &stderr, "v1.2.3"); status != 0 {
		t.Fatalf("RunVersion() status = %d, want 0; stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if stdout.String() != "git-rg v1.2.3\n" || stderr.Len() != 0 {
		t.Fatalf("version output = stdout %q/stderr %q, want stdout version and empty stderr", stdout.String(), stderr.String())
	}
}

func TestRunCommitInfoControlsNDJSONMeta(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enabled bool
	}{
		{name: "disabled", enabled: false},
		{name: "enabled", enabled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newCommitInfoTestServer(t)
			defer server.Close()

			args := []string{"--api-base", server.URL, "--auth", "env", "--no-cache", "--mode", "exact"}
			if tt.enabled {
				args = append(args, "--commit-info")
			}
			args = append(args, "needle", "github:octocat/Hello-World")
			var stdout, stderr bytes.Buffer
			if status := Run(args, &stdout, &stderr); status != 0 {
				t.Fatalf("Run() status = %d, stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}

			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(lines) < 1 {
				t.Fatalf("stdout = %q, want a meta event", stdout.String())
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(lines[0]), &raw); err != nil {
				t.Fatalf("meta line is not JSON: %q (%v)", lines[0], err)
			}
			var meta search.Event
			if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil {
				t.Fatalf("meta line cannot decode as event: %q (%v)", lines[0], err)
			}
			if meta.Type != "meta" || meta.Commit != "commit-main" {
				t.Fatalf("meta event = %#v", meta)
			}
			_, present := raw["commit_info"]
			if tt.enabled {
				if !present || meta.CommitInfo == nil {
					t.Fatalf("commit-info enabled meta = %s, want commit_info", lines[0])
				}
				if meta.CommitInfo.Author.Username != "octocat" || meta.CommitInfo.Committer.Username != "hubot" {
					t.Fatalf("commit-info logins = %#v, want octocat/hubot", meta.CommitInfo)
				}
			} else if present || meta.CommitInfo != nil {
				t.Fatalf("commit-info disabled meta = %s, want commit_info omitted", lines[0])
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRunSummaryDurationIncludesResolveTime(t *testing.T) {
	const delay = 50 * time.Millisecond
	server := newCommitInfoTestServerWithDelay(t, delay)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	args := []string{"--api-base", server.URL, "--auth", "env", "--no-cache", "--mode", "exact", "needle", "github:octocat/Hello-World"}
	if status := Run(args, &stdout, &stderr); status != 0 {
		t.Fatalf("Run() status = %d, stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	var summary *search.Summary
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var event search.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("output line is not JSON: %q (%v)", line, err)
		}
		if event.Type == "summary" {
			summary = event.Summary
		}
	}
	if summary == nil {
		t.Fatalf("stdout = %q, want summary event", stdout.String())
	}
	if summary.DurationMS < int64(delay/time.Millisecond)/2 {
		t.Fatalf("summary duration = %dms, want resolve delay included (at least %dms)", summary.DurationMS, int64(delay/time.Millisecond)/2)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunReturnsExitTwoOnNDJSONWriteFailure(t *testing.T) {
	server := newCommitInfoTestServer(t)
	defer server.Close()
	var stderr bytes.Buffer
	writeErr := errors.New("output sink failed")
	stdout := &cliFailingWriter{err: writeErr, failOnSummary: true}
	args := []string{"--api-base", server.URL, "--auth", "env", "--no-cache", "--mode", "exact", "needle", "github:octocat/Hello-World"}
	if status := Run(args, stdout, &stderr); status != 2 {
		t.Fatalf("Run() status = %d, want 2; stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, `"type":"meta"`) || !strings.Contains(got, `"type":"match"`) {
		t.Fatalf("successful output before summary failure = %q, want meta and match events", got)
	}
	if !strings.Contains(stderr.String(), "git-rg: write_output: output sink failed") {
		t.Fatalf("stderr = %q, want write_output diagnostic", stderr.String())
	}
}

func TestRunCommitInfoTextOutput(t *testing.T) {
	server := newCommitInfoTestServer(t)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	args := []string{
		"--api-base", server.URL, "--auth", "env",
		"--no-cache",
		"--mode", "exact",
		"--format", "text",
		"--commit-info",
		"needle", "github:octocat/Hello-World",
	}
	if status := Run(args, &stdout, &stderr); status != 0 {
		t.Fatalf("Run() status = %d, stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	want := "commit commit-main\n" +
		"Author: The Octocat <octocat@example.com> (@octocat)\n" +
		"AuthorDate: 2024-01-02T03:04:05Z\n" +
		"Committer: Hubot <hubot@example.com> (@hubot)\n" +
		"CommitDate: 2024-01-03T04:05:06Z\n" +
		"Message: main commit\n" +
		"README.md:1:1:needle\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func newCommitInfoTestServer(t *testing.T) *httptest.Server {
	return newCommitInfoTestServerWithDelay(t, 0)
}

func newCommitInfoTestServerWithDelay(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	archive := makeCommitInfoArchive(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/repos/octocat/Hello-World":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
		case "/repos/octocat/Hello-World/commits/main":
			if delay > 0 {
				time.Sleep(delay)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sha":       "commit-main",
				"author":    map[string]any{"login": "octocat"},
				"committer": map[string]any{"login": "hubot"},
				"commit": map[string]any{
					"author":    map[string]any{"name": "The Octocat", "email": "octocat@example.com", "date": "2024-01-02T03:04:05Z"},
					"committer": map[string]any{"name": "Hubot", "email": "hubot@example.com", "date": "2024-01-03T04:05:06Z"},
					"message":   "main commit",
					"tree":      map[string]any{"sha": "tree-main"},
				},
			})
		case "/repos/octocat/Hello-World/git/trees/tree-main":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"truncated": false,
				"tree": []map[string]any{{
					"path": "README.md",
					"mode": "100644",
					"type": "blob",
					"sha":  cliGitBlobOID([]byte("needle\n")),
					"size": len([]byte("needle\n")),
				}},
			})
		case "/repos/octocat/Hello-World/tarball/commit-main":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
}

func makeCommitInfoArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	content := []byte("needle\n")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "commit-main/README.md", Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return archive.Bytes()
}

func cliGitBlobOID(data []byte) string {
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "blob %d\x00", len(data))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

type cliFailingWriter struct {
	err           error
	failOnSummary bool
	data          bytes.Buffer
}

func (w *cliFailingWriter) Write(data []byte) (int, error) {
	if w.failOnSummary {
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var event search.Event
			if err := json.Unmarshal(line, &event); err == nil && event.Type == "summary" {
				return 0, w.err
			}
		}
	}
	return w.data.Write(data)
}

func (w *cliFailingWriter) String() string {
	return w.data.String()
}

func TestRunRefsDefaultNDJSONAssociatesHeads(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.EscapedPath())
		switch r.URL.EscapedPath() {
		case "/repos/octocat/Hello-World/branches":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "stable", "commit": map[string]any{"sha": "commit-stable"}},
				{"name": "main", "commit": map[string]any{"sha": "commit-shared"}},
			})
		case "/repos/octocat/Hello-World/tags":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "v1.0", "commit": map[string]any{"sha": "commit-shared"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	status := Run([]string{"refs", "--api-base", server.URL, "--auth", "env", "github:octocat/Hello-World"}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("Run(refs) status = %d, stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("refs NDJSON lines = %d, want meta + 3 refs + summary: %q", len(lines), stdout.String())
	}
	events := make([]refsEvent, 0, len(lines))
	for _, line := range lines {
		var event refsEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid refs event %q: %v", line, err)
		}
		events = append(events, event)
	}
	if events[0].Type != "meta" || events[0].Schema != 1 || events[0].Provider != "github" {
		t.Fatalf("meta event = %#v", events[0])
	}
	if events[1].Type != "ref" || events[1].Kind != provider.RefKindBranch || events[1].Name != "main" || events[1].Commit != "commit-shared" || len(events[1].HeadTags) != 1 || events[1].HeadTags[0] != "v1.0" {
		t.Fatalf("main ref event = %#v", events[1])
	}
	if events[2].Type != "ref" || events[2].Kind != provider.RefKindBranch || events[2].Name != "stable" || len(events[2].HeadTags) != 0 {
		t.Fatalf("stable ref event = %#v", events[2])
	}
	if events[3].Type != "ref" || events[3].Kind != provider.RefKindTag || events[3].Name != "v1.0" || events[3].Commit != "commit-shared" || len(events[3].HeadBranches) != 1 || events[3].HeadBranches[0] != "main" {
		t.Fatalf("tag ref event = %#v", events[3])
	}
	if events[4].Type != "summary" || events[4].Summary == nil || events[4].Summary.Branches != 2 || events[4].Summary.Tags != 1 || events[4].Summary.APIRequests != 2 || !events[4].Summary.Complete {
		t.Fatalf("summary event = %#v", events[4])
	}
	if strings.Join(requests, "\n") != "/repos/octocat/Hello-World/branches\n/repos/octocat/Hello-World/tags" {
		t.Fatalf("requests = %v, want branches and tags only", requests)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunRefsKindOnlyRequestsSelectedEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name         string
		kind         provider.RefKind
		path         string
		wantBranches int
		wantTags     int
		wantRefKind  provider.RefKind
		wantRefName  string
	}{
		{name: "branch", kind: provider.RefKindBranch, path: "/repos/octocat/Hello-World/branches", wantBranches: 1, wantRefKind: provider.RefKindBranch, wantRefName: "main"},
		{name: "tag", kind: provider.RefKindTag, path: "/repos/octocat/Hello-World/tags", wantTags: 1, wantRefKind: provider.RefKindTag, wantRefName: "v1.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.URL.EscapedPath())
				switch r.URL.EscapedPath() {
				case "/repos/octocat/Hello-World/branches":
					_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "main", "commit": map[string]any{"sha": "commit-main"}}})
				case "/repos/octocat/Hello-World/tags":
					_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "v1.0", "commit": map[string]any{"sha": "commit-tag"}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			var stdout, stderr bytes.Buffer
			args := []string{"refs", "--api-base", server.URL, "--auth", "env", "--kind", string(tt.kind), "github:octocat/Hello-World"}
			if status := Run(args, &stdout, &stderr); status != 0 {
				t.Fatalf("Run(refs) status = %d, stdout=%q stderr=%q", status, stdout.String(), stderr.String())
			}
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(lines) != 3 {
				t.Fatalf("refs NDJSON lines = %d, want meta + ref + summary: %q", len(lines), stdout.String())
			}
			var refEvent, summaryEvent refsEvent
			if err := json.Unmarshal([]byte(lines[1]), &refEvent); err != nil {
				t.Fatalf("ref event is not JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(lines[2]), &summaryEvent); err != nil {
				t.Fatalf("summary event is not JSON: %v", err)
			}
			if refEvent.Type != "ref" || refEvent.Kind != tt.wantRefKind || refEvent.Name != tt.wantRefName {
				t.Fatalf("ref event = %#v", refEvent)
			}
			if summaryEvent.Summary == nil || summaryEvent.Summary.Branches != tt.wantBranches || summaryEvent.Summary.Tags != tt.wantTags || summaryEvent.Summary.APIRequests != 1 {
				t.Fatalf("summary event = %#v", summaryEvent)
			}
			if len(requests) != 1 || requests[0] != tt.path {
				t.Fatalf("requests = %v, want only %s", requests, tt.path)
			}
		})
	}
}

func TestRunRefsDoesNotEmitPartialRefsOnLaterPageFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/repos/octocat/Hello-World/branches" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			http.Error(w, "page failed", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(cliGitHubRefItems(100))
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	status := Run([]string{"refs", "--api-base", server.URL, "--auth", "env", "--kind", "branch", "github:octocat/Hello-World"}, &stdout, &stderr)
	if status != 2 {
		t.Fatalf("Run(refs) status = %d, want 2; stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want only error event: %q", len(lines), stdout.String())
	}
	var event refsEvent
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("error event is not JSON: %v", err)
	}
	if event.Type != "error" || event.Code != "list_refs_failed" {
		t.Fatalf("error event = %#v, want list_refs_failed", event)
	}
	if strings.Contains(stdout.String(), `"type":"ref"`) || strings.Contains(stdout.String(), `"type":"meta"`) || strings.Contains(stdout.String(), `"type":"summary"`) {
		t.Fatalf("stdout emitted partial refs: %q", stdout.String())
	}
}

func TestRunRefsInvalidKindDoesNotContactRemote(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	status := Run([]string{"refs", "--api-base", server.URL, "--auth", "env", "--kind", "unknown", "github:octocat/Hello-World"}, &stdout, &stderr)
	if status != 2 {
		t.Fatalf("Run(refs) status = %d, want 2", status)
	}
	var event refsEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &event); err != nil {
		t.Fatalf("error event is not JSON: %v", err)
	}
	if event.Type != "error" || event.Code != "invalid_ref_kind" {
		t.Fatalf("error event = %#v, want invalid_ref_kind", event)
	}
	if requests != 0 {
		t.Fatalf("invalid kind requests = %d, want zero", requests)
	}
}

func TestRunRefsResourceLimitFailsBeforeOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/repos/octocat/Hello-World/branches" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode(cliGitHubRefItems(101))
			return
		}
		_ = json.NewEncoder(w).Encode(cliGitHubRefItems(100))
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	status := Run([]string{"refs", "--api-base", server.URL, "--auth", "env", "--kind", "branch", "github:octocat/Hello-World"}, &stdout, &stderr)
	if status != 2 {
		t.Fatalf("Run(refs) status = %d, want 2; stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	var event refsEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &event); err != nil {
		t.Fatalf("error event is not JSON: %v", err)
	}
	if event.Type != "error" || event.Code != "resource_limit" {
		t.Fatalf("error event = %#v, want resource_limit", event)
	}
	if strings.Contains(stdout.String(), `"type":"ref"`) || strings.Contains(stdout.String(), `"type":"meta"`) || strings.Contains(stdout.String(), `"type":"summary"`) {
		t.Fatalf("stdout emitted partial refs: %q", stdout.String())
	}
}

func TestRefNamesByCommitEnforcesAssociationBudgetAndCancellation(t *testing.T) {
	const count = 1001
	refs := make([]provider.Ref, 0, count*2)
	for index := 0; index < count; index++ {
		refs = append(refs,
			provider.Ref{Kind: provider.RefKindBranch, Name: fmt.Sprintf("branch-%04d", index), Commit: "shared"},
			provider.Ref{Kind: provider.RefKindTag, Name: fmt.Sprintf("tag-%04d", index), Commit: "shared"},
		)
	}
	_, _, err := refNamesByCommitContext(context.Background(), refs)
	var limitErr *provider.ResourceLimitError
	if err == nil || !errors.As(err, &limitErr) || refsProviderErrorCode(err) != "resource_limit" {
		t.Fatalf("association error = %v, want resource_limit", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = refNamesByCommitContext(ctx, refs[:2])
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled association error = %v, want context.Canceled", err)
	}
}

func cliGitHubRefItems(count int) []map[string]any {
	items := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("branch-%03d", i)
		commit := fmt.Sprintf("commit-%03d", i)
		items = append(items, map[string]any{"name": name, "commit": map[string]any{"sha": commit}})
	}
	return items
}
