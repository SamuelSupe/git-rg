package proposal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// AgentIdentity is caller-supplied attribution, not the authenticated platform author.
type AgentIdentity struct {
	Name  string `json:"name"`
	Model string `json:"model,omitempty"`
	RunID string `json:"run_id,omitempty"`
}

func (a *AgentIdentity) validate() error {
	if a == nil {
		return nil
	}
	if strings.TrimSpace(a.Name) == "" {
		return errors.New("agent.name is required when agent identity is supplied")
	}
	for _, field := range []struct{ name, value string }{{"name", a.Name}, {"model", a.Model}, {"run_id", a.RunID}} {
		if len(field.value) > 200 || !utf8.ValidString(field.value) || strings.ContainsFunc(field.value, unicode.IsControl) {
			return fmt.Errorf("agent.%s must be at most 200 UTF-8 bytes without control characters", field.name)
		}
	}
	return nil
}

func (p Plan) pullRequestBody() string {
	if p.Agent == nil {
		return p.Body
	}
	body := p.Body
	if body != "" {
		body += "\n\n---\n\n"
	}
	identity, _ := json.MarshalIndent(p.Agent, "", "  ")
	return body + "### Agent (self-reported)\n\n```json\n" + string(identity) + "\n```\n"
}
