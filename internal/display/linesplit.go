package display

import "sync"

// LineWriter is an io.Writer that splits incoming bytes into logical lines on
// either '\r' or '\n' and invokes emit for each completed line (the terminator
// is stripped). rsync's --info=progress2 rewrites its progress line using
// carriage returns rather than newlines, so splitting on '\r' as well as '\n'
// lets us capture each progress update as a job's current "last line".
type LineWriter struct {
	mu   sync.Mutex
	buf  []byte
	emit func(string)
}

// NewLineWriter returns a LineWriter that calls emit once per completed line.
func NewLineWriter(emit func(string)) *LineWriter {
	return &LineWriter{emit: emit}
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\r' || b == '\n' {
			w.flushLocked()
			continue
		}
		w.buf = append(w.buf, b)
	}
	return len(p), nil
}

// Flush emits any buffered bytes as a final line. Call it once the producing
// process has exited and no further writes can occur.
func (w *LineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
}

func (w *LineWriter) flushLocked() {
	if len(w.buf) == 0 {
		return
	}
	w.emit(string(w.buf))
	w.buf = w.buf[:0]
}
