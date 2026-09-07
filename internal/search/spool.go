package search

import (
	"bufio"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
)

const maxResultSpoolBytes int64 = 32 << 20
const maxMemorySpoolBytes int64 = 256 << 10

var errResultSpool = errors.New("buffer search result")
var errResultSpoolLimit = errors.New("file results exceed spool byte limit")

type eventSpool struct {
	file        *os.File
	encoder     *gob.Encoder
	buffer      *bufio.Writer
	events      []Event
	memoryBytes int64
}

func (s *eventSpool) Emit(event Event) error {
	if s.file == nil {
		// Charge struct/slice overhead and retained strings, not just matching text.
		size := int64(512 + len(event.Path) + len(event.Text) + len(event.Context))
		for _, match := range event.Submatches {
			size += 64 + int64(len(match.Text))
		}
		if s.memoryBytes+size <= maxMemorySpoolBytes {
			s.events = append(s.events, event)
			s.memoryBytes += size
			return nil
		}
		file, err := os.CreateTemp("", "git-rg-events-*")
		if err != nil {
			return err
		}
		if err := file.Chmod(0o600); err != nil {
			file.Close()
			os.Remove(file.Name())
			return err
		}
		s.file = file
		s.buffer = bufio.NewWriterSize(file, 64<<10)
		s.encoder = gob.NewEncoder(&limitedSpoolWriter{writer: s.buffer})
		for _, buffered := range s.events {
			if err := s.encoder.Encode(buffered); err != nil {
				return err
			}
		}
		s.events = nil
		s.memoryBytes = 0
	}
	return s.encoder.Encode(event)
}

type limitedSpoolWriter struct {
	writer  io.Writer
	written int64
}

func (w *limitedSpoolWriter) Write(data []byte) (int, error) {
	if w.written+int64(len(data)) > maxResultSpoolBytes {
		return 0, fmt.Errorf("%w (%d bytes)", errResultSpoolLimit, maxResultSpoolBytes)
	}
	written, err := w.writer.Write(data)
	w.written += int64(written)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	return written, err
}

func (s *eventSpool) Finish() (string, error) {
	if s.file == nil {
		return "", nil
	}
	name := s.file.Name()
	flushErr := s.buffer.Flush()
	closeErr := s.file.Close()
	if err := errors.Join(flushErr, closeErr); err != nil {
		os.Remove(name)
		s.file = nil
		return "", err
	}
	s.file = nil
	return name, nil
}

func (s *eventSpool) Abort() {
	s.events = nil
	s.memoryBytes = 0
	if s.file == nil {
		return
	}
	name := s.file.Name()
	s.file.Close()
	os.Remove(name)
	s.file = nil
}
