package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/dansimau/macmigrate/internal/migrate"
)

// reportCode runs report and extracts the exit code, treating a nil error as 0.
func reportCode(results []migrate.Result, interrupted bool) int {
	return report(results, "log", time.Second, interrupted)
}

func TestReportExitCodes(t *testing.T) {
	ok := migrate.Result{Status: migrate.StatusOK}
	partial := migrate.Result{Status: migrate.StatusPartial, Code: 23, Job: migrate.Job{Label: "Library/Mail"}}
	failed := migrate.Result{Status: migrate.StatusFailed, Code: 2, Job: migrate.Job{Label: "Documents"}}

	// Partial transfers exit non-zero so a scripted run notices them.
	if code := reportCode([]migrate.Result{ok, partial}, false); code != 1 {
		t.Errorf("ok+partial exit = %d, want 1", code)
	}
	// No partials, no failures → success.
	if code := reportCode([]migrate.Result{ok}, false); code != 0 {
		t.Errorf("all ok exit = %d, want 0", code)
	}
	// Hard failures exit 1.
	if code := reportCode([]migrate.Result{ok, failed}, false); code != 1 {
		t.Errorf("with failure exit = %d, want 1", code)
	}
	// Interruption wins.
	if code := reportCode([]migrate.Result{ok, partial}, true); code != 130 {
		t.Errorf("interrupted exit = %d, want 130", code)
	}
}

func TestRsyncVersionParse(t *testing.T) {
	const sample = "rsync  version 3.4.3  protocol version 32"
	m := rsyncVersionRE.FindStringSubmatch(sample)
	if m == nil {
		t.Fatal("regex did not match")
	}
	if atoi(m[1]) != 3 || atoi(m[2]) != 4 {
		t.Fatalf("parsed %s.%s, want 3.4", m[1], m[2])
	}
}

// TestCheckRsync runs against the real rsync in PATH; it should be >= 3.1.
func TestCheckRsync(t *testing.T) {
	if err := checkRsync("rsync"); err != nil {
		t.Fatalf("checkRsync(rsync): %v", err)
	}
}

// TestFailExitCode verifies fail() and sudoRequiredError() carry the expected
// fatal exit code through the exitErr type.
func TestFailExitCode(t *testing.T) {
	var ee *exitErr
	if err := fail("boom"); !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("fail() = %v (code %d), want exitErr code 2", err, ee.code)
	}
	if err := sudoRequiredError("h"); !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("sudoRequiredError() code = %d, want 2", ee.code)
	}
}
