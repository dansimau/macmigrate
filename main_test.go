package main

import (
	"testing"
	"time"

	"macmigrate/internal/migrate"
)

func TestReportExitCodes(t *testing.T) {
	ok := migrate.Result{Status: migrate.StatusOK}
	partial := migrate.Result{Status: migrate.StatusPartial, Code: 23, Job: migrate.Job{Label: "Library/Mail"}}
	failed := migrate.Result{Status: migrate.StatusFailed, Code: 2, Job: migrate.Job{Label: "Documents"}}

	// Partial transfers exit non-zero so a scripted run notices them.
	if code := report([]migrate.Result{ok, partial}, "log", time.Second, false); code != 1 {
		t.Errorf("ok+partial exit = %d, want 1", code)
	}
	// No partials, no failures → success.
	if code := report([]migrate.Result{ok}, "log", time.Second, false); code != 0 {
		t.Errorf("all ok exit = %d, want 0", code)
	}
	// Hard failures exit 1.
	if code := report([]migrate.Result{ok, failed}, "log", time.Second, false); code != 1 {
		t.Errorf("with failure exit = %d, want 1", code)
	}
	// Interruption wins.
	if code := report([]migrate.Result{ok, partial}, "log", time.Second, true); code != 130 {
		t.Errorf("interrupted exit = %d, want 130", code)
	}
}

func TestScope(t *testing.T) {
	cases := []struct {
		in               []string
		home, apps, dirs bool
	}{
		{nil, true, true, true},
		{[]string{"home"}, true, false, false},
		{[]string{"apps"}, false, true, false},
		{[]string{"applications"}, false, true, false},
		{[]string{"dirs"}, false, false, true},
		{[]string{"home", "apps", "dirs"}, true, true, true},
		{[]string{"bogus"}, false, false, false},
	}
	for _, c := range cases {
		h, a, d := scope(c.in)
		if h != c.home || a != c.apps || d != c.dirs {
			t.Errorf("scope(%v) = (%v, %v, %v), want (%v, %v, %v)", c.in, h, a, d, c.home, c.apps, c.dirs)
		}
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
