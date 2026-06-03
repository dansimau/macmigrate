//go:build integration

// End-to-end integration tests: build the real binary and run it against
// localhost over real ssh, with the built-in paths re-rooted into temporary
// directories via -root/-remote-root. The destination side therefore exercises
// the same machinery as a genuine migration: key-based ssh, passwordless
// `sudo -n /usr/bin/rsync`, remote-dir preparation, and ownership-preserving
// transfers.
//
// Requirements (anything missing skips with instructions, it never fails):
//   - run as root:  sudo go test -tags=integration -count=1 .
//   - Remote Login enabled (System Settings ▸ General ▸ Sharing)
//
// Setup auto-provisions a dedicated local user (macmigratetest) with an ssh
// key, membership in com.apple.access_ssh, and a NOPASSWD sudoers entry; both
// the user and /etc/sudoers.d/macmigrate-test persist across runs for speed.
// Set MACMIGRATE_TEST_CLEANUP=1 to delete them after the run.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dansimau/macmigrate/internal/migrate"
)

const (
	testUser       = "macmigratetest"
	testHome       = "/Users/" + testUser
	testUser2      = "macmigratetest2" // destination user for the cross-username test
	testHome2      = "/Users/" + testUser2
	sudoersPath    = "/etc/sudoers.d/macmigrate-test"
	fixturesDir    = "test/fixtures"
	harnessKeyName = "harness_key" // non-standard on purpose; see provisionTestUser
)

var (
	setupOnce sync.Once
	setupErr  error
	binDir    string // deleted in TestMain
	binPath   string
	testUID   int
	testGID   int
	test2UID  int
	test2GID  int
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	if os.Getenv("MACMIGRATE_TEST_CLEANUP") == "1" && os.Geteuid() == 0 {
		exec.Command("sysadminctl", "-deleteUser", testUser).Run()
		exec.Command("sysadminctl", "-deleteUser", testUser2).Run()
		os.Remove(sudoersPath)
		os.Remove(migrate.SudoersPath) // in case a setup test failed mid-way
	}
	os.Exit(code)
}

// requireEnv skips the test unless we are root and the one-time environment
// setup (binary build + test-user provisioning) succeeded.
func requireEnv(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("integration tests require root; run: sudo go test -tags=integration -count=1 .")
	}
	setupOnce.Do(func() {
		if setupErr = buildBinary(); setupErr != nil {
			return
		}
		setupErr = provisionTestUser()
	})
	if setupErr != nil {
		t.Skipf("integration environment unavailable: %v", setupErr)
	}
}

func buildBinary() error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	binDir, err = os.MkdirTemp("", "macmigrate-bin-")
	if err != nil {
		return err
	}
	// The setup/cleanup tests execute the binary as the (unprivileged) test
	// user, so the root-owned temp dir and binary must be world-readable.
	if err := os.Chmod(binDir, 0o755); err != nil {
		return err
	}
	binPath = filepath.Join(binDir, "macmigrate")
	if err := runCmd(goBin, "build", "-o", binPath, "."); err != nil {
		return err
	}
	return os.Chmod(binPath, 0o755)
}

// provisionTestUser creates the macmigratetest user (if missing) and equips it
// for the migration's ssh path: key in authorized_keys, ssh config carrying the
// non-interactive options (the tool passes no -o flags), membership in the
// Remote Login access group, and passwordless sudo. ssh resolves ~/.ssh via the
// real uid's passwd entry — not $HOME — so `sudo -E -u macmigratetest ssh`
// reads exactly the files provisioned here.
func provisionTestUser() error {
	if err := ensureUser(testUser, testHome); err != nil {
		return err
	}
	var err error
	if testUID, testGID, err = idsOf(testUser); err != nil {
		return err
	}

	sshDir := filepath.Join(testHome, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	// The harness key deliberately has a NON-standard name: `setup` reuses any
	// standard key (id_ed25519, …) it finds and then short-circuits as already
	// configured, but the setup test needs it to take the full generate+provision
	// path. Drop legacy standard-named keys from earlier harness versions.
	for _, legacy := range []string{"id_ed25519", "id_ed25519.pub"} {
		os.Remove(filepath.Join(sshDir, legacy))
	}
	key := filepath.Join(sshDir, harnessKeyName)
	if _, err := os.Stat(key); err != nil {
		if err := runCmd("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "macmigrate-integration-test", "-f", key); err != nil {
			return fmt.Errorf("generating test key: %w", err)
		}
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), pub, 0o600); err != nil {
		return err
	}

	keys, err := exec.Command("ssh-keyscan", "-H", "localhost").Output()
	if err != nil || len(bytes.TrimSpace(keys)) == 0 {
		return fmt.Errorf("ssh-keyscan localhost returned nothing — enable Remote Login (System Settings ▸ General ▸ Sharing)")
	}
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), keys, 0o600); err != nil {
		return err
	}

	// macmigrate invokes plain `ssh` with no -o options, so everything needed
	// for a non-interactive connection lives in the test user's config.
	config := fmt.Sprintf(`Host localhost
  User %s
  IdentityFile %s
  IdentitiesOnly yes
  BatchMode yes
  StrictHostKeyChecking accept-new
  UserKnownHostsFile %s
  PasswordAuthentication no
`, testUser, key, filepath.Join(sshDir, "known_hosts"))
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0o600); err != nil {
		return err
	}
	if err := runCmd("chown", "-R", fmt.Sprintf("%d:%d", testUID, testGID), sshDir); err != nil {
		return err
	}

	// The second user is the DESTINATION of the cross-username test. It is
	// reachable with the same harness key (the source user's ssh carries the
	// identity; the destination only needs the public key authorized) and can
	// sudo, but has no keys or config of its own.
	if err := ensureUser(testUser2, testHome2); err != nil {
		return err
	}
	if test2UID, test2GID, err = idsOf(testUser2); err != nil {
		return err
	}
	ssh2 := filepath.Join(testHome2, ".ssh")
	if err := os.MkdirAll(ssh2, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ssh2, "authorized_keys"), pub, 0o600); err != nil {
		return err
	}
	if err := runCmd("chown", "-R", fmt.Sprintf("%d:%d", test2UID, test2GID), ssh2); err != nil {
		return err
	}

	if err := installSudoers(); err != nil {
		return err
	}

	// Smoke-test the full destination path the tool depends on: key-based ssh
	// as the test user, then passwordless sudo on the "remote" — against both
	// destination users.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, dst := range []string{testUser, testUser2} {
		smoke := exec.CommandContext(ctx, "sudo", "-u", testUser, "ssh", dst+"@localhost", "sudo -n true")
		if out, err := smoke.CombinedOutput(); err != nil {
			return fmt.Errorf("ssh+sudo smoke test to %s failed (Remote Login enabled? MDM restrictions?): %v: %s",
				dst, err, bytes.TrimSpace(out))
		}
	}
	return nil
}

// ensureUser creates a local macOS user with a home directory and (when Remote
// Login is restricted to com.apple.access_ssh members) ssh access. Idempotent:
// an existing user is left untouched. The full name is the short name itself —
// full names must be unique, so a shared label would make the second user's
// creation fail. sysadminctl is also notorious for exiting 0 on failure, so the
// record is verified (with a short retry for Open Directory propagation)
// rather than trusting the exit code.
func ensureUser(name, home string) error {
	if !userExists(name) {
		pw, err := randomHex(16)
		if err != nil {
			return err
		}
		out, err := exec.Command("sysadminctl", "-addUser", name, "-fullName", name,
			"-password", pw, "-home", home, "-shell", "/bin/zsh").CombinedOutput()
		if err != nil {
			return fmt.Errorf("creating user %s: %v: %s", name, err, bytes.TrimSpace(out))
		}
		for i := 0; i < 20 && !userExists(name); i++ {
			time.Sleep(250 * time.Millisecond)
		}
		if !userExists(name) {
			return fmt.Errorf("sysadminctl reported success but %s does not exist: %s", name, bytes.TrimSpace(out))
		}
		if err := runCmd("createhomedir", "-c", "-u", name); err != nil {
			return fmt.Errorf("creating home for %s: %w", name, err)
		}
	}
	// Remote Login can be limited to members of com.apple.access_ssh; when the
	// group exists, a user outside it is refused even with a valid key.
	if exec.Command("dseditgroup", "-o", "read", "com.apple.access_ssh").Run() == nil {
		if err := runCmd("dseditgroup", "-o", "edit", "-a", name, "-t", "user", "com.apple.access_ssh"); err != nil {
			return fmt.Errorf("adding %s to com.apple.access_ssh: %w", name, err)
		}
	}
	return nil
}

func userExists(name string) bool {
	return exec.Command("dscl", ".", "-read", "/Users/"+name).Run() == nil
}

// installSudoers grants both test users passwordless sudo on the "destination",
// which the remote `sudo -n /usr/bin/rsync` / `sudo -n mkdir` / the chown pass
// require. The file is validated before install; the temporary name contains a
// dot, which sudo's includedir ignores.
func installSudoers() error {
	content := testUser + " ALL=(ALL) NOPASSWD: ALL\n" +
		testUser2 + " ALL=(ALL) NOPASSWD: ALL\n"
	if cur, err := os.ReadFile(sudoersPath); err == nil && string(cur) == content {
		return nil
	}
	tmp := sudoersPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o440); err != nil {
		return err
	}
	if err := runCmd("visudo", "-cf", tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("sudoers validation failed: %w", err)
	}
	return os.Rename(tmp, sudoersPath)
}

func idsOf(name string) (uid, gid int, err error) {
	for flag, dst := range map[string]*int{"-u": &uid, "-g": &gid} {
		out, err := exec.Command("id", flag, name).Output()
		if err != nil {
			return 0, 0, fmt.Errorf("id %s %s: %w", flag, name, err)
		}
		if *dst, err = strconv.Atoi(strings.TrimSpace(string(out))); err != nil {
			return 0, 0, err
		}
	}
	return uid, gid, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// fixture is one test's pair of temporary roots: localRoot stands in for the
// source Mac's "/" and remoteRoot for the destination's.
type fixture struct {
	localRoot  string
	remoteRoot string
	home       string // <localRoot>/Users/macmigratetest
	remoteHome string // <remoteRoot>/Users/macmigratetest
}

// newFixture copies test/fixtures/local into a fresh local root and
// test/fixtures/remote (the destination's pre-existing state) into a fresh
// remote root, then creates the skeleton directories git can't carry: the
// destination home (with Library) and Applications. Roots are world-readable
// so the unprivileged ssh user can traverse them (remoteList runs without
// sudo).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireEnv(t)
	f := &fixture{localRoot: mkRoot(t, "src"), remoteRoot: mkRoot(t, "dst")}
	f.home = filepath.Join(f.localRoot, "Users", testUser)
	f.remoteHome = filepath.Join(f.remoteRoot, "Users", testUser)

	copyTree(t, filepath.Join(fixturesDir, "local"), f.localRoot)
	copyTree(t, filepath.Join(fixturesDir, "remote"), f.remoteRoot)
	for _, d := range []string{filepath.Join(f.remoteHome, "Library"), filepath.Join(f.remoteRoot, "Applications")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// runMigrate runs the built binary against the fixture and returns its
// combined output and exit code. Homebrew comes first in PATH so the local
// side runs rsync 3 while the destination is pinned to /usr/bin/rsync by
// --rsync-path — the version pairing of a real old-Mac migration.
func (f *fixture) runMigrate(t *testing.T, extra ...string) (string, int) {
	t.Helper()
	return f.runMigrateTo(t, testUser+"@localhost", extra...)
}

// runMigrateTo is runMigrate with an explicit destination, for migrations to a
// different destination user.
func (f *fixture) runMigrateTo(t *testing.T, dest string, extra ...string) (string, int) {
	t.Helper()
	args := []string{
		"sync", dest,
		"--user", testUser,
		"--root", f.localRoot,
		"--remote-root", f.remoteRoot,
		"-j", "4",
		"--debug",
	}
	args = append(args, extra...)
	cmd := exec.Command(binPath, args...)
	cmd.Dir = t.TempDir() // the binary writes macmigrate-*.log to its cwd
	cmd.Env = envWithPath("/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running %s: %v\n%s", binPath, err, out.String())
		}
		code = ee.ExitCode()
	}
	t.Logf("macmigrate %s (exit %d):\n%s", strings.Join(args, " "), code, out.String())
	return out.String(), code
}

// runAsTestUser runs the built binary as the (unprivileged) test user — the
// way setup and cleanup run in real life, where they are NOT under sudo. env(1)
// pins HOME because the binary resolves ~/.ssh through it.
func runAsTestUser(t *testing.T, args ...string) (string, int) {
	t.Helper()
	argv := append([]string{
		"-u", testUser, "env",
		"HOME=" + testHome,
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		binPath,
	}, args...)
	cmd := exec.Command("sudo", argv...)
	cmd.Dir = t.TempDir()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running %s as %s: %v\n%s", binPath, testUser, err, out.String())
		}
		code = ee.ExitCode()
	}
	t.Logf("macmigrate %s (as %s, exit %d):\n%s", strings.Join(args, " "), testUser, code, out.String())
	return out.String(), code
}

func envWithPath(path string) []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "PATH=") {
			env = append(env, e)
		}
	}
	return append(env, "PATH="+path)
}

func mkRoot(t *testing.T, kind string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "macmigrate-it-"+kind+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("copying fixture %s: %v", src, err)
	}
}

func assertSuccess(t *testing.T, out string, code int) {
	t.Helper()
	if code != 0 {
		t.Fatalf("macmigrate exited %d", code)
	}
	if !strings.Contains(out, "0 partial · 0 failed") {
		t.Errorf("report does not show a clean run")
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("expected file missing: %v", err)
		return
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("expected to exist: %v", err)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("%s should not have been copied", path)
	}
}

func chownPath(t *testing.T, path string, uid, gid int) {
	t.Helper()
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
}

func chownTree(t *testing.T, root string, uid, gid int) {
	t.Helper()
	if err := runCmd("chown", "-R", fmt.Sprintf("%d:%d", uid, gid), root); err != nil {
		t.Fatal(err)
	}
}

func statUIDGID(t *testing.T, path string) (uid, gid int) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid)
}

func TestIntegrationHomeMigration(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink("notes.txt", filepath.Join(f.home, "link-to-notes")); err != nil {
		t.Fatal(err)
	}

	out, code := f.runMigrate(t)
	assertSuccess(t, out, code)

	assertContent(t, filepath.Join(f.remoteHome, "notes.txt"), "loose file at the top of home\n")
	assertExists(t, filepath.Join(f.remoteHome, ".hushlogin"))
	assertContent(t, filepath.Join(f.remoteHome, "Documents/report.txt"), "quarterly report\n")
	assertContent(t, filepath.Join(f.remoteHome, "Documents/sub/deep.txt"), "nested deep file\n")
	assertExists(t, filepath.Join(f.remoteHome, "Library/Mail/inbox.mbox/messages"))
	assertExists(t, filepath.Join(f.remoteHome, "Library/Preferences/com.example.test.plist"))

	// Symlinks arrive as symlinks (rsync -a).
	link := filepath.Join(f.remoteHome, "link-to-notes")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link-to-notes: err=%v, not a symlink", err)
	} else if target, _ := os.Readlink(link); target != "notes.txt" {
		t.Errorf("link target = %q, want notes.txt", target)
	}

	// Skips and excludes: trash and caches are skipped wholesale; the source's
	// authorized_keys must never clobber the destination's; Kandji MDM files
	// are excluded by basename.
	assertAbsent(t, filepath.Join(f.remoteHome, ".Trash"))
	assertAbsent(t, filepath.Join(f.remoteHome, "Library/Caches"))
	assertExists(t, filepath.Join(f.remoteHome, ".ssh/id_test"))
	assertAbsent(t, filepath.Join(f.remoteHome, ".ssh/authorized_keys"))
	assertAbsent(t, filepath.Join(f.remoteHome, "Library/Preferences/io.kandji.agent.plist"))
}

// TestIntegrationOwnershipPreserved is the core ownership check: files owned by
// three different users on the source arrive at the destination with uid/gid
// intact, which only works because both rsync ends run as root.
func TestIntegrationOwnershipPreserved(t *testing.T) {
	f := newFixture(t)
	owned := filepath.Join(f.home, "owned")
	const daemonUID, daemonGID = 1, 1
	for path, ids := range map[string][2]int{
		owned:                              {testUID, testGID},
		filepath.Join(owned, "user.txt"):   {testUID, testGID},
		filepath.Join(owned, "root.txt"):   {0, 0},
		filepath.Join(owned, "daemon.txt"): {daemonUID, daemonGID},
	} {
		if err := os.Chown(path, ids[0], ids[1]); err != nil {
			t.Fatal(err)
		}
	}

	out, code := f.runMigrate(t)
	assertSuccess(t, out, code)

	dst := filepath.Join(f.remoteHome, "owned")
	for path, want := range map[string][2]int{
		dst:                              {testUID, testGID}, // the directory itself
		filepath.Join(dst, "user.txt"):   {testUID, testGID},
		filepath.Join(dst, "root.txt"):   {0, 0},
		filepath.Join(dst, "daemon.txt"): {daemonUID, daemonGID},
	} {
		if uid, gid := statUIDGID(t, path); uid != want[0] || gid != want[1] {
			t.Errorf("%s owned by %d:%d, want %d:%d", path, uid, gid, want[0], want[1])
		}
	}
}

func TestIntegrationApps(t *testing.T) {
	f := newFixture(t)
	out, code := f.runMigrate(t)
	assertSuccess(t, out, code)

	// Foo.app is new on the destination: copied whole, executable bit intact.
	assertContent(t, filepath.Join(f.remoteRoot, "Applications/Foo.app/Contents/Info.plist"), "<plist>Foo v1.0</plist>\n")
	fooBin := filepath.Join(f.remoteRoot, "Applications/Foo.app/Contents/MacOS/Foo")
	if fi, err := os.Stat(fooBin); err != nil {
		t.Errorf("app binary missing: %v", err)
	} else if fi.Mode()&0o111 == 0 {
		t.Errorf("%s lost its executable bit (mode %v)", fooBin, fi.Mode())
	}

	// Bar.app already exists on the destination: skipped, and not overwritten.
	if !strings.Contains(out, "SKIP (exists): Bar.app") {
		t.Errorf("missing skip note for Bar.app")
	}
	assertContent(t, filepath.Join(f.remoteRoot, "Applications/Bar.app/Contents/Info.plist"),
		"<plist>remote Bar v1 — must survive</plist>\n")

	// Non-.app entries in /Applications are ignored.
	assertAbsent(t, filepath.Join(f.remoteRoot, "Applications/README.txt"))
}

// TestIntegrationDirs covers the additional-directory machinery: DefaultDirs
// that exist under the local root are picked up, created on the destination by
// PrepareRemoteDir, and copied with attributes.
func TestIntegrationDirs(t *testing.T) {
	f := newFixture(t)
	out, code := f.runMigrate(t)
	assertSuccess(t, out, code)

	assertContent(t, filepath.Join(f.remoteRoot, "Library/Fonts/TestFont.ttf"), "fake font bytes\n")
	tool := filepath.Join(f.remoteRoot, "usr/local/bin/tool")
	assertContent(t, tool, "#!/bin/sh\necho tool\n")
	if fi, err := os.Stat(tool); err == nil && fi.Mode()&0o111 == 0 {
		t.Errorf("%s lost its executable bit (mode %v)", tool, fi.Mode())
	}
}

func TestIntegrationDryRun(t *testing.T) {
	f := newFixture(t)
	out, code := f.runMigrate(t, "-n")
	if code != 0 {
		t.Fatalf("dry run exited %d", code)
	}
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("missing DRY RUN banner")
	}

	// Nothing may be written: not the home files, not apps, not the dirs that
	// PrepareRemoteDir would otherwise create.
	assertAbsent(t, filepath.Join(f.remoteHome, "notes.txt"))
	assertAbsent(t, filepath.Join(f.remoteHome, "Documents"))
	assertAbsent(t, filepath.Join(f.remoteRoot, "Applications/Foo.app"))
	assertAbsent(t, filepath.Join(f.remoteRoot, "Library/Fonts"))
}

// TestIntegrationCrossUserChown migrates to a DIFFERENT destination username —
// the scenario the post-rsync chown pass exists for. Files owned by the source
// user must arrive owned by the destination login user (rsync alone would
// leave them under the source uid), while files owned by anyone else (root,
// daemon) keep their owner: the pass targets only the probed source uid.
func TestIntegrationCrossUserChown(t *testing.T) {
	f := newFixture(t)
	// The destination user's home skeleton, as newFixture builds for testUser.
	remoteHome2 := filepath.Join(f.remoteRoot, "Users", testUser2)
	if err := os.MkdirAll(filepath.Join(remoteHome2, "Library"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The destination's ~/.ssh pre-exists — it must, for the migration's key
	// auth to work at all. Owned by root (which sshd's StrictModes accepts),
	// 0700, with the authorized_keys the migration depends on. If the sync
	// transfers the .ssh directory node itself, rsync re-owns it to the source
	// user and the (eventual) chown pass to the destination user — either of
	// which on a real machine trips StrictModes and cuts off ssh mid-sync.
	remoteSSH := filepath.Join(remoteHome2, ".ssh")
	if err := os.MkdirAll(remoteSSH, 0o700); err != nil {
		t.Fatal(err)
	}
	const remoteAuthKeys = "ssh-ed25519 REMOTEKEY the-key-the-migration-runs-over\n"
	if err := os.WriteFile(filepath.Join(remoteSSH, "authorized_keys"), []byte(remoteAuthKeys), 0o600); err != nil {
		t.Fatal(err)
	}
	chownPath(t, filepath.Join(remoteSSH, "authorized_keys"), test2UID, test2GID)

	// The source user owns their home content, as on a real Mac; root- and
	// daemon-owned files prove the pass leaves other owners alone.
	for _, p := range []string{
		"Documents", "Documents/report.txt", "Documents/sub", "Documents/sub/deep.txt",
		"notes.txt", "owned", "owned/user.txt", ".ssh", ".ssh/id_test",
	} {
		chownPath(t, filepath.Join(f.home, p), testUID, testGID)
	}
	chownPath(t, filepath.Join(f.home, "owned/root.txt"), 0, 0)
	chownPath(t, filepath.Join(f.home, "owned/daemon.txt"), 1, 1)
	// An app bundle owned by the source user exercises the app job's own
	// (bundle-scoped) chown pass.
	chownTree(t, filepath.Join(f.localRoot, "Applications/Foo.app"), testUID, testGID)

	out, code := f.runMigrateTo(t, testUser2+"@localhost")
	assertSuccess(t, out, code)
	if want := fmt.Sprintf("Usernames differ (%s → %s)", testUser, testUser2); !strings.Contains(out, want) {
		t.Errorf("output missing %q — chown pass not announced", want)
	}

	// Everything the source user owned now belongs to the destination user.
	for _, p := range []string{
		"Documents", "Documents/report.txt", "Documents/sub/deep.txt",
		"notes.txt", "owned/user.txt",
	} {
		if uid, _ := statUIDGID(t, filepath.Join(remoteHome2, p)); uid != test2UID {
			t.Errorf("%s owned by uid %d, want %s (%d)", p, uid, testUser2, test2UID)
		}
	}
	for _, app := range []string{"Applications/Foo.app", "Applications/Foo.app/Contents/Info.plist"} {
		if uid, _ := statUIDGID(t, filepath.Join(f.remoteRoot, app)); uid != test2UID {
			t.Errorf("%s owned by uid %d, want %s (%d)", app, uid, testUser2, test2UID)
		}
	}

	// Other owners are untouched: the pass chowns only the source uid's files.
	if uid, _ := statUIDGID(t, filepath.Join(remoteHome2, "owned/root.txt")); uid != 0 {
		t.Errorf("root.txt owned by uid %d, want 0", uid)
	}
	if uid, _ := statUIDGID(t, filepath.Join(remoteHome2, "owned/daemon.txt")); uid != 1 {
		t.Errorf("daemon.txt owned by uid %d, want 1", uid)
	}
	// Root-owned at the source but inside $HOME: root is not the probed uid, so
	// it keeps its owner — as stray root-owned files in a real home would.
	if uid, _ := statUIDGID(t, filepath.Join(remoteHome2, ".hushlogin")); uid != 0 {
		t.Errorf(".hushlogin owned by uid %d, want 0 (root-owned at source)", uid)
	}

	// THE INVARIANT THAT KEEPS THE MIGRATION ALIVE: the destination ~/.ssh
	// directory's own owner and mode are never touched (sshd's StrictModes
	// would otherwise refuse authorized_keys and kill every subsequent ssh
	// connection — including the chown pass that would have repaired it), and
	// the authorized_keys the migration authenticates with survives verbatim.
	// The .ssh *contents* still migrate and get re-owned like everything else.
	if uid, _ := statUIDGID(t, remoteSSH); uid != 0 {
		t.Errorf("destination .ssh dir owned by uid %d, want 0 (untouched) — the .ssh directory node was transferred", uid)
	}
	if fi, err := os.Stat(remoteSSH); err == nil && fi.Mode().Perm() != 0o700 {
		t.Errorf("destination .ssh dir mode = %v, want 0700 (untouched)", fi.Mode().Perm())
	}
	assertContent(t, filepath.Join(remoteSSH, "authorized_keys"), remoteAuthKeys)
	if uid, _ := statUIDGID(t, filepath.Join(remoteSSH, "authorized_keys")); uid != test2UID {
		t.Errorf("authorized_keys owned by uid %d, want %s (%d)", uid, testUser2, test2UID)
	}
	if uid, _ := statUIDGID(t, filepath.Join(remoteSSH, "id_test")); uid != test2UID {
		t.Errorf(".ssh/id_test owned by uid %d, want %s (%d) — contents must still migrate and be re-owned", uid, testUser2, test2UID)
	}
}

// TestIntegrationSetupAndCleanup drives the full provision/undo cycle as the
// test user: `setup` generates ~/.ssh/id_macmigrate (the harness key has a
// non-standard name precisely so no existing key is reused), installs it in
// authorized_keys, and writes the sudoers grant; a second run takes the
// idempotent fast path; `cleanup` then reverses both without touching the
// harness's own key. It runs unattended because the master connection
// authenticates via the harness key (ssh config) and the remote sudo is
// already passwordless via the harness's separate sudoers file.
func TestIntegrationSetupAndCleanup(t *testing.T) {
	requireEnv(t)
	generatedKey := filepath.Join(testHome, ".ssh", "id_macmigrate")
	authKeys := filepath.Join(testHome, ".ssh", "authorized_keys")
	removeArtifacts := func() {
		os.Remove(generatedKey)
		os.Remove(generatedKey + ".pub")
		os.Remove(migrate.SudoersPath)
	}
	removeArtifacts() // a previous failed run must not short-circuit this one
	t.Cleanup(removeArtifacts)

	// First run: full path — keygen, key install, sudoers install, verification.
	out, code := runAsTestUser(t, "setup", testUser+"@localhost")
	if code != 0 {
		t.Fatalf("setup exited %d", code)
	}
	for _, want := range []string{"Generated a new SSH key", "✓ Key-based ssh works", "✓ Passwordless sudo configured"} {
		if !strings.Contains(out, want) {
			t.Errorf("setup output missing %q", want)
		}
	}
	if uid, gid := statUIDGID(t, generatedKey); uid != testUID || gid != testGID {
		t.Errorf("%s owned by %d:%d, want %d:%d", generatedKey, uid, gid, testUID, testGID)
	}
	pub, err := os.ReadFile(generatedKey + ".pub")
	if err != nil {
		t.Fatalf("setup did not generate a public key: %v", err)
	}
	pubLine := strings.TrimSpace(string(pub))
	if ak := readFile(t, authKeys); !strings.Contains(ak, pubLine) {
		t.Errorf("authorized_keys does not contain the generated key:\n%s", ak)
	}
	wantSudoers := testUser + " ALL=(ALL) NOPASSWD: ALL\n"
	assertContent(t, migrate.SudoersPath, wantSudoers)
	if fi, err := os.Stat(migrate.SudoersPath); err == nil && fi.Mode().Perm() != 0o440 {
		t.Errorf("%s mode = %v, want 0440", migrate.SudoersPath, fi.Mode().Perm())
	}

	// Second run: everything in place — the prompt-free fast path.
	out, code = runAsTestUser(t, "setup", testUser+"@localhost")
	if code != 0 {
		t.Fatalf("re-run of setup exited %d", code)
	}
	if !strings.Contains(out, "✓ Already set up") {
		t.Errorf("re-run of setup did not take the fast path")
	}

	// Cleanup: sudoers grant and installed key removed, harness key untouched.
	out, code = runAsTestUser(t, "cleanup", testUser+"@localhost")
	if code != 0 {
		t.Fatalf("cleanup exited %d", code)
	}
	for _, want := range []string{"✓ sudoers grant removed", "✓ public key removed"} {
		if !strings.Contains(out, want) {
			t.Errorf("cleanup output missing %q", want)
		}
	}
	assertAbsent(t, migrate.SudoersPath)
	harnessPub := strings.TrimSpace(readFile(t, filepath.Join(testHome, ".ssh", harnessKeyName+".pub")))
	ak := readFile(t, authKeys)
	if strings.Contains(ak, pubLine) {
		t.Errorf("authorized_keys still contains the macmigrate key after cleanup:\n%s", ak)
	}
	if !strings.Contains(ak, harnessPub) {
		t.Errorf("cleanup removed the harness key from authorized_keys:\n%s", ak)
	}
	// The very access the harness depends on must have survived cleanup.
	if err := runCmd("sudo", "-u", testUser, "ssh", testUser+"@localhost", "true"); err != nil {
		t.Errorf("ssh as %s broken after cleanup: %v", testUser, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}
