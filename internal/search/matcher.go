package search

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxLineBytes = 8 << 20
const maxBufferedContextBytes = 32 << 20
const maxSubmatchesPerLine = 100_000

var errInvalidUTF8 = errors.New("content is not valid UTF-8")
var errLineTooLong = errors.New("line exceeds maximum size")
var errContextWindowTooLarge = errors.New("before-context window exceeds maximum size")
var errSubmatchLimit = errors.New("line exceeds maximum submatch count")

type MatcherConfig struct {
	Pattern    string
	Fixed      bool
	IgnoreCase bool
	Word       bool
	Before     int
	After      int
}

type Matcher struct {
	regexp  *regexp.Regexp
	literal string
	word    bool
	before  int
	after   int
}

type FileResult struct {
	Events   []Event
	Matches  int
	HitLimit bool
}

func NewMatcher(config MatcherConfig) (*Matcher, error) {
	pattern := config.Pattern
	literal := ""
	if config.Fixed {
		literal = config.Pattern
		pattern = regexp.QuoteMeta(config.Pattern)
	} else if parsed, err := regexp.Compile(config.Pattern); err == nil {
		literal, _ = parsed.LiteralPrefix()
	}
	if config.IgnoreCase {
		pattern = "(?i:" + pattern + ")"
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}
	return &Matcher{regexp: compiled, literal: literal, word: config.Word, before: config.Before, after: config.After}, nil
}

func (m *Matcher) LiteralPrefix() string { return m.literal }

func (m *Matcher) Scan(filePath string, reader io.Reader, maxMatches int) (FileResult, error) {
	result := FileResult{}
	scanned, err := m.ScanEmitContext(context.Background(), filePath, reader, maxMatches, func(event Event) error {
		result.Events = append(result.Events, event)
		return nil
	})
	result.Matches = scanned.Matches
	result.HitLimit = scanned.HitLimit
	return result, err
}

func (m *Matcher) ScanEmit(filePath string, reader io.Reader, maxMatches int, emit func(Event) error) (FileResult, error) {
	return m.ScanEmitContext(context.Background(), filePath, reader, maxMatches, emit)
}

func (m *Matcher) ScanEmitContext(ctx context.Context, filePath string, reader io.Reader, maxMatches int, emit func(Event) error) (FileResult, error) {
	type bufferedLine struct {
		number int
		text   string
	}

	result := FileResult{}
	input := bufio.NewReaderSize(reader, 64<<10)
	before := make([]bufferedLine, 0, min(m.before, 1024))
	beforeBytes := 0
	lineNumber := 0
	lastEmitted := 0
	afterRemaining := 0

	emitEvent := func(event Event) error {
		if err := emit(event); err != nil {
			return fmt.Errorf("%w for %q: %w", errResultSpool, filePath, err)
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return FileResult{}, err
		}
		lineBytes, readErr := readBoundedLine(input)
		if len(lineBytes) == 0 && readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return result, nil
			}
			return FileResult{}, fmt.Errorf("read %q: %w", filePath, readErr)
		}
		lineNumber++
		line := strings.TrimSuffix(strings.TrimSuffix(string(lineBytes), "\n"), "\r")
		if !utf8.ValidString(line) {
			return FileResult{}, fmt.Errorf("read %q: %w", filePath, errInvalidUTF8)
		}
		if err := ctx.Err(); err != nil {
			return FileResult{}, err
		}
		matches, matchErr := m.find(line)
		if matchErr != nil {
			return FileResult{}, fmt.Errorf("match %q: %w", filePath, matchErr)
		}
		if err := ctx.Err(); err != nil {
			return FileResult{}, err
		}
		if len(matches) > 0 {
			for _, previous := range before {
				if previous.number > lastEmitted {
					if err := emitEvent(Event{Type: "context", Path: filePath, Line: previous.number, Text: previous.text, Context: "before"}); err != nil {
						return FileResult{}, err
					}
					lastEmitted = previous.number
				}
			}
			submatches := make([]Submatch, 0, len(matches))
			for _, match := range matches {
				submatches = append(submatches, Submatch{Start: match[0], End: match[1], Text: line[match[0]:match[1]]})
			}
			if err := emitEvent(Event{
				Type:       "match",
				Path:       filePath,
				Line:       lineNumber,
				Column:     matches[0][0] + 1,
				Text:       line,
				Submatches: submatches,
			}); err != nil {
				return FileResult{}, err
			}
			result.Matches++
			lastEmitted = lineNumber
			afterRemaining = m.after
			if maxMatches > 0 && result.Matches >= maxMatches {
				result.HitLimit = true
				return result, nil
			}
		} else if afterRemaining > 0 {
			if err := emitEvent(Event{Type: "context", Path: filePath, Line: lineNumber, Text: line, Context: "after"}); err != nil {
				return FileResult{}, err
			}
			lastEmitted = lineNumber
			afterRemaining--
		}

		if m.before > 0 {
			before = append(before, bufferedLine{number: lineNumber, text: line})
			beforeBytes += len(line)
			if len(before) > m.before {
				beforeBytes -= len(before[0].text)
				before = before[len(before)-m.before:]
			}
			if beforeBytes > maxBufferedContextBytes {
				return FileResult{}, fmt.Errorf("read %q: %w (%d bytes)", filePath, errContextWindowTooLarge, maxBufferedContextBytes)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if err := ctx.Err(); err != nil {
					return FileResult{}, err
				}
				return result, nil
			}
			return FileResult{}, fmt.Errorf("read %q: %w", filePath, readErr)
		}
	}
}

func readBoundedLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxLineBytes+2 {
			return nil, fmt.Errorf("%w (%d bytes)", errLineTooLong, maxLineBytes)
		}
		line = append(line, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		contentLength := len(line)
		if contentLength > 0 && line[contentLength-1] == '\n' {
			contentLength--
		}
		if contentLength > 0 && line[contentLength-1] == '\r' {
			contentLength--
		}
		if contentLength > maxLineBytes {
			return nil, fmt.Errorf("%w (%d bytes)", errLineTooLong, maxLineBytes)
		}
		return line, err
	}
}

func (m *Matcher) find(line string) ([][]int, error) {
	matches := m.regexp.FindAllStringIndex(line, maxSubmatchesPerLine+1)
	if len(matches) > maxSubmatchesPerLine {
		return nil, fmt.Errorf("%w (%d)", errSubmatchLimit, maxSubmatchesPerLine)
	}
	if !m.word || len(matches) == 0 {
		return matches, nil
	}
	filtered := matches[:0]
	for _, match := range matches {
		if match[0] > 0 {
			previous, _ := utf8.DecodeLastRuneInString(line[:match[0]])
			if isWordRune(previous) {
				continue
			}
		}
		if match[1] < len(line) {
			next, _ := utf8.DecodeRuneInString(line[match[1]:])
			if isWordRune(next) {
				continue
			}
		}
		filtered = append(filtered, match)
	}
	return filtered, nil
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r)
}
