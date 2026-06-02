// Command macmigrate copies a user's home directory and third-party
// applications from one Mac to another over ssh, running many rsync transfers
// in parallel to saturate a fast direct link.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/dansimau/macmigrate/internal/display"
	"github.com/dansimau/macmigrate/internal/migrate"
	"github.com/dansimau/macmigrate/internal/xexec"
)

// stringSlice is a repeatable string flag.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	os.Exit(run())
}

func run() int {
	const rsyncBin = "rsync"
	username := flag.String("user", "", "username whose home (/Users/<user>) to migrate; set automatically when re-running under sudo")
	dest := flag.String("dest", "", "destination [user@]host (required), e.g. 169.254.190.76")
	parallel := flag.Int("j", 4, "maximum number of parallel rsync jobs")
	dryRun := flag.Bool("n", false, "dry run: pass --dry-run to rsync (no files written)")
	debug := flag.Bool("debug", false, "print diagnostics (app selection, full rsync commands) to stderr")

	var excludes, includes stringSlice
	flag.Var(&excludes, "exclude", "home/Library entry to skip, e.g. 'Library/Containers' (repeatable; adds to defaults)")
	flag.Var(&includes, "include", "additional absolute directory to migrate to the same path, as root (repeatable; adds to defaults)")
	flag.Parse()

	if *dest == "" {
		flag.Usage()
		return fail("-dest is required")
	}
	if *parallel < 1 {
		return fail("-j must be at least 1")
	}

	// macmigrate copies files as root locally so rsync can read every file
	// regardless of owner or mode. Started without sudo, it re-runs itself under
	// sudo (which prompts for a password), passing --user so the root instance
	// knows whose home to migrate.
	if os.Geteuid() != 0 {
		return reexecUnderSudo(*username)
	}

	if *username == "" {
		return fail("-user is required (set automatically when re-running under sudo)")
	}
	home := filepath.Join("/Users", *username)
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		return fail("home directory %s does not exist", home)
	}

	dirs, err := resolveDirs(migrate.DefaultDirs, includes)
	if err != nil {
		return fail("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := checkRsync(rsyncBin); err != nil {
		return fail("%v", err)
	}

	// Full Disk Access is a property of the responsible terminal and is
	// inherited across the sudo re-exec; without it, TCC-protected data is
	// silently skipped. Warn, but let the run proceed — the rest still copies.
	switch granted, ok := migrate.FullDiskAccess(home); {
	case ok && granted:
		fmt.Println("✓ Full Disk Access granted")
	case ok && !granted:
		fmt.Println("⚠ Full Disk Access NOT granted to this terminal — TCC-protected data")
		fmt.Println("  (Mail, Messages, Safari, Photos) will be skipped. To include it, grant")
		fmt.Println("  System Settings ▸ Privacy & Security ▸ Full Disk Access to your terminal,")
		fmt.Println("  then re-run. You can proceed without it.")
	}

	// We're root after the sudo re-exec, but ssh must run as the invoking user
	// so it uses that user's agent/keys rather than root's (which has none).
	fmt.Printf("Connecting to %s …\n", *dest)
	rhome, err := migrate.RemoteHome(ctx, *username, *dest)
	if err != nil {
		return fail("%v\n\nEnable Remote Login on the destination (System Settings ▸ General ▸ Sharing)\nand set up key-based ssh so parallel jobs don't hit password prompts.", err)
	}
	migrate.Debugf(*debug, "remote home on %s = %s", *dest, rhome)

	// Everything is copied with `sudo rsync` so file ownership is preserved,
	// which can't prompt for a password across parallel ssh connections. Require
	// passwordless sudo up front and fail loudly if it's missing.
	if !*dryRun && !migrate.CanSudo(ctx, *username, *dest) {
		return sudoRequiredError(*dest)
	}

	// Create each additional directory (and any missing parents) on the
	// destination before its contents are copied; rsync sets the ownership of
	// the entries inside. A prep failure is fatal — we don't quietly skip work.
	for _, dir := range dirs {
		if !*dryRun {
			fmt.Printf("Preparing %s on %s …\n", dir, *dest)
		}
		if err := migrate.PrepareRemoteDir(ctx, *username, *dest, dir, *dryRun); err != nil {
			return fail("preparing %s on destination: %v", dir, err)
		}
	}

	opt := migrate.Options{
		Dest:         *dest,
		SSHUser:      *username,
		Home:         home,
		RemoteHome:   rhome,
		Dirs:         dirs,
		SkipNames:    concat(migrate.DefaultSkip, excludes),
		RsyncExclude: migrate.DefaultRsyncExclude,
		DoHome:       true,
		DoApps:       true,
		DoDirs:       len(dirs) > 0,
		Debug:        *debug,
	}
	jobs, notes, err := migrate.BuildJobs(ctx, opt)
	if err != nil {
		return fail("%v", err)
	}
	for _, j := range jobs {
		migrate.Debugf(*debug, "job %s: rsync %s", j.Label, strings.Join(j.Args(*dryRun), " "))
	}
	for _, n := range notes {
		fmt.Println(n)
	}
	if len(jobs) == 0 {
		fmt.Println("Nothing to copy.")
		return 0
	}

	lp := fmt.Sprintf("macmigrate-%s.log", time.Now().Format("20060102-150405"))

	fmt.Printf("macmigrate → %s  (%s)\n", *dest, rhome)
	fmt.Printf("  %d jobs · up to %d parallel · log: %s\n", len(jobs), *parallel, lp)
	if *dryRun {
		fmt.Println("  DRY RUN — no files will be written")
	}

	disp, err := display.New(*parallel, lp)
	if err != nil {
		return fail("opening log file: %v", err)
	}
	defer disp.Close() // safety net; Close is idempotent

	// Keep both Macs awake for the whole transfer; stop() lets them sleep again.
	stopCaffeinate := migrate.Caffeinate(*username, *dest)
	defer stopCaffeinate()

	start := time.Now()
	results := migrate.Run(ctx, jobs, *parallel, disp, rsyncBin, *dryRun)
	disp.Close() // tear down the live region before printing the report

	return report(results, lp, time.Since(start), ctx.Err() != nil)
}

// report prints the end-of-run summary and returns the process exit code.
// Partial transfers (rsync exit 23/24 — typically macOS privacy-protected
// directories) exit non-zero so a scripted run notices them, with their error
// lines surfaced in the summary.
func report(results []migrate.Result, logPath string, elapsed time.Duration, interrupted bool) int {
	var partial, failed []migrate.Result
	ok := 0
	for _, r := range results {
		switch r.Status {
		case migrate.StatusPartial:
			partial = append(partial, r)
		case migrate.StatusFailed:
			failed = append(failed, r)
		default:
			ok++
		}
	}

	fmt.Printf("\n%d done · %d partial · %d failed · %s\n", ok, len(partial), len(failed), elapsed.Round(time.Second))
	fmt.Printf("Full log: %s\n", logPath)

	// Hard failures get full detail.
	for _, r := range failed {
		fmt.Printf("\n✗ %s: %v\n", r.Job.Label, r.Err)
		for _, line := range r.Stderr {
			fmt.Printf("    %s\n", line)
		}
		fmt.Printf("    full output: grep -F '[%s] ' %s\n", r.Job.Label, logPath)
	}

	// Partials get a per-directory list with their error lines and a single
	// Full Disk Access nudge.
	if len(partial) > 0 {
		fmt.Printf("\n⚠ %d director%s had unreadable items (everything else copied):\n",
			len(partial), plural(len(partial), "y", "ies"))
		for _, r := range partial {
			fmt.Printf("    %s\n", r.Job.Label)
			for _, line := range r.Stderr {
				fmt.Printf("        %s\n", line)
			}
		}
		fmt.Println("\n  These are macOS privacy-protected (TCC). To include data like Mail,")
		fmt.Println("  Messages, Safari and Photos, grant Full Disk Access to your terminal:")
		fmt.Println("    System Settings ▸ Privacy & Security ▸ Full Disk Access")
		fmt.Println("  then re-run. A few system stores (e.g. com.apple.TCC) can never be copied.")
		fmt.Printf("  Per-directory errors are in the log: grep -F '[<label>] ' %s\n", logPath)
	}

	switch {
	case interrupted:
		fmt.Println("\nInterrupted before completion.")
		return 130
	case len(failed) > 0, len(partial) > 0:
		return 1
	default:
		return 0
	}
}

// reexecUnderSudo re-runs this binary under `sudo`, recording the invoking user
// via --user so the root instance knows whose home directory to migrate. sudo
// inherits our stdio, so its password prompt reaches the user's terminal.
func reexecUnderSudo(userFlag string) int {
	self, err := os.Executable()
	if err != nil {
		return fail("locating own binary: %v", err)
	}
	username := userFlag
	if username == "" {
		username = currentUsername()
	}
	if username == "" {
		return fail("cannot determine the invoking username; pass --user")
	}

	// -E preserves the environment (notably SSH_AUTH_SOCK / SSH_AGENT_PID) so
	// the root instance's ssh can talk to the user's ssh-agent. Inject --user
	// only if the user didn't already pass one (otherwise it's already in args).
	args := []string{"-E", self}
	if userFlag == "" {
		args = append(args, "--user", username)
	}
	args = append(args, os.Args[1:]...)

	fmt.Printf("Re-running under sudo as root (migrating /Users/%s) …\n", username)
	cmd := xexec.Command("sudo", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return fail("running under sudo: %v", err)
	}
	return 0
}

// currentUsername returns the name of the user who invoked macmigrate.
func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// resolveDirs builds the additional-directory list: defaults that exist locally,
// plus every -include value (which must exist), de-duplicated and cleaned.
func resolveDirs(defaults, extra []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		d = filepath.Clean(d)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, d := range defaults {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			add(d)
		}
	}
	for _, d := range extra {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("-include %q: not an existing directory", d)
		}
		add(d)
	}
	return out, nil
}

var rsyncVersionRE = regexp.MustCompile(`version (\d+)\.(\d+)`)

// checkRsync verifies the rsync binary exists and is new enough for
// --info=progress2 (rsync >= 3.1; stock /usr/bin/rsync may be too old).
func checkRsync(bin string) error {
	cmd := xexec.Command(bin, "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %q --version: %w", bin, err)
	}
	m := rsyncVersionRE.FindStringSubmatch(out.String())
	if m == nil {
		return fmt.Errorf("could not parse %q version output", bin)
	}
	major, minor := atoi(m[1]), atoi(m[2])
	if major < 3 || (major == 3 && minor < 1) {
		return fmt.Errorf("%s is rsync %d.%d; --info=progress2 needs >= 3.1 (try Homebrew rsync, or pass -rsync)", bin, major, minor)
	}
	return nil
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func concat(a []string, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

// sudoRequiredError prints the passwordless-sudo remediation and returns a
// fatal exit code. Every transfer runs as root to preserve ownership, so the
// migration can't proceed without it.
func sudoRequiredError(dest string) int {
	w := os.Stderr
	fmt.Fprintf(w, "macmigrate: passwordless sudo is not available on %s.\n", dest)
	fmt.Fprintln(w, "  Files are copied with `sudo rsync` so ownership is preserved, which can't")
	fmt.Fprintln(w, "  prompt for a password over parallel ssh connections.")
	fmt.Fprintln(w, "  Enable passwordless sudo for your user ON THE DESTINATION, e.g.:")
	fmt.Fprintln(w, `    echo "$(id -un) ALL=(ALL) NOPASSWD: ALL" | sudo tee /etc/sudoers.d/macmigrate >/dev/null`)
	fmt.Fprintln(w, "    sudo chmod 440 /etc/sudoers.d/macmigrate")
	fmt.Fprintln(w, "  (Remove that file once the migration is done.)")
	return 2
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "macmigrate: "+format+"\n", args...)
	return 2
}
