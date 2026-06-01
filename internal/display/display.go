// Package display renders a live, one-line-per-running-job view of parallel
// rsync transfers while streaming the full output of every job to a log file.
// When stdout is not a terminal it degrades to periodic plain-text status
// lines so logs and pipelines stay readable.
package display

import (
	"fmt"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

const (
	ttyTick    = 100 * time.Millisecond
	nonTTYTick = 2 * time.Second
)

type slot struct {
	label  string
	last   string
	active bool
}

// Display owns all writes to stdout for the duration of a run. A single
// renderer goroutine redraws the live region on a ticker, so concurrent jobs
// never interleave their output on the terminal.
type Display struct {
	out     *os.File
	isTTY   bool
	logFile *os.File

	mu         sync.Mutex
	slots      []slot
	printed    []string // last line printed per slot (non-TTY de-duplication)
	pending    []string // permanent lines waiting to scroll above the live region
	dirty      bool
	linesDrawn int

	logMu sync.Mutex

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// New creates a Display with numSlots live lines (one per worker) and opens
// logPath for the combined, per-line log of every job.
func New(numSlots int, logPath string) (*Display, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	d := &Display{
		out:     os.Stdout,
		isTTY:   term.IsTerminal(int(os.Stdout.Fd())),
		logFile: f,
		slots:   make([]slot, numSlots),
		printed: make([]string, numSlots),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if d.isTTY {
		fmt.Fprint(d.out, "\033[?25l") // hide cursor for the duration of the run
	}
	go d.loop()
	return d, nil
}

// StartSlot binds a worker's slot to a job label and clears its line.
func (d *Display) StartSlot(i int, label string) {
	d.mu.Lock()
	d.slots[i] = slot{label: label, active: true}
	d.printed[i] = ""
	d.dirty = true
	d.mu.Unlock()
}

// UpdateSlot sets the most recent output line for a running slot.
func (d *Display) UpdateSlot(i int, line string) {
	d.mu.Lock()
	if d.slots[i].active {
		d.slots[i].last = line
		d.dirty = true
	}
	d.mu.Unlock()
}

// FinishSlot marks a slot idle so it drops out of the live region.
func (d *Display) FinishSlot(i int) {
	d.mu.Lock()
	d.slots[i].active = false
	d.slots[i].last = ""
	d.dirty = true
	d.mu.Unlock()
}

// Permanent enqueues a line to be printed above the live region (e.g. a job's
// final ✓/✗ status). It scrolls up and stays on screen.
func (d *Display) Permanent(line string) {
	d.mu.Lock()
	d.pending = append(d.pending, line)
	d.dirty = true
	d.mu.Unlock()
}

// Log appends one prefixed line to the combined log file.
func (d *Display) Log(label, line string) {
	d.logMu.Lock()
	fmt.Fprintf(d.logFile, "[%s] %s\n", label, line)
	d.logMu.Unlock()
}

// Close stops the renderer, restores the cursor, and closes the log file. It is
// safe to call more than once.
func (d *Display) Close() {
	d.closeOnce.Do(func() {
		close(d.stop)
		<-d.done
		if d.isTTY {
			fmt.Fprint(d.out, "\033[?25h") // restore cursor
		}
		d.logFile.Close()
	})
}

func (d *Display) loop() {
	defer close(d.done)
	interval := ttyTick
	if !d.isTTY {
		interval = nonTTYTick
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			d.render(true)
			return
		case <-t.C:
			d.render(false)
		}
	}
}

func (d *Display) render(final bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.dirty && len(d.pending) == 0 && !final {
		return
	}
	pending := d.pending
	d.pending = nil

	if d.isTTY {
		// Move to the top of the previous live region and clear downward.
		if d.linesDrawn > 0 {
			fmt.Fprintf(d.out, "\033[%dA", d.linesDrawn)
		}
		fmt.Fprint(d.out, "\r\033[0J")

		for _, line := range pending {
			fmt.Fprintln(d.out, line)
		}

		w := d.width()
		drawn := 0
		for i := range d.slots {
			if !d.slots[i].active {
				continue
			}
			fmt.Fprintln(d.out, truncate(formatSlot(&d.slots[i]), w))
			drawn++
		}
		d.linesDrawn = drawn
		d.dirty = false
		return
	}

	// Non-TTY: print permanent lines, then any slot whose line changed.
	for _, line := range pending {
		fmt.Fprintln(d.out, line)
	}
	for i := range d.slots {
		s := &d.slots[i]
		if !s.active || s.last == "" || s.last == d.printed[i] {
			continue
		}
		d.printed[i] = s.last
		fmt.Fprintln(d.out, formatSlot(s))
	}
	d.dirty = false
}

func (d *Display) width() int {
	w, _, err := term.GetSize(int(d.out.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

func formatSlot(s *slot) string {
	if s.last == "" {
		return "[" + s.label + "] …"
	}
	return "[" + s.label + "] " + s.last
}

// truncate shortens s to at most w display runes, adding an ellipsis when cut.
func truncate(s string, w int) string {
	if w <= 0 || utf8.RuneCountInString(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	cut := w - 1
	runes := 0
	for i := range s {
		if runes == cut {
			return s[:i] + "…"
		}
		runes++
	}
	return s
}
