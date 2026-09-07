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
	segments []string
	negative bool
	basename bool
	literal  bool
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
		segments := strings.Split(raw, "/")
		for _, segment := range segments {
			if segment == "**" {
				continue
			}
			if _, err := path.Match(segment, "validate"); err != nil {
				return nil, fmt.Errorf("invalid glob %q: %w", raw, err)
			}
		}
		set.patterns = append(set.patterns, globPattern{
			pattern: raw, segments: segments, negative: negative,
			basename: len(segments) == 1, literal: !strings.ContainsAny(raw, "*?[\\"),
		})
	}
	return set, nil
}

func (g *GlobSet) Match(filePath string) bool {
	allowed := !g.hasPositive
	// Keep scratch local so a compiled set remains safe for concurrent scans.
	var storage [16]string
	var segments []string
	for _, pattern := range g.patterns {
		candidate := filePath
		if pattern.basename {
			candidate = path.Base(filePath)
		}
		var matched bool
		switch {
		case pattern.literal:
			matched = pattern.pattern == candidate
		case pattern.basename:
			if pattern.pattern == "**" {
				matched = true
			} else {
				matched, _ = path.Match(pattern.pattern, candidate)
			}
		default:
			if segments == nil {
				segments = storage[:0]
				for segment := range strings.SplitSeq(filePath, "/") {
					segments = append(segments, segment)
				}
			}
			matched = matchSegments(pattern.segments, segments)
		}
		if matched {
			allowed = !pattern.negative
		}
	}
	return allowed
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
