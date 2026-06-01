// Command macmigrate copies a user's home directory and third-party
// applications from one Mac to another over ssh, running many rsync transfers
// in parallel to saturate a fast direct link.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"macmigrate/internal/display"
	"macmigrate/internal/migrate"
	"macmigrate/internal/xexec"
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
	dest := flag.String("dest", "", "destination [user@]host (required), e.g. 169.254.190.76")
	parallel := flag.Int("j", 4, "maximum number of parallel rsync jobs")
	logPath := flag.String("log", "", "combined log file (default ./macmigrate-<timestamp>.log)")
	rsyncBin := flag.String("rsync", "rsync", "rsync binary to use")
	remoteHome := flag.String("remote-home", "", "destination $HOME (default: resolved over ssh)")
	dryRun := flag.Bool("n", false, "dry run: pass --dry-run to rsync (no files written)")

	var only, excludes, rsyncExcludes, extraDirs stringSlice
	flag.Var(&only, "only", "limit scope to 'home', 'apps', and/or 'dirs' (repeatable; default all)")
	flag.Var(&excludes, "exclude", "home/Library entry to skip, e.g. 'Library/Containers' (repeatable; adds to defaults)")
	flag.Var(&rsyncExcludes, "rsync-exclude", "rsync --exclude pattern (repeatable; adds to defaults)")
	flag.Var(&extraDirs, "dir", "additional absolute directory to migrate to the same path, as root (repeatable; adds to defaults)")
	flag.Parse()

	if *dest == "" {
		flag.Usage()
		return fail("-dest is required")
	}
	if *parallel < 1 {
		return fail("-j must be at least 1")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail("cannot determine home directory: %v", err)
	}
	doHome, doApps, doDirs := scope(only)
	if !doHome && !doApps && !doDirs {
		return fail("-only must be 'home', 'apps', and/or 'dirs'")
	}

	var dirs []string
	if doDirs {
		dirs, err = resolveDirs(migrate.DefaultDirs, extraDirs)
		if err != nil {
			return fail("%v", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := checkRsync(*rsyncBin); err != nil {
		return fail("%v", err)
	}

	rhome := *remoteHome
	if rhome == "" {
		fmt.Printf("Connecting to %s …\n", *dest)
		rhome, err = migrate.RemoteHome(ctx, *dest)
		if err != nil {
			return fail("%v\n\nEnable Remote Login on the destination (System Settings ▸ General ▸ Sharing)\nand set up key-based ssh so parallel jobs don't hit password prompts.", err)
		}
	}

	// Everything is copied with `sudo rsync` so file ownership is preserved,
	// which can't prompt for a password across parallel ssh connections. Require
	// passwordless sudo up front and fail loudly if it's missing.
	if !*dryRun && !migrate.CanSudo(ctx, *dest) {
		return sudoRequiredError(*dest)
	}

	// Create each additional directory (and any missing parents) on the
	// destination before its contents are copied; rsync sets the ownership of
	// the entries inside. A prep failure is fatal — we don't quietly skip work.
	if doDirs {
		for _, dir := range dirs {
			if !*dryRun {
				fmt.Printf("Preparing %s on %s …\n", dir, *dest)
			}
			if err := migrate.PrepareRemoteDir(ctx, *dest, dir, *dryRun); err != nil {
				return fail("preparing %s on destination: %v", dir, err)
			}
		}
	}

	opt := migrate.Options{
		Dest:         *dest,
		Home:         home,
		RemoteHome:   rhome,
		Dirs:         dirs,
		SkipNames:    concat(migrate.DefaultSkip, excludes),
		RsyncExclude: concat(migrate.DefaultRsyncExclude, rsyncExcludes),
		DoHome:       doHome,
		DoApps:       doApps,
		DoDirs:       doDirs && len(dirs) > 0,
	}
	jobs, notes, err := migrate.BuildJobs(ctx, opt)
	if err != nil {
		return fail("%v", err)
	}
	for _, n := range notes {
		fmt.Println(n)
	}
	if len(jobs) == 0 {
		fmt.Println("Nothing to copy.")
		return 0
	}

	lp := *logPath
	if lp == "" {
		lp = fmt.Sprintf("macmigrate-%s.log", time.Now().Format("20060102-150405"))
	}

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

	start := time.Now()
	results := migrate.Run(ctx, jobs, *parallel, disp, *rsyncBin, *dryRun)
	disp.Close() // tear down the live region before printing the report

	return report(results, lp, time.Since(start), ctx.Err() != nil)
}

// report prints the end-of-run summary and returns the process exit code.
func report(results []migrate.Result, logPath string, elapsed time.Duration, interrupted bool) int {
	var failed []migrate.Result
	ok := 0
	for _, r := range results {
		if r.Status == migrate.StatusFailed {
			failed = append(failed, r)
		} else {
			ok++
		}
	}

	fmt.Printf("\n%d done · %d failed · %s\n", ok, len(failed), elapsed.Round(time.Second))
	fmt.Printf("Full log: %s\n", logPath)

	for _, r := range failed {
		fmt.Printf("\n✗ %s: %v\n", r.Job.Label, r.Err)
		for _, line := range r.Stderr {
			fmt.Printf("    %s\n", line)
		}
		fmt.Printf("    full output: grep -F '[%s] ' %s\n", r.Job.Label, logPath)
	}

	switch {
	case interrupted:
		fmt.Println("\nInterrupted before completion.")
		return 130
	case len(failed) > 0:
		return 1
	default:
		return 0
	}
}

// scope resolves the -only flags into which categories to copy.
func scope(only []string) (home, apps, dirs bool) {
	if len(only) == 0 {
		return true, true, true
	}
	for _, o := range only {
		switch strings.ToLower(o) {
		case "home":
			home = true
		case "apps", "applications":
			apps = true
		case "dirs", "dir":
			dirs = true
		}
	}
	return home, apps, dirs
}

// resolveDirs builds the additional-directory list: defaults that exist locally,
// plus every -dir value (which must exist), de-duplicated and cleaned.
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
			return nil, fmt.Errorf("-dir %q: not an existing directory", d)
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

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "macmigrate: "+format+"\n", args...)
	return 2
}
