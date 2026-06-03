package migrate

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"

	"github.com/dansimau/macmigrate/internal/display"
	"github.com/dansimau/macmigrate/internal/xexec"
)

// maxErrLines caps how much of a failed job's stderr is retained for the
// end-of-run report. The full output is always in the log file.
const maxErrLines = 40

// Status is the outcome category of a job.
type Status int

const (
	StatusOK      Status = iota // rsync exited 0
	StatusPartial               // rsync exited 23/24: some files skipped, the rest copied
	StatusFailed                // rsync exited with any other non-zero code, or never ran
)

// Result is the outcome of one job.
type Result struct {
	Job    Job
	Status Status
	Code   int      // rsync exit code (0 on success, -1 if it never ran)
	Err    error    // non-nil for partial and failed jobs
	Stderr []string // captured stderr lines, for the report
}

// classify maps a cmd.Run error to a status and rsync exit code. Exit 23
// ("partial transfer due to error") and 24 ("some source files vanished") mean
// rsync copied everything it could read and skipped the rest — a warning, not a
// failed run. macOS TCC-protected directories surface as exit 23.
func classify(err error) (Status, int) {
	if err == nil {
		return StatusOK, 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if code == 23 || code == 24 {
			return StatusPartial, code
		}
		return StatusFailed, code
	}
	return StatusFailed, -1
}

// Run executes jobs using up to numWorkers concurrent rsync processes,
// rendering progress through disp. Each worker owns one stable display slot, so
// the live region shows one line per worker. It returns one Result per job that
// ran (fewer than len(jobs) if ctx is cancelled mid-run).
func Run(ctx context.Context, jobs []Job, numWorkers int, disp *display.Display, rsyncBin string, ssh SSH, dest string, dryRun bool) []Result {
	if numWorkers < 1 {
		numWorkers = 1
	}
	if len(jobs) < numWorkers {
		numWorkers = len(jobs)
	}

	jobCh := make(chan Job)
	results := make([]Result, 0, len(jobs))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for job := range jobCh {
				res := runOne(ctx, slot, job, disp, rsyncBin, ssh, dest, dryRun)
				mu.Lock()
				results = append(results, res)
				mu.Unlock()
			}
		}(w)
	}

dispatch:
	for _, job := range jobs {
		select {
		case <-ctx.Done():
			break dispatch
		case jobCh <- job:
		}
	}
	close(jobCh)
	wg.Wait()
	return results
}

func runOne(ctx context.Context, slot int, job Job, disp *display.Display, rsyncBin string, ssh SSH, dest string, dryRun bool) Result {
	disp.StartSlot(slot, job.Label)

	stdout := display.NewLineWriter(func(line string) {
		disp.UpdateSlot(slot, line)
		disp.Log(job.Label, line)
	})
	var stderrLines []string
	stderr := display.NewLineWriter(func(line string) {
		disp.UpdateSlot(slot, line)
		disp.Log(job.Label, line)
		if len(stderrLines) < maxErrLines {
			stderrLines = append(stderrLines, line)
		}
	})

	cmd := xexec.CommandContext(ctx, rsyncBin, job.Args(dryRun)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	// cmd.Run has waited for the output copiers, so flushing trailing partial
	// lines here cannot race with them.
	stdout.Flush()
	stderr.Flush()

	disp.FinishSlot(slot)
	status, code := classify(err)
	// Second execution: fix ownership of what rsync just wrote (see Chown).
	// Partial jobs copied most of their files, so they get fixed too. A chown
	// failure is a failed job — silently wrong ownership is what this prevents.
	if status != StatusFailed && !dryRun && job.Chown != nil {
		if cerr := ssh.RunChown(ctx, dest, job.Chown); cerr != nil {
			status = StatusFailed
			err = fmt.Errorf("fixing ownership: %w", cerr)
		}
	}
	switch status {
	case StatusPartial:
		disp.Permanent(fmt.Sprintf("⚠ [%s] partial — some items unreadable (exit %d)", job.Label, code))
	case StatusFailed:
		disp.Permanent(fmt.Sprintf("✗ [%s] FAILED: %v", job.Label, err))
	default: // StatusOK
		if dryRun {
			disp.Permanent(fmt.Sprintf("✓ [%s] done (dry-run)", job.Label))
		} else {
			disp.Permanent(fmt.Sprintf("✓ [%s] done", job.Label))
		}
	}
	return Result{Job: job, Status: status, Code: code, Err: err, Stderr: stderrLines}
}
