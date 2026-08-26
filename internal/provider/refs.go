package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

func validateRefKind(kind RefKind) error {
	if kind != RefKindAll && kind != RefKindBranch && kind != RefKindTag {
		return fmt.Errorf("unsupported ref kind %q", kind)
	}
	return nil
}

func normalizeRefs(refs []Ref) ([]Ref, error) {
	return normalizeRefsContext(context.Background(), refs)
}

func normalizeRefsContext(ctx context.Context, refs []Ref) ([]Ref, error) {
	byName := make(map[string]Ref, len(refs))
	for _, ref := range refs {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if ref.Kind != RefKindBranch && ref.Kind != RefKindTag {
			return nil, fmt.Errorf("provider returned invalid ref kind %q", ref.Kind)
		}
		if ref.Name == "" || ref.Commit == "" {
			return nil, fmt.Errorf("provider returned an incomplete %s ref", ref.Kind)
		}
		if !validRefName(ref.Name) || containsControl(ref.Commit) {
			return nil, fmt.Errorf("provider returned an invalid %s ref", ref.Kind)
		}
		key := string(ref.Kind) + "\x00" + ref.Name
		if previous, ok := byName[key]; ok {
			if previous.Commit != ref.Commit {
				return nil, fmt.Errorf("provider returned conflicting commits for %s ref %q", ref.Kind, ref.Name)
			}
			continue
		}
		byName[key] = ref
	}
	result := make([]Ref, 0, len(byName))
	for _, ref := range byName {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind == RefKindBranch
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].Commit < result[j].Commit
	})
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func validRefName(name string) bool {
	if name == "@" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.Contains(name, "//") || strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == ' ' || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
