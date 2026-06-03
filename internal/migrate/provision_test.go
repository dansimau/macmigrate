package migrate

import (
	"strings"
	"testing"
)

func TestInstallAuthorizedKeyCmd(t *testing.T) {
	pub := "ssh-ed25519 AAAAbody macmigrate@host"
	got := installAuthorizedKeyCmd(pub)

	// ~/.ssh and authorized_keys get the right modes before any write, and the
	// file is guaranteed to end in a newline before the key is appended (an
	// unterminated last line would swallow the key into the previous entry).
	for _, want := range []string{
		"mkdir -p ~/.ssh && chmod 700 ~/.ssh",
		"chmod 600 $HOME/.ssh/authorized_keys",
		`[ -z "$(tail -c1 $HOME/.ssh/authorized_keys)" ] || echo >> $HOME/.ssh/authorized_keys`,
		"grep -qxF 'ssh-ed25519 AAAAbody macmigrate@host' $HOME/.ssh/authorized_keys",
		"printf '%s\\n' 'ssh-ed25519 AAAAbody macmigrate@host' >> $HOME/.ssh/authorized_keys",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("installAuthorizedKeyCmd missing %q\ngot: %s", want, got)
		}
	}
	// The append must be guarded by the grep (idempotent), so they share a || .
	if !strings.Contains(got, "grep -qxF") || !strings.Contains(got, "|| printf") {
		t.Errorf("append is not guarded by grep: %s", got)
	}
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
	got := provisionCmd(pub)
	want := installAuthorizedKeyCmd(pub) + " && " + installSudoersCmd()
	if got != want {
		t.Errorf("provisionCmd = %s\nwant %s", got, want)
	}
}

func TestRemoveAuthorizedKeyCmd(t *testing.T) {
	got := removeAuthorizedKeyCmd("AAAAbody")
	for _, want := range []string{
		"if [ -f $HOME/.ssh/authorized_keys ]; then",
		"grep -vF AAAAbody $HOME/.ssh/authorized_keys > $HOME/.ssh/authorized_keys.macmigrate.tmp || true",
		"mv $HOME/.ssh/authorized_keys.macmigrate.tmp $HOME/.ssh/authorized_keys",
		"chmod 600 $HOME/.ssh/authorized_keys",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("removeAuthorizedKeyCmd missing %q\ngot: %s", want, got)
		}
	}
}
