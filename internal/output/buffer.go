package output

import (
	"bufio"
	"io"
	"sync"
	"time"
)

const outputFlushInterval = 100 * time.Millisecond

type bufferedWriter struct {
	mu     sync.Mutex
	writer *bufio.Writer
	timer  *time.Timer
	closed bool
}

func newBufferedWriter(out io.Writer) *bufferedWriter {
	return &bufferedWriter{writer: bufio.NewWriterSize(out, 64<<10)}
}

func (w *bufferedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := w.writer.Write(data)
	if err == nil && w.writer.Buffered() > 0 && w.timer == nil {
		w.timer = time.AfterFunc(outputFlushInterval, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if !w.closed {
				// bufio.Writer retains flush errors for the next Write, Flush or Close.
				_ = w.writer.Flush()
			}
			w.timer = nil
		})
	}
	return n, err
}

func (w *bufferedWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Flush()
}

func (w *bufferedWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.timer != nil {
		w.timer.Stop()
	}
	return w.writer.Flush()
}
