package proposal

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/SamuelSupe/git-rg/internal/provider"
)

const MaxPlanBytes = 8 << 20
const MaxFileBytes = 1 << 20
const maxContentBytes = 4 << 20
const maxFiles = 100

type Plan struct {
	Schema        int            `json:"schema_version"`
	BaseCommit    string         `json:"base_commit"`
	BaseBranch    string         `json:"base_branch"`
	Branch        string         `json:"branch"`
	Title         string         `json:"title"`
	Body          string         `json:"body,omitempty"`
	CommitMessage string         `json:"commit_message"`
	Changes       []Change       `json:"changes"`
	Agent         *AgentIdentity `json:"agent,omitempty"`
}

type Change struct {
	Action       string  `json:"action"`
	Path         string  `json:"path"`
	ExpectedBlob string  `json:"expected_blob,omitempty"`
	Content      *string `json:"content,omitempty"`
}

func Decode(r io.Reader) (Plan, error) {
	var plan Plan
	data, err := io.ReadAll(io.LimitReader(r, MaxPlanBytes+1))
	if err != nil {
		return plan, err
	}
	if len(data) > MaxPlanBytes {
		return plan, errors.New("change plan exceeds 8 MiB")
	}
	if !utf8.Valid(data) {
		return plan, errors.New("change plan must be UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return plan, errors.New("expected exactly one change plan")
	}
	return plan, plan.Validate()
}

func (p Plan) Validate() error {
	if p.Schema != 1 {
		return errors.New("change plan schema_version must be 1")
	}
	if !validOID(p.BaseCommit) {
		return errors.New("base_commit must be a full lowercase commit SHA")
	}
	if !provider.ValidBranchName(p.BaseBranch) || !provider.ValidBranchName(p.Branch) || p.BaseBranch == p.Branch {
		return errors.New("base_branch and branch must be valid, distinct branch names")
	}
	if strings.TrimSpace(p.Title) == "" || len(p.Title) > 200 || strings.ContainsFunc(p.Title, unicode.IsControl) {
		return errors.New("title must contain 1 to 200 UTF-8 bytes without control characters")
	}
	if err := p.Agent.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(p.CommitMessage) == "" || len(p.CommitMessage) > 16<<10 || strings.ContainsRune(p.CommitMessage, '\x00') {
		return errors.New("commit_message is required (up to 16 KiB)")
	}
	if len(p.pullRequestBody()) > 64<<10 {
		return errors.New("body including agent identity is limited to 64 KiB")
	}
	if len(p.Changes) == 0 || len(p.Changes) > maxFiles {
		return errors.New("a change plan must contain 1 to 100 files")
	}
	seen := make(map[string]bool, len(p.Changes))
	total := 0
	for _, change := range p.Changes {
		if err := ValidatePath(change.Path); err != nil {
			return err
		}
		if seen[change.Path] {
			return fmt.Errorf("duplicate change path %q", change.Path)
		}
		seen[change.Path] = true
		switch change.Action {
		case "create":
			if change.ExpectedBlob != "" {
				return fmt.Errorf("create %q must not specify expected_blob", change.Path)
			}
		case "update", "delete":
			if !validOID(change.ExpectedBlob) {
				return fmt.Errorf("%s %q requires a full expected_blob SHA", change.Action, change.Path)
			}
		default:
			return fmt.Errorf("unsupported action %q; use create, update, or delete", change.Action)
		}
		if change.Action == "delete" {
			if change.Content != nil {
				return fmt.Errorf("delete %q must omit content", change.Path)
			}
		} else {
			if change.Content == nil {
				return fmt.Errorf("%s %q requires content (an empty string is allowed)", change.Action, change.Path)
			}
			if err := validateText(*change.Content); err != nil {
				return fmt.Errorf("%q: %w", change.Path, err)
			}
			total += len(*change.Content)
		}
	}
	if total > maxContentBytes {
		return errors.New("new file contents exceed 4 MiB in total")
	}
	return nil
}

func ValidatePath(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 4096 || !utf8.ValidString(name) || path.IsAbs(name) || path.Clean(name) != name || strings.HasPrefix(name, "../") || strings.ContainsRune(name, '\\') || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("unsupported file path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return errors.New(".git paths cannot be edited")
		}
	}
	return nil
}

func validOID(value string) bool {
	if len(value) != 40 && len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateText(content string) error {
	if len(content) > MaxFileBytes {
		return errors.New("text file exceeds 1 MiB")
	}
	if !utf8.ValidString(content) || strings.ContainsRune(content, '\x00') {
		return errors.New("only UTF-8 text without NUL bytes is supported")
	}
	if strings.HasPrefix(content, "version https://git-lfs.github.com/spec/v1\n") {
		return errors.New("Git LFS pointers are not supported")
	}
	return nil
}

func sortedChanges(changes []Change) []Change {
	result := append([]Change(nil), changes...)
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}
