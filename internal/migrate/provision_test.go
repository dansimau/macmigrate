package migrate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallAuthorizedKeyCmd(t *testing.T) {
	pub := "ssh-ed25519 AAAAbody macmigrate@host"
	got := installAuthorizedKeyCmd(pub, "AAAAbody")

	// ~/.ssh and authorized_keys get the right modes before any write, the file
	// is guaranteed to end in a newline before the key is appended (an
	// unterminated last line would swallow the key into the previous entry),
	// the presence check ignores comment lines, and the appended line carries
	// the tag cleanup matches on.
	for _, want := range []string{
		"mkdir -p ~/.ssh && chmod 700 ~/.ssh",
		"chmod 600 $HOME/.ssh/authorized_keys",
		`[ -z "$(tail -c1 $HOME/.ssh/authorized_keys)" ] || echo >> $HOME/.ssh/authorized_keys`,
		"grep -v -e '^[[:space:]]*#' $HOME/.ssh/authorized_keys | grep -qF AAAAbody",
		"printf '%s\\n' 'ssh-ed25519 AAAAbody macmigrate@host " + authorizedKeyTag + "' >> $HOME/.ssh/authorized_keys",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("installAuthorizedKeyCmd missing %q\ngot: %s", want, got)
		}
	}
	// The append must be guarded by the grep (idempotent), so they share a || .
	if !strings.Contains(got, "grep -qF") || !strings.Contains(got, "|| printf") {
		t.Errorf("append is not guarded by grep: %s", got)
	}
}

// runRemoteCmd executes a provisioning shell snippet the way the destination's
// login shell would, with $HOME (and ~, via sh's HOME) pointed at a temp dir.
func runRemoteCmd(t *testing.T, home, cmd string) {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
	c.Env = append(os.Environ(), "HOME="+home)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("sh -c %q: %v: %s", cmd, err, out)
	}
}

// TestAuthorizedKeyInstallRemoveBehavior runs the real install/remove commands
// against a temp $HOME and checks the invariant the tag exists for: cleanup
// removes exactly what setup added — never a pre-existing entry for the same
// key, and never an unrelated key.
func TestAuthorizedKeyInstallRemoveBehavior(t *testing.T) {
	const (
		pub   = "ssh-ed25519 AAAAfresh dan@old-mac"
		body  = "AAAAfresh"
		other = "ssh-ed25519 AAAAother someone@elsewhere\n"
	)
	akPath := func(home string) string { return filepath.Join(home, ".ssh", "authorized_keys") }

	t.Run("fresh key: added tagged, removed by cleanup, others untouched", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(akPath(home), []byte(other), 0o600); err != nil {
			t.Fatal(err)
		}

		runRemoteCmd(t, home, installAuthorizedKeyCmd(pub, body))
		ak, _ := os.ReadFile(akPath(home))
		if want := pub + " " + authorizedKeyTag + "\n"; !strings.Contains(string(ak), want) {
			t.Fatalf("install did not append the tagged line:\n%s", ak)
		}

		// Re-running the install must not duplicate the entry.
		runRemoteCmd(t, home, installAuthorizedKeyCmd(pub, body))
		ak, _ = os.ReadFile(akPath(home))
		if strings.Count(string(ak), body) != 1 {
			t.Fatalf("re-install duplicated the key:\n%s", ak)
		}

		runRemoteCmd(t, home, removeAuthorizedKeyCmd(body))
		ak, _ = os.ReadFile(akPath(home))
		if strings.Contains(string(ak), body) {
			t.Errorf("remove left the setup-added key behind:\n%s", ak)
		}
		if !strings.Contains(string(ak), "AAAAother") {
			t.Errorf("remove deleted an unrelated key:\n%s", ak)
		}
	})

	t.Run("commented-out copy of the key does not suppress the install", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		// The same key body exists only on lines sshd ignores: a comment line
		// and an indented comment line. Neither authorizes the key, so setup
		// must still append an active entry.
		disabled := "# ssh-ed25519 " + body + " disabled-by-user\n" +
			"  # ssh-ed25519 " + body + " also-disabled\n"
		if err := os.WriteFile(akPath(home), []byte(disabled), 0o600); err != nil {
			t.Fatal(err)
		}

		runRemoteCmd(t, home, installAuthorizedKeyCmd(pub, body))
		ak, _ := os.ReadFile(akPath(home))
		if want := pub + " " + authorizedKeyTag + "\n"; !strings.Contains(string(ak), want) {
			t.Fatalf("install did not append an active entry over commented-out copies:\n%s", ak)
		}

		// cleanup removes only the tagged active line; the user's commented-out
		// lines (no tag) stay.
		runRemoteCmd(t, home, removeAuthorizedKeyCmd(body))
		ak, _ = os.ReadFile(akPath(home))
		if string(ak) != disabled {
			t.Errorf("remove did not restore the original commented-out-only file:\n%s", ak)
		}
	})

	t.Run("pre-existing key: never re-added, never removed", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		// The user authorized this key themselves, with their own comment —
		// different from the local .pub's, as is typical.
		preexisting := "ssh-ed25519 " + body + " dans-own-comment\n"
		if err := os.WriteFile(akPath(home), []byte(preexisting), 0o600); err != nil {
			t.Fatal(err)
		}

		// setup must recognise the key by body and add nothing.
		runRemoteCmd(t, home, installAuthorizedKeyCmd(pub, body))
		ak, _ := os.ReadFile(akPath(home))
		if string(ak) != preexisting {
			t.Fatalf("install touched a file that already authorized the key:\n%s", ak)
		}

		// THE BUG THIS GUARDS AGAINST: cleanup must not revoke access the user
		// had before macmigrate ever ran.
		runRemoteCmd(t, home, removeAuthorizedKeyCmd(body))
		ak, _ = os.ReadFile(akPath(home))
		if string(ak) != preexisting {
			t.Errorf("remove deleted the user's pre-existing key:\n%s", ak)
		}
	})
}

func TestInstallSudoersCmd(t *testing.T) {
	got := installSudoersCmd()
	for _, want := range []string{
		// `id -un` is expanded by the remote login shell (the user, not root).
		`echo "$(id -un) ALL=(ALL) NOPASSWD: ALL" | sudo tee /etc/sudoers.d/macmigrate >/dev/null`,
		"sudo chmod 440 /etc/sudoers.d/macmigrate",
		"sudo visudo -cf /etc/sudoers.d/macmigrate",
		// A malformed file is removed, not left breaking sudo.
		"|| { sudo rm -f /etc/sudoers.d/macmigrate; exit 1; }",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("installSudoersCmd missing %q\ngot: %s", want, got)
		}
	}
}

// TestProvisionCmdSingleSession guards the one-session contract: the key
// install and the sudoers install run chained in a single remote command over
// the master connection, so the user authenticates to ssh only once.
func TestProvisionCmdSingleSession(t *testing.T) {
	pub := "ssh-ed25519 AAAAbody macmigrate@host"
	got := provisionCmd(pub, "AAAAbody")
	want := installAuthorizedKeyCmd(pub, "AAAAbody") + " && " + installSudoersCmd()
	if got != want {
		t.Errorf("provisionCmd = %s\nwant %s", got, want)
	}
}

func TestRemoveAuthorizedKeyCmd(t *testing.T) {
	got := removeAuthorizedKeyCmd("AAAAbody")
	for _, want := range []string{
		"if [ -f $HOME/.ssh/authorized_keys ]; then",
		// Only lines carrying both the key body and the macmigrate tag go;
		// pre-existing (untagged) entries for the same key survive.
		"grep -v -e 'AAAAbody.*" + authorizedKeyTag + "' $HOME/.ssh/authorized_keys > $HOME/.ssh/authorized_keys.macmigrate.tmp || true",
		"mv $HOME/.ssh/authorized_keys.macmigrate.tmp $HOME/.ssh/authorized_keys",
		"chmod 600 $HOME/.ssh/authorized_keys",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("removeAuthorizedKeyCmd missing %q\ngot: %s", want, got)
		}
	}
}
