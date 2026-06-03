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
)

const (
	testUser    = "macmigratetest"
	testHome    = "/Users/" + testUser
	sudoersPath = "/etc/sudoers.d/macmigrate-test"
	fixturesDir = "test/fixtures"
)

var (
	setupOnce sync.Once
	setupErr  error
	binDir    string // deleted in TestMain
	binPath   string
	testUID   int
	testGID   int
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	if os.Getenv("MACMIGRATE_TEST_CLEANUP") == "1" && os.Geteuid() == 0 {
		exec.Command("sysadminctl", "-deleteUser", testUser).Run()
		os.Remove(sudoersPath)
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
	binPath = filepath.Join(binDir, "macmigrate")
	return runCmd(goBin, "build", "-o", binPath, ".")
}

// provisionTestUser creates the macmigratetest user (if missing) and equips it
// for the migration's ssh path: key in authorized_keys, ssh config carrying the
// non-interactive options (the tool passes no -o flags), membership in the
// Remote Login access group, and passwordless sudo. ssh resolves ~/.ssh via the
// real uid's passwd entry — not $HOME — so `sudo -E -u macmigratetest ssh`
// reads exactly the files provisioned here.
func provisionTestUser() error {
	if exec.Command("dscl", ".", "-read", testHome).Run() != nil {
		pw, err := randomHex(16)
		if err != nil {
			return err
		}
		if err := runCmd("sysadminctl", "-addUser", testUser, "-fullName", "macmigrate integration test",
			"-password", pw, "-home", testHome, "-shell", "/bin/zsh"); err != nil {
			return fmt.Errorf("creating user %s: %w", testUser, err)
		}
		if err := runCmd("createhomedir", "-c", "-u", testUser); err != nil {
			return fmt.Errorf("creating home for %s: %w", testUser, err)
		}
	}
	var err error
	if testUID, err = idOf("-u"); err != nil {
		return err
	}
	if testGID, err = idOf("-g"); err != nil {
		return err
	}

	// Remote Login can be limited to members of com.apple.access_ssh; when the
	// group exists, a user outside it is refused even with a valid key.
	if exec.Command("dseditgroup", "-o", "read", "com.apple.access_ssh").Run() == nil {
		if err := runCmd("dseditgroup", "-o", "edit", "-a", testUser, "-t", "user", "com.apple.access_ssh"); err != nil {
			return fmt.Errorf("adding %s to com.apple.access_ssh: %w", testUser, err)
		}
	}

	sshDir := filepath.Join(testHome, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	key := filepath.Join(sshDir, "id_ed25519")
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

	if err := installSudoers(); err != nil {
		return err
	}

	// Smoke-test the full destination path the tool depends on: key-based ssh
	// as the test user, then passwordless sudo on the "remote".
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	smoke := exec.CommandContext(ctx, "sudo", "-u", testUser, "ssh", testUser+"@localhost", "sudo -n true")
	if out, err := smoke.CombinedOutput(); err != nil {
		return fmt.Errorf("ssh+sudo smoke test failed (Remote Login enabled? MDM restrictions?): %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// installSudoers grants the test user passwordless sudo on the "destination",
// which the remote `sudo -n /usr/bin/rsync` / `sudo -n mkdir` require. The file
// is validated before install; the temporary name contains a dot, which sudo's
// includedir ignores.
func installSudoers() error {
	content := testUser + " ALL=(ALL) NOPASSWD: ALL\n"
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

func idOf(flag string) (int, error) {
	out, err := exec.Command("id", flag, testUser).Output()
	if err != nil {
		return 0, fmt.Errorf("id %s %s: %w", flag, testUser, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
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
	args := []string{
		"sync", testUser + "@localhost",
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
