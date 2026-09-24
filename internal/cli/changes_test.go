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
	"sync"
	"testing"
	"time"

	"github.com/SamuelSupe/git-rg/internal/proposal"
)

func TestProposeRejectsInvalidPublicationBeforeRemoteRequests(t *testing.T) {
	t.Setenv("GITRG_TOKEN", "read-token-with-possibly-broad-permissions")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid publication must fail before remote requests")
		w.WriteHeader(500)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name, token, body, code string
		flags                   []string
	}{
		{name: "dedicated credential required", code: "provider_init_failed"},
		{name: "agent name required", token: "write-token", code: "invalid_changes", flags: []string{"--agent-model", "model-1"}},
		{name: "agent cannot inject lines", token: "write-token", code: "invalid_changes", flags: []string{"--agent-name", "agent\n```"}},
		{name: "body budget includes identity", token: "write-token", code: "invalid_changes", body: strings.Repeat("x", 64<<10), flags: []string{"--agent-name", "Agent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITRG_WRITE_TOKEN", tc.token)
			content := "hello"
			plan := proposal.Plan{Schema: 1, BaseCommit: baseSHA, BaseBranch: "main", Branch: "agent/test", Title: "Change", CommitMessage: "Change", Body: tc.body,
				Changes: []proposal.Change{{Action: "create", Path: "new.txt", Content: &content}}}
			data, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(t.TempDir(), "changes.json")
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"propose", "--enable-write", "--changes", file, "--api-base", server.URL}, tc.flags...)
			var stdout, stderr bytes.Buffer
			status := Run(append(args, "github:o/r"), &stdout, &stderr)
			var event changeEvent
			if err := json.Unmarshal(stdout.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if status != 2 || event.Code != tc.code {
				t.Fatalf("status=%d event=%+v", status, event)
			}
		})
	}
}

func TestRunProposalWorkflow(t *testing.T) {
	for _, platform := range []string{"github", "gitlab"} {
		for _, scenario := range []string{"success", "commit_reply_lost", "pr_failed", "pr_reply_lost", "base_moved", "branch_conflict", "unexpected_symlink", "blob_conflict", "create_over_symlink", "directory_collision", "directory_to_file", "agent_plan", "agent_flags"} {
			t.Run(platform+"/"+scenario, func(t *testing.T) {
				t.Setenv("GITRG_TOKEN", "read-token")
				t.Setenv("GITRG_WRITE_TOKEN", "write-token")
				api := &proposalAPI{t: t, platform: platform, auth: "read-token", files: map[string]proposalFile{
					"hello.go": {"old\n", "100755"}, "remove.txt": {"obsolete\n", "100644"}, "keep.txt": {"keep\n", "100644"},
					"shortcut": {"keep.txt", "120000"}, "module": {"submodule", "160000"},
				}}
				if scenario == "directory_to_file" {
					api.files["replace/child.txt"] = proposalFile{"old\n", "100644"}
				}
				server := httptest.NewServer(http.HandlerFunc(api.serveHTTP))
				defer server.Close()
				repo := platform + ":o/r"
				readArgs := []string{"read", "--auth", "env", "--api-base", server.URL, repo, "hello.go"}
				var stdout, stderr bytes.Buffer
				if status := Run(readArgs, &stdout, &stderr); status != 0 {
					t.Fatalf("read: %d %s %s", status, &stdout, &stderr)
				}
				var file changeEvent
				if err := json.Unmarshal(stdout.Bytes(), &file); err != nil {
					t.Fatal(err)
				}
				if file.Commit != baseSHA || file.Blob != cliGitBlobOID([]byte("old\n")) || file.Content == nil || *file.Content != "old\n" || file.Mode != "100755" {
					t.Fatalf("file = %+v", file)
				}
				updated, added := "new\n", "added\n"
				plan := proposal.Plan{Schema: 1, BaseCommit: file.Commit, BaseBranch: "main", Branch: "agent/fix", Title: "Fix behavior", Body: "Validation: pending CI", CommitMessage: "Fix behavior", Changes: []proposal.Change{
					{Action: "update", Path: "hello.go", ExpectedBlob: file.Blob, Content: &updated},
					{Action: "create", Path: "new.txt", Content: &added},
					{Action: "delete", Path: "remove.txt", ExpectedBlob: cliGitBlobOID([]byte("obsolete\n"))},
				}}
				if scenario == "directory_to_file" {
					plan.Changes = append(plan.Changes,
						proposal.Change{Action: "delete", Path: "replace/child.txt", ExpectedBlob: cliGitBlobOID([]byte("old\n"))},
						proposal.Change{Action: "create", Path: "replace", Content: &added})
				}
				var wantAgent *proposal.AgentIdentity
				if scenario == "agent_plan" {
					plan.Agent = &proposal.AgentIdentity{Name: "Agent `quoted` <bot>", Model: "model-1", RunID: "run-123"}
					wantAgent = plan.Agent
				}
				planPath := filepath.Join(t.TempDir(), "change.json")
				writePlan := func() {
					data, err := json.Marshal(plan)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(planPath, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				writePlan()
				args := []string{"propose", "--auth", "env", "--api-base", server.URL, "--changes", planPath}
				if scenario == "agent_flags" {
					args = append(args, "--agent-name", "CLI Agent", "--agent-model", "model-2", "--agent-run-id", "run-123")
					wantAgent = &proposal.AgentIdentity{Name: "CLI Agent", Model: "model-2", RunID: "run-123"}
				}
				run := func(extra ...string) (int, []changeEvent) {
					stdout.Reset()
					stderr.Reset()
					command := append(append(append([]string{}, args...), extra...), repo)
					status := Run(command, &stdout, &stderr)
					var events []changeEvent
					decoder := json.NewDecoder(&stdout)
					for {
						var event changeEvent
						if err := decoder.Decode(&event); err != nil {
							if err == io.EOF {
								break
							}
							t.Fatal(err)
						}
						events = append(events, event)
					}
					return status, events
				}
				api.mu.Lock()
				before := api.requests
				api.mu.Unlock()
				status, events := run()
				if status != 2 || events[len(events)-1].Code != "write_disabled" {
					t.Fatalf("default: %d %+v", status, events)
				}
				api.mu.Lock()
				after := api.requests
				api.mu.Unlock()
				if before != after {
					t.Fatal("disabled command made a request")
				}
				status, events = run("--enable-write", "--dry-run")
				if status != 0 || len(events) != 1 || events[0].Type != "preview" || events[0].Diff == "" {
					t.Fatalf("preview: %d %+v", status, events)
				}
				if wantAgent != nil && (events[0].Agent == nil || *events[0].Agent != *wantAgent) {
					t.Fatalf("preview agent = %+v, want %+v", events[0].Agent, wantAgent)
				}
				api.mu.Lock()
				if api.writes != 0 {
					t.Fatal("dry run mutated remote state")
				}
				api.auth, api.scenario = "write-token", scenario
				if scenario == "branch_conflict" {
					api.head = newSHA
					api.message = "someone else's change"
					api.next = api.files
				}
				api.mu.Unlock()
				switch scenario {
				case "blob_conflict":
					plan.Changes[0].ExpectedBlob = strings.Repeat("f", 40)
				case "create_over_symlink":
					plan.Changes[1].Path = "shortcut"
				case "directory_collision":
					plan.Changes[1].Path = "keep.txt/child.txt"
				}
				writePlan()
				status, events = run("--enable-write")
				switch scenario {
				case "base_moved", "branch_conflict", "unexpected_symlink", "blob_conflict", "create_over_symlink", "directory_collision":
					if status != 2 {
						t.Fatalf("conflict accepted: %+v", events)
					}
					api.mu.Lock()
					defer api.mu.Unlock()
					if api.prPosts != 0 || scenario != "unexpected_symlink" && api.writes != 0 {
						t.Fatalf("conflict caused mutations: writes=%d PRs=%d", api.writes, api.prPosts)
					}
					return
				case "commit_reply_lost", "pr_failed", "pr_reply_lost":
					if status != 2 {
						t.Fatalf("uncertain publication should report failure: %+v", events)
					}
					for _, event := range events {
						if event.Result != nil && event.Result.Complete {
							t.Fatal("partial publication marked complete")
						}
					}
					status, events = run("--enable-write")
				}
				if status != 0 {
					t.Fatalf("publish/recovery: status=%d events=%+v stderr=%s", status, events, &stderr)
				}
				result := events[len(events)-1].Result
				if result == nil || !result.Complete || result.Commit != newSHA || result.PullRequest == nil || !result.PullRequest.Draft || result.PullRequest.Number != 9 {
					t.Fatalf("result = %+v", result)
				}
				status, events = run("--enable-write")
				if status != 0 || events[len(events)-1].Result == nil || !events[len(events)-1].Result.Reused {
					t.Fatalf("repeat: %d %+v", status, events)
				}
				if wantAgent != nil {
					api.mu.Lock()
					api.auth = "read-token"
					api.mu.Unlock()
					status, events = run("--dry-run", "--agent-model", "model-override")
					want := *wantAgent
					want.Model = "model-override"
					if status != 0 || events[0].Agent == nil || *events[0].Agent != want {
						t.Fatalf("CLI identity override lost other fields: %d %+v", status, events)
					}
					api.mu.Lock()
					api.auth = "write-token"
					api.mu.Unlock()
					status, events = run("--enable-write", "--agent-run-id", "different-run")
					if status != 2 || events[len(events)-1].Code != "branch_conflict" {
						t.Fatalf("changed identity reused a previous run: %d %+v", status, events)
					}
				}
				api.mu.Lock()
				defer api.mu.Unlock()
				wantPRs := 1
				if scenario == "pr_failed" {
					wantPRs = 2
				}
				if api.commitPosts != 1 || api.prPosts != wantPRs {
					t.Fatalf("duplicate writes: commits=%d PRs=%d", api.commitPosts, api.prPosts)
				}
				if api.next["hello.go"] != (proposalFile{"new\n", "100755"}) || api.next["new.txt"].content != "added\n" || api.next["keep.txt"] != api.files["keep.txt"] || api.next["shortcut"] != api.files["shortcut"] || api.next["module"] != api.files["module"] {
					t.Fatalf("unexpected files: %+v", api.next)
				}
				if _, present := api.next["remove.txt"]; present {
					t.Fatal("deleted file remains")
				}
				if scenario == "directory_to_file" {
					if _, present := api.next["replace/child.txt"]; present || api.next["replace"].content != added {
						t.Fatalf("directory replacement failed: %+v", api.next)
					}
				}
				if wantAgent == nil {
					if api.prBody != plan.Body {
						t.Fatalf("PR body changed without agent identity: %q", api.prBody)
					}
				} else {
					_, metadata, found := strings.Cut(api.prBody, "```json\n")
					metadata, _, _ = strings.Cut(metadata, "\n```")
					var posted proposal.AgentIdentity
					if !found || !strings.HasPrefix(api.prBody, plan.Body) || json.Unmarshal([]byte(metadata), &posted) != nil || posted != *wantAgent {
						t.Fatalf("PR body lost attribution or original description: %q", api.prBody)
					}
				}
			})
		}
	}
}

const baseSHA = "1111111111111111111111111111111111111111"
const newSHA = "2222222222222222222222222222222222222222"
const baseTreeSHA = "3333333333333333333333333333333333333333"
const newTreeSHA = "4444444444444444444444444444444444444444"

type proposalFile struct{ content, mode string }

// This fixture applies the actual HTTP payload to an in-memory repository and
// exposes it through the existing read APIs, so post-write verification is real.
type proposalAPI struct {
	mu                                     sync.Mutex
	t                                      *testing.T
	platform, auth, scenario               string
	files, next                            map[string]proposalFile
	head, message, prBody                  string
	pr                                     map[string]any
	requests, writes, commitPosts, prPosts int
	lostCommitReply, lostPRReply           bool
}

func (a *proposalAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests++
	if r.Header.Get("Authorization") != "Bearer "+a.auth {
		a.t.Errorf("wrong credential for %s %s", r.Method, r.URL.Path)
	}
	if r.Method != http.MethodGet {
		a.writes++
	}
	encode := func(value any) {
		if err := json.NewEncoder(w).Encode(value); err != nil {
			a.t.Error(err)
		}
	}
	decode := func() map[string]any {
		var value map[string]any
		if err := json.NewDecoder(r.Body).Decode(&value); err != nil {
			a.t.Error(err)
		}
		return value
	}
	path := r.URL.Path
	base := "/repos/o/r"
	if a.platform == "gitlab" {
		base = "/projects/7"
	}
	if r.Method == http.MethodGet {
		switch {
		case path == "/repos/o/r" || path == "/projects/o/r":
			encode(map[string]any{"id": 7, "default_branch": "main"})
		case strings.Contains(path, "/git/ref/heads/") || strings.Contains(path, "/repository/branches/"):
			sha := a.head
			if strings.HasSuffix(path, "/main") {
				sha = baseSHA
				if a.scenario == "base_moved" {
					sha = strings.Repeat("e", 40)
				}
			}
			if sha == "" {
				http.NotFound(w, r)
				return
			}
			encode(map[string]any{"object": map[string]string{"sha": sha}, "commit": map[string]string{"id": sha}})
		case strings.Contains(path, "/commits/"):
			sha, tree, message := baseSHA, baseTreeSHA, "original"
			parents := []string{strings.Repeat("0", 40)}
			if strings.HasSuffix(path, newSHA) {
				sha, tree, message, parents = newSHA, newTreeSHA, a.message, []string{baseSHA}
			}
			encode(map[string]any{"sha": sha, "id": sha, "message": message, "parent_ids": parents, "parents": []map[string]string{{"sha": parents[0]}}, "commit": map[string]any{"message": message, "tree": map[string]string{"sha": tree}}})
		case strings.Contains(path, "/git/trees/") || strings.HasSuffix(path, "/repository/tree"):
			files := a.files
			if strings.HasSuffix(path, newTreeSHA) || r.URL.Query().Get("ref") == newSHA {
				files = a.next
			}
			entries := make([]map[string]any, 0, len(files))
			for name, file := range files {
				kind := "blob"
				if file.mode == "160000" {
					kind = "commit"
				}
				oid := cliGitBlobOID([]byte(file.content))
				entries = append(entries, map[string]any{"path": name, "sha": oid, "id": oid, "mode": file.mode, "type": kind, "size": len(file.content)})
			}
			if a.platform == "github" {
				encode(map[string]any{"tree": entries, "truncated": false})
			} else {
				encode(entries)
			}
		case strings.Contains(path, "/blobs/"):
			for _, file := range a.files {
				if strings.Contains(path, cliGitBlobOID([]byte(file.content))) {
					io.WriteString(w, file.content)
					return
				}
			}
			http.NotFound(w, r)
		case path == base+"/pulls" || path == base+"/merge_requests":
			if a.pr == nil {
				encode([]any{})
			} else {
				encode([]any{a.pr})
			}
		default:
			a.t.Errorf("unexpected GET %s", r.URL)
			http.NotFound(w, r)
		}
		return
	}
	body := decode()
	switch path {
	case base + "/git/trees":
		if body["base_tree"] != baseTreeSHA {
			a.t.Error("tree did not preserve base tree")
		}
		if err := a.apply(body["tree"].([]any), false); err != nil {
			w.WriteHeader(400)
			encode(map[string]string{"message": err.Error()})
			return
		}
		encode(map[string]string{"sha": newTreeSHA})
	case base + "/git/commits":
		a.commitPosts++
		parents := body["parents"].([]any)
		if len(parents) != 1 || parents[0] != baseSHA || body["tree"] != newTreeSHA {
			a.t.Error("commit did not use pinned parent/tree")
		}
		a.message = body["message"].(string)
		encode(map[string]string{"sha": newSHA})
	case base + "/git/refs":
		if body["ref"] != "refs/heads/agent/fix" || body["sha"] != newSHA || a.head != "" {
			a.t.Error("not a new branch at the expected commit")
		}
		a.head = newSHA
		if a.loseCommit(w) {
			return
		}
		encode(map[string]string{"ref": "refs/heads/agent/fix"})
	case base + "/repository/commits":
		a.commitPosts++
		if body["branch"] != "agent/fix" || body["start_sha"] != baseSHA || body["force"] != false || a.head != "" {
			a.t.Error("commit did not create a new branch from pinned SHA")
		}
		if err := a.apply(body["actions"].([]any), true); err != nil {
			w.WriteHeader(400)
			encode(map[string]string{"message": err.Error()})
			return
		}
		a.message = body["commit_message"].(string)
		a.head = newSHA
		if a.loseCommit(w) {
			return
		}
		encode(map[string]string{"id": newSHA})
	case base + "/pulls", base + "/merge_requests":
		a.prPosts++
		if a.scenario == "pr_failed" && a.prPosts == 1 {
			w.WriteHeader(503)
			encode(map[string]string{"message": "try later"})
			return
		}
		if a.platform == "github" {
			a.prBody = body["body"].(string)
			if body["draft"] != true || body["head"] != "agent/fix" || body["base"] != "main" {
				a.t.Error("incorrect draft PR request")
			}
			a.pr = map[string]any{"number": 9, "html_url": "https://github.com/o/r/pull/9", "state": "open", "draft": true, "head": map[string]any{"sha": a.head, "ref": "agent/fix", "repo": map[string]string{"full_name": "o/r"}}, "base": map[string]string{"ref": "main"}}
		} else {
			a.prBody = body["description"].(string)
			if !strings.HasPrefix(body["title"].(string), "Draft: ") || body["source_branch"] != "agent/fix" || body["target_branch"] != "main" {
				a.t.Error("incorrect draft MR request")
			}
			a.pr = map[string]any{"iid": 9, "web_url": "https://gitlab.com/o/r/-/merge_requests/9", "state": "opened", "draft": true, "sha": a.head, "source_branch": "agent/fix", "target_branch": "main", "source_project_id": 7, "target_project_id": 7}
		}
		if a.scenario == "pr_reply_lost" && !a.lostPRReply {
			a.lostPRReply = true
			w.WriteHeader(503)
			encode(map[string]string{"message": "reply lost"})
			return
		}
		encode(a.pr)
	default:
		a.t.Errorf("unexpected mutation %s %s", r.Method, path)
		http.NotFound(w, r)
	}
}

func (a *proposalAPI) loseCommit(w http.ResponseWriter) bool {
	if a.scenario != "commit_reply_lost" || a.lostCommitReply {
		return false
	}
	a.lostCommitReply = true
	w.WriteHeader(503)
	fmt.Fprint(w, `{"message":"reply lost after creating branch"}`)
	return true
}

func (a *proposalAPI) apply(items []any, gitlab bool) error {
	a.next = make(map[string]proposalFile, len(a.files))
	for name, file := range a.files {
		a.next[name] = file
	}
	for _, raw := range items {
		item := raw.(map[string]any)
		key := "path"
		if gitlab {
			key = "file_path"
		}
		name := item[key].(string)
		if gitlab && item["action"] != "create" && item["last_commit_id"] != baseSHA {
			a.t.Error("missing file concurrency check")
		}
		if item["action"] == "delete" || !gitlab && item["content"] == nil {
			if _, present := a.next[name]; gitlab && !present {
				return fmt.Errorf("file %q does not exist", name)
			}
			delete(a.next, name)
			continue
		}
		if gitlab && item["action"] == "create" {
			// Gitaly replaces a same-named directory before applying the next action.
			for existing := range a.next {
				if strings.HasPrefix(existing, name+"/") {
					delete(a.next, existing)
				}
			}
		}
		mode := "100644"
		if file, exists := a.next[name]; exists {
			mode = file.mode
		}
		if !gitlab {
			mode = item["mode"].(string)
		}
		a.next[name] = proposalFile{item["content"].(string), mode}
	}
	if a.scenario == "unexpected_symlink" {
		a.next["unexpected"] = proposalFile{"keep.txt", "120000"}
	}
	return nil
}

func TestProposeTimeoutWhileWaitingForInputEOF(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin := os.Stdin
	os.Stdin = read
	defer func() { os.Stdin = stdin; read.Close(); write.Close() }()
	plan := `{"schema_version":1,"base_commit":"1111111111111111111111111111111111111111","base_branch":"main","branch":"agent/test","title":"Change","commit_message":"Change","changes":[{"action":"create","path":"new.txt","content":"hello"}]}`
	if _, err := io.WriteString(write, plan); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("timed out input must not trigger remote requests")
		w.WriteHeader(500)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run([]string{"propose", "--dry-run", "--changes", "-", "--timeout", "50ms", "--auth", "env", "--api-base", server.URL, "github:o/r"}, &stdout, &stderr)
	}()
	select {
	case status := <-done:
		var event changeEvent
		if err := json.Unmarshal(stdout.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if status != 2 || event.Code != "cancelled" {
			t.Fatalf("status=%d event=%+v stderr=%s", status, event, &stderr)
		}
	case <-time.After(2 * time.Second):
		read.Close()
		<-done
		t.Fatal("command did not time out while stdin remained open")
	}
}
