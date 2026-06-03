package migrate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/dansimau/macmigrate/internal/display"
)

func TestClassify(t *testing.T) {
	if s, c := classify(nil); s != StatusOK || c != 0 {
		t.Errorf("classify(nil) = (%v, %d), want (OK, 0)", s, c)
	}
	cases := map[int]Status{23: StatusPartial, 24: StatusPartial, 2: StatusFailed}
	for code, want := range cases {
		// Pass the code as $1 rather than interpolating into the script.
		err := exec.Command("/bin/sh", "-c", `exit "$1"`, "sh", strconv.Itoa(code)).Run()
		if s, c := classify(err); s != want || c != code {
			t.Errorf("classify(exit %d) = (%v, %d), want (%v, %d)", code, s, c, want, code)
		}
	}
}

// TestRunCopiesLocally exercises the full worker-pool + xexec + LineWriter +
// display pipeline against the real rsync binary, using rsync's local mode
// (no "host:" prefix) so it needs no second machine or ssh.
func TestRunCopiesLocally(t *testing.T) {
	if _, err := os.Stat("/opt/homebrew/bin/rsync"); err != nil {
		if _, err := os.Stat("/usr/bin/rsync"); err != nil {
			t.Skip("rsync not found")
		}
	}

	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	disp, err := display.New(2, filepath.Join(t.TempDir(), "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	jobs := []Job{{Label: "t", Srcs: []string{src + "/"}, Dst: dst + "/"}}
	results := Run(context.Background(), jobs, 2, disp, "rsync", SSH{}, "", false)
	disp.Close()

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Status != StatusOK || results[0].Err != nil {
		t.Fatalf("job not OK: status=%v err=%v (stderr: %v)", results[0].Status, results[0].Err, results[0].Stderr)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("copy failed: content=%q err=%v", got, err)
	}
}

// TestRunPartialExit23 verifies the runOne→classify path: a job whose rsync
// exits 23 (the macOS-protected-directory case) is reported as a partial, not a
// failure, and its stderr is still captured.
func TestRunPartialExit23(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fakersync")
	script := "#!/bin/sh\n" +
		"echo 'rsync: [sender] opendir failed: Operation not permitted (1)' 1>&2\n" +
		"exit 23\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	disp, err := display.New(1, filepath.Join(dir, "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	jobs := []Job{{Label: "p", Srcs: []string{"/src/"}, Dst: "/dst/"}}
	results := Run(context.Background(), jobs, 1, disp, fake, SSH{}, "", false)
	disp.Close()

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Status != StatusPartial || results[0].Code != 23 {
		t.Fatalf("status=%v code=%d, want Partial/23", results[0].Status, results[0].Code)
	}
	if len(results[0].Stderr) == 0 {
		t.Error("expected captured stderr lines for the partial job")
	}
}
