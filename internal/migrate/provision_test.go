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
		// -S: the password comes from the session's stdin, not a tty prompt.
		`sudo -S -p '' /bin/sh -c`,
		// The login shell (the user, not root) resolves the username into $1.
		`'printf "%s ALL=(ALL) NOPASSWD: ALL\n" "$1" > /etc/sudoers.d/macmigrate`,
		"chmod 440 /etc/sudoers.d/macmigrate",
		"visudo -cf /etc/sudoers.d/macmigrate",
		// A malformed file is removed, not left breaking sudo.
		`|| { rm -f /etc/sudoers.d/macmigrate; exit 1; }' sh "$(id -un)"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("installSudoersCmd missing %q\ngot: %s", want, got)
		}
	}
}

// TestProvisionCmdSingleSession guards the one-password contract: the key
// install and the sudoers install run chained in a single remote command, so
// one ssh session (and one password) covers both.
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
