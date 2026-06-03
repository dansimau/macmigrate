package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/dansimau/macmigrate/internal/display"
	"github.com/dansimau/macmigrate/internal/migrate"
	"github.com/dansimau/macmigrate/internal/xexec"
	"github.com/spf13/cobra"
)

const rsyncBin = "rsync"

var (
	syncUser       string
	syncParallel   int
	syncDryRun     bool
	syncIdentity   string
	syncExcludes   []string
	syncIncludes   []string
	syncRoot       string
	syncRemoteRoot string
)

var syncCmd = &cobra.Command{
	Use:   "sync <dest>",
	Short: "Migrate the home directory and apps to the destination",
	Long: "sync copies $HOME, third-party /Applications, and a set of system\n" +
		"directories to the destination over ssh, running many rsync transfers in\n" +
		"parallel. It re-runs itself under sudo so rsync can read every local file.\n\n" +
		"Run `macmigrate setup <dest>` first to configure key-based ssh and\n" +
		"passwordless sudo on the destination.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSync(args[0])
	},
}

func init() {
	syncCmd.Flags().StringVar(&syncUser, "user", "",
		"username whose home (/Users/<user>) to migrate; set automatically when re-running under sudo")
	syncCmd.Flags().IntVarP(&syncParallel, "jobs", "j", 4, "maximum number of parallel rsync jobs")
	syncCmd.Flags().BoolVarP(&syncDryRun, "dry-run", "n", false, "dry run: pass --dry-run to rsync (no files written)")
	syncCmd.Flags().StringVarP(&syncIdentity, "identity", "i", "",
		"SSH key to connect with (defaults to ~/.ssh/"+macmigrateKeyName+" if present, else ssh's default)")
	syncCmd.Flags().StringArrayVar(&syncExcludes, "exclude", nil,
		"home/Library entry to skip, e.g. 'Library/Containers' (repeatable; adds to defaults)")
	syncCmd.Flags().StringArrayVar(&syncIncludes, "include", nil,
		"additional absolute directory to migrate to the same path, as root (repeatable; adds to defaults)")
	syncCmd.Flags().StringVar(&syncRoot, "root", "/",
		"local root prefix for the built-in paths (/Users/<user>, /Applications, default dirs); for testing")
	syncCmd.Flags().StringVar(&syncRemoteRoot, "remote-root", "/",
		"destination root prefix for the built-in paths; for testing")
	rootCmd.AddCommand(syncCmd)
}

func runSync(dest string) error {
	if syncParallel < 1 {
		return fail("--jobs must be at least 1")
	}

	// macmigrate copies files as root locally so rsync can read every file
	// regardless of owner or mode. Started without sudo, it re-runs itself under
	// sudo (which prompts for a password), passing --user so the root instance
	// knows whose home to migrate.
	if os.Geteuid() != 0 {
		return reexecUnderSudo(syncUser)
	}

	if syncUser == "" {
		return fail("--user is required (set automatically when re-running under sudo)")
	}
	home := filepath.Join(syncRoot, "Users", syncUser)
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		return fail("home directory %s does not exist", home)
	}

	// ssh connects as the invoking user (root has no keys/agent). Resolve the
	// identity from that user's ~/.ssh — auto-selecting the macmigrate key when
	// setup generated one — so the connection uses it without an explicit -i.
	identity, err := resolveSyncIdentity(home, syncIdentity)
	if err != nil {
		return fail("%v", err)
	}
	ssh := migrate.SSH{User: syncUser, Identity: identity}

	dirs, err := resolveDirs(syncRoot, migrate.DefaultDirs, syncIncludes)
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

	fmt.Printf("Connecting to %s …\n", dest)
	rhome, err := ssh.RemoteHome(ctx, dest)
	if err != nil {
		return fail("%v\n\nRun `macmigrate setup %s` to configure key-based ssh, or enable Remote Login\non the destination (System Settings ▸ General ▸ Sharing) and set up key-based ssh\nso parallel jobs don't hit password prompts.", err, dest)
	}
	if syncRemoteRoot != "/" {
		// Under a test root the destination "home" lives inside it; the ssh
		// resolution above still serves as the connectivity preflight.
		rhome = path.Join(syncRemoteRoot, "Users", syncUser)
	}
	migrate.Debugf(debug, "remote home on %s = %s", dest, rhome)

	// Everything is copied with `sudo rsync` so file ownership is preserved,
	// which can't prompt for a password across parallel ssh connections. Require
	// passwordless sudo up front and fail loudly if it's missing.
	if !syncDryRun && !ssh.CanSudo(ctx, dest) {
		return sudoRequiredError(dest)
	}

	// When the usernames differ, the destination can't map the source owner by
	// name, so each transfer is followed by a chown pass (see migrate.Chown).
	// Probe once which uid the source user's files arrive as on the destination.
	var chownUID string
	dstUser, err := ssh.RemoteUsername(ctx, dest)
	if err != nil {
		return fail("resolving destination username: %v", err)
	}
	if dstUser != syncUser {
		srcUID, err := uidForUser(syncUser)
		if err != nil {
			return fail("looking up local user %s: %v", syncUser, err)
		}
		chownUID, err = ssh.ChownUID(ctx, dest, syncUser, srcUID)
		if err != nil {
			return fail("resolving source uid on destination: %v", err)
		}
		fmt.Printf("Usernames differ (%s → %s): fixing file ownership after each transfer\n", syncUser, dstUser)
	}

	// Create each additional directory (and any missing parents) on the
	// destination before its contents are copied; rsync sets the ownership of
	// the entries inside. A prep failure is fatal — we don't quietly skip work.
	for _, dir := range dirs {
		rdir := migrate.RemoteDirPath(syncRoot, syncRemoteRoot, dir)
		if !syncDryRun {
			fmt.Printf("Preparing %s on %s …\n", rdir, dest)
		}
		if err := ssh.PrepareRemoteDir(ctx, dest, rdir, syncDryRun); err != nil {
			return fail("preparing %s on destination: %v", rdir, err)
		}
	}

	opt := migrate.Options{
		Dest:          dest,
		SSH:           ssh,
		Home:          home,
		RemoteHome:    rhome,
		AppsDir:       filepath.Join(syncRoot, "Applications"),
		RemoteAppsDir: path.Join(syncRemoteRoot, "Applications"),
		Root:          syncRoot,
		RemoteRoot:    syncRemoteRoot,
		Dirs:          dirs,
		SkipNames:     concat(migrate.DefaultSkip, syncExcludes),
		RsyncExclude:  migrate.DefaultRsyncExclude,
		DoHome:        true,
		DoApps:        true,
		DoDirs:        len(dirs) > 0,
		Debug:         debug,
		ChownUID:      chownUID,
	}
	jobs, notes, err := migrate.BuildJobs(ctx, opt)
	if err != nil {
		return fail("%v", err)
	}
	for _, j := range jobs {
		// Shell-quoted so multi-word args (--rsync-path, -e) paste back correctly.
		migrate.Debugf(debug, "job %s: rsync %s", j.Label, shellescape.QuoteCommand(j.Args(syncDryRun)))
	}
	for _, n := range notes {
		fmt.Println(n)
	}
	if len(jobs) == 0 {
		fmt.Println("Nothing to copy.")
		return nil
	}

	lp := fmt.Sprintf("macmigrate-%s.log", time.Now().Format("20060102-150405"))

	fmt.Printf("macmigrate → %s  (%s)\n", dest, rhome)
	fmt.Printf("  %d jobs · up to %d parallel · log: %s\n", len(jobs), syncParallel, lp)
	if syncDryRun {
		fmt.Println("  DRY RUN — no files will be written")
	}

	disp, err := display.New(syncParallel, lp)
	if err != nil {
		return fail("opening log file: %v", err)
	}
	defer disp.Close() // safety net; Close is idempotent

	// Keep both Macs awake for the whole transfer; stop() lets them sleep again.
	stopCaffeinate := migrate.Caffeinate(ssh, dest)
	defer stopCaffeinate()

	start := time.Now()
	results := migrate.Run(ctx, jobs, syncParallel, disp, rsyncBin, ssh, dest, syncDryRun)
	disp.Close() // tear down the live region before printing the report

	if code := report(results, lp, time.Since(start), ctx.Err() != nil); code != 0 {
		return &exitErr{code: code}
	}
	return nil
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
func reexecUnderSudo(userFlag string) error {
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
	// the root instance's ssh can talk to the user's ssh-agent. Append --user
	// (a sync flag, so after the subcommand) only if the user didn't already
	// pass one — otherwise it's already in os.Args.
	args := []string{"-E", self}
	args = append(args, os.Args[1:]...)
	if userFlag == "" {
		args = append(args, "--user", username)
	}

	fmt.Printf("Re-running under sudo as root (migrating /Users/%s) …\n", username)
	cmd := xexec.Command("sudo", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &exitErr{code: ee.ExitCode()}
		}
		return fail("running under sudo: %v", err)
	}
	return nil
}

// resolveDirs builds the additional-directory list: defaults that exist locally
// (looked up under root), plus every --include value (which must exist and is
// taken literally), de-duplicated and cleaned.
func resolveDirs(root string, defaults, extra []string) ([]string, error) {
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
		d = filepath.Join(root, d)
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			add(d)
		}
	}
	for _, d := range extra {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("--include %q: not an existing directory", d)
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
		return fmt.Errorf("%s is rsync %d.%d; --info=progress2 needs >= 3.1 (try Homebrew rsync)", bin, major, minor)
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

// sudoRequiredError returns the passwordless-sudo remediation as a fatal error.
// Every transfer runs as root to preserve ownership, so the migration can't
// proceed without it.
func sudoRequiredError(dest string) error {
	return &exitErr{
		code: 2,
		msg: fmt.Sprintf("passwordless sudo is not available on %s.\n", dest) +
			"  Files are copied with `sudo rsync` so ownership is preserved, which can't\n" +
			"  prompt for a password over parallel ssh connections.\n" +
			fmt.Sprintf("  Run `macmigrate setup %s` to configure this automatically, or enable\n", dest) +
			"  passwordless sudo for your user ON THE DESTINATION by hand, e.g.:\n" +
			`    echo "$(id -un) ALL=(ALL) NOPASSWD: ALL" | sudo tee /etc/sudoers.d/macmigrate >/dev/null` + "\n" +
			"    sudo chmod 440 /etc/sudoers.d/macmigrate",
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
