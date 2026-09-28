package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SamuelSupe/git-rg/internal/agent"
)

func TestRunIssuePreviewAndCreate(t *testing.T) {
	for _, tc := range []struct {
		platform, repository, endpoint string
		identity                       bool
	}{
		{"github", "github:o/r", "/repos/o/r/issues", false},
		{"github", "https://github.example.test/o/r", "/repos/o/r/issues", true},
		{"gitlab", "gitlab:group/sub/r", "/projects/group%2Fsub%2Fr/issues", true},
		{"gitlab", "https://gitlab.example.test/group/sub/r", "/projects/group%2Fsub%2Fr/issues", false},
	} {
		t.Run(tc.repository, func(t *testing.T) {
			t.Setenv("GITRG_TOKEN", "read-token")
			t.Setenv("GITRG_WRITE_TOKEN", "write-token")
			title, body := "创建 issue", "## Reproduction\n\nActual: 错误\nExpected: success\n"
			bodyFile := filepath.Join(t.TempDir(), "issue.md")
			if err := os.WriteFile(bodyFile, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			var previewBody string
			var requests atomic.Int32
			issueURL := "https://example.test/o/r/issues/42"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "POST" || r.URL.EscapedPath() != tc.endpoint || r.Header.Get("Authorization") != "Bearer write-token" {
					t.Errorf("unexpected request: %s %s auth=%q", r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"))
				}
				var payload map[string]string
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				bodyKey := "body"
				response := map[string]any{"number": 42, "html_url": issueURL, "state": "open"}
				if tc.platform == "gitlab" {
					bodyKey = "description"
					response = map[string]any{"id": 900, "iid": 42, "web_url": issueURL, "state": "opened"}
				}
				if len(payload) != 2 || payload["title"] != title || payload[bodyKey] != previewBody {
					t.Errorf("payload differs from preview: %+v", payload)
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			args := []string{"issue", "create", "--provider", tc.platform, "--api-base", server.URL, "--title", title, "--body-file", bodyFile}
			wantAgent := agent.Identity{Name: "Issue Agent", Model: "model-1", RunID: "run-42"}
			if tc.identity {
				args = append(args, "--agent-name", wantAgent.Name, "--agent-model", wantAgent.Model, "--agent-run-id", wantAgent.RunID)
			}
			run := func(extra ...string) (int, issueEvent) {
				t.Helper()
				command := append(append(append([]string{}, args...), extra...), tc.repository)
				var stdout, stderr bytes.Buffer
				status := Run(command, &stdout, &stderr)
				var event issueEvent
				if err := json.Unmarshal(stdout.Bytes(), &event); err != nil {
					t.Fatalf("expected one issue event: %v; stdout=%s stderr=%s", err, &stdout, &stderr)
				}
				return status, event
			}
			if status, event := run(); status != 2 || event.Code != "write_disabled" || requests.Load() != 0 {
				t.Fatalf("default write gate: status=%d event=%+v requests=%d", status, event, requests.Load())
			}
			t.Setenv("GITRG_TOKEN", "")
			t.Setenv("GITRG_WRITE_TOKEN", "")
			status, preview := run("--dry-run", "--enable-write")
			if status != 0 || preview.Type != "preview" || preview.Schema != 1 || preview.Title != title || requests.Load() != 0 {
				t.Fatalf("offline preview: status=%d event=%+v requests=%d", status, preview, requests.Load())
			}
			previewBody = preview.Body
			if tc.identity {
				_, metadata, found := strings.Cut(preview.Body, "```json\n")
				metadata, _, _ = strings.Cut(metadata, "\n```")
				var posted agent.Identity
				if !found || !strings.HasPrefix(preview.Body, body) || json.Unmarshal([]byte(metadata), &posted) != nil || posted != wantAgent || preview.Agent == nil || *preview.Agent != wantAgent {
					t.Fatalf("preview lost body or attribution: %+v", preview)
				}
			} else if preview.Body != body {
				t.Fatalf("body changed: %q", preview.Body)
			}
			t.Setenv("GITRG_TOKEN", "read-token")
			t.Setenv("GITRG_WRITE_TOKEN", "write-token")
			status, event := run("--enable-write")
			if status != 0 || event.Type != "issue" || event.Result == nil || !event.Result.Complete || event.Result.Number != 42 || event.Result.URL != issueURL || event.Result.State != "open" || requests.Load() != 1 {
				t.Fatalf("create: status=%d event=%+v requests=%d", status, event, requests.Load())
			}
		})
	}
}

func TestRunIssueRejectsInvalidInputBeforeRequests(t *testing.T) {
	largeFile := filepath.Join(t.TempDir(), "too-large.md")
	if err := os.WriteFile(largeFile, bytes.Repeat([]byte("x"), maxIssueBodyBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid issue reached remote API")
		w.WriteHeader(500)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, token, code string
		flags             []string
	}{
		{"disabled before stdin", "write-token", "write_disabled", []string{"--enable-write=false", "--body-file", "-"}},
		{"missing token", "", "provider_init_failed", nil},
		{"invalid token", "bad token", "provider_init_failed", nil},
		{"empty title", "write-token", "invalid_issue", []string{"--title", " "}},
		{"title control", "write-token", "invalid_issue", []string{"--title", "bad\nline"}},
		{"title utf8", "write-token", "invalid_issue", []string{"--title", "\xff"}},
		{"title budget", "write-token", "invalid_issue", []string{"--title", strings.Repeat("x", 201)}},
		{"body sources conflict", "write-token", "invalid_arguments", []string{"--body", "", "--body-file", largeFile}},
		{"empty filename", "write-token", "invalid_arguments", []string{"--body-file", ""}},
		{"body utf8", "write-token", "invalid_issue", []string{"--body", "\xff"}},
		{"body NUL", "write-token", "invalid_issue", []string{"--body", "bad\x00body"}},
		{"file budget", "write-token", "invalid_issue", []string{"--body-file", largeFile}},
		{"identity body budget", "write-token", "invalid_issue", []string{"--body", strings.Repeat("x", maxIssueBodyBytes), "--agent-name", "Agent"}},
		{"identity requires name", "write-token", "invalid_issue", []string{"--agent-model", "model-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITRG_TOKEN", "read-token")
			t.Setenv("GITRG_WRITE_TOKEN", tc.token)
			args := append([]string{"issue", "create", "--enable-write", "--api-base", server.URL, "--title", "Issue"}, tc.flags...)
			var stdout, stderr bytes.Buffer
			status := Run(append(args, "github:o/r"), &stdout, &stderr)
			var event issueEvent
			if err := json.Unmarshal(stdout.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if status != 2 || event.Code != tc.code {
				t.Fatalf("status=%d event=%+v", status, event)
			}
		})
	}
}

func TestRunIssueDoesNotRetryUnconfirmedCreation(t *testing.T) {
	for _, platform := range []string{"github", "gitlab"} {
		for _, tc := range []struct {
			name, response, code string
			status               int
		}{
			{"unauthorized", `{"message":"write-token"}`, "issue_create_failed", 401},
			{"issues disabled", `{"message":"disabled"}`, "issue_create_failed", 403},
			{"rate limited", `{"message":"rate limited"}`, "issue_create_failed", 429},
			{"server error", `{"message":"write-token"}`, "issue_creation_uncertain", 503},
			{"redirect", "", "issue_creation_uncertain", 307},
			{"invalid json", "not JSON", "issue_creation_uncertain", 201},
			{"incomplete result", `{}`, "issue_creation_uncertain", 201},
			{"lost reply", "", "issue_creation_uncertain", 0},
		} {
			t.Run(platform+"/"+tc.name, func(t *testing.T) {
				t.Setenv("GITRG_WRITE_TOKEN", "write-token")
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Method != "POST" {
						t.Errorf("unexpected method: %s", r.Method)
					}
					if tc.status == 0 {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						connection.Close()
						return
					}
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.response)
				}))
				defer server.Close()
				var stdout, stderr bytes.Buffer
				status := Run([]string{"issue", "create", "--enable-write", "--title", "Issue", "--api-base", server.URL, platform + ":o/r"}, &stdout, &stderr)
				var event issueEvent
				if err := json.Unmarshal(stdout.Bytes(), &event); err != nil {
					t.Fatal(err)
				}
				if status != 2 || event.Code != tc.code || requests.Load() != 1 || strings.Contains(stdout.String()+stderr.String(), "write-token") {
					t.Fatalf("status=%d event=%+v requests=%d stderr=%q", status, event, requests.Load(), &stderr)
				}
			})
		}
	}
}

func TestRunIssueBodyStdinHonorsTimeout(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin := os.Stdin
	os.Stdin = read
	defer func() { os.Stdin = stdin; read.Close(); write.Close() }()
	if _, err := fmt.Fprint(write, "a body without EOF"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"issue", "create", "--dry-run", "--title", "Issue", "--body-file", "-", "--timeout", "50ms", "github:o/r"}, &stdout, &stderr)
	}()
	select {
	case status := <-done:
		var event issueEvent
		if err := json.Unmarshal(stdout.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if status != 2 || event.Code != "cancelled" {
			t.Fatalf("status=%d event=%+v", status, event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("issue input ignored overall timeout")
	}
}
