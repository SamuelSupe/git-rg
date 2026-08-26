package search

import (
	"fmt"
	"path"
	"strings"
)

type GlobSet struct {
	patterns    []globPattern
	hasPositive bool
}

type globPattern struct {
	pattern  string
	negative bool
	basename bool
}

func CompileGlobs(patterns []string) (*GlobSet, error) {
	set := &GlobSet{}
	for _, raw := range patterns {
		negative := strings.HasPrefix(raw, "!")
		if negative {
			raw = strings.TrimPrefix(raw, "!")
		} else {
			set.hasPositive = true
		}
		raw = strings.TrimPrefix(strings.TrimSpace(raw), "/")
		if raw == "" {
			return nil, fmt.Errorf("glob pattern is empty")
		}
		for _, segment := range strings.Split(raw, "/") {
			if segment == "**" {
				continue
			}
			if _, err := path.Match(segment, "validate"); err != nil {
				return nil, fmt.Errorf("invalid glob %q: %w", raw, err)
			}
		}
		set.patterns = append(set.patterns, globPattern{pattern: raw, negative: negative, basename: !strings.Contains(raw, "/")})
	}
	return set, nil
}

func (g *GlobSet) Match(filePath string) bool {
	allowed := !g.hasPositive
	for _, pattern := range g.patterns {
		candidate := filePath
		if pattern.basename {
			candidate = path.Base(filePath)
		}
		if matchGlob(pattern.pattern, candidate) {
			allowed = !pattern.negative
		}
	}
	return allowed
}

func matchGlob(pattern, value string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(value, "/"))
}

func matchSegments(pattern, value []string) bool {
	current := make([]bool, len(value)+1)
	next := make([]bool, len(value)+1)
	current[0] = true
	for _, segment := range pattern {
		clear(next)
		if segment == "**" {
			next[0] = current[0]
			for index := 1; index <= len(value); index++ {
				next[index] = current[index] || next[index-1]
			}
		} else {
			for index, candidate := range value {
				if !current[index] {
					continue
				}
				next[index+1], _ = path.Match(segment, candidate)
			}
		}
		current, next = next, current
	}
	return current[len(value)]
}
