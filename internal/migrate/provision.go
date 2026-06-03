package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"github.com/dansimau/macmigrate/internal/xexec"
)

// SudoersPath is the drop-in file setup writes (and cleanup removes) to grant
// the destination login user passwordless sudo, which the parallel rsync and
// chown passes require.
const SudoersPath = "/etc/sudoers.d/macmigrate"

// authorizedKeysPath is the destination user's authorized_keys, expanded by the
// remote login shell ($HOME, not ~, so it also resolves under sudo-free ssh).
const authorizedKeysPath = "$HOME/.ssh/authorized_keys"

// Provision installs pubKey in the destination's authorized_keys and writes the
// passwordless-sudo drop-in, all in a single ssh session so the destination
// password is needed exactly once: ssh authentication (when the key isn't
// authorized yet) reads it through an SSH_ASKPASS helper, and the remote
// `sudo -S` reads the same password from the piped stdin. s should carry the
// identity being installed — if it's already authorized, ssh uses it and the
// password only feeds sudo.
func Provision(ctx context.Context, s SSH, dest, pubKey, password string) error {
	argv := append(s.prefix(),
		// Host-key confirmation can't be answered interactively here (askpass
		// would feed it the password), so accept unseen hosts; changed keys
		// still hard-fail. A wrong password should fail fast, not retry.
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "NumberOfPasswordPrompts=1",
		dest, provisionCmd(pubKey))
	cmd := xexec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(password + "\n")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	askpass, cleanup, err := askpassScript()
	if err != nil {
		return err
	}
	defer cleanup()
	env := append(os.Environ(),
		"SSH_ASKPASS="+askpass,
		"SSH_ASKPASS_REQUIRE=force",
		"MACMIGRATE_ASKPASS="+password,
	)
	if os.Getenv("DISPLAY") == "" {
		env = append(env, "DISPLAY=:0") // pre-8.4 ssh only consults askpass when DISPLAY is set
	}
	cmd.Env = env

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("provisioning %s: %w", dest, err)
	}
	return nil
}

// askpassScript writes a throwaway SSH_ASKPASS helper that prints the password
// ssh asks for. The password itself stays out of the script (and off disk) —
// the helper reads it from the environment Provision sets on the ssh process.
func askpassScript() (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "macmigrate-askpass")
	if err != nil {
		return "", nil, fmt.Errorf("creating askpass helper: %w", err)
	}
	path = filepath.Join(dir, "askpass.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$MACMIGRATE_ASKPASS\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("creating askpass helper: %w", err)
	}
	return path, func() { os.RemoveAll(dir) }, nil
}

// provisionCmd chains the key install and the sudoers install into one remote
// command, so both happen in the single password-authenticated session.
func provisionCmd(pubKey string) string {
	return installAuthorizedKeyCmd(pubKey) + " && " + installSudoersCmd()
}

// installAuthorizedKeyCmd builds the idempotent key-install command: ensure
// ~/.ssh (700) and authorized_keys (600) exist and that the file ends in a
// newline (an unterminated last line would swallow the appended key into the
// previous entry's comment), then append the key only if an identical line
// isn't already present (grep -qxF), so re-running setup is safe.
func installAuthorizedKeyCmd(pubKey string) string {
	q := shellescape.Quote(pubKey)
	f := authorizedKeysPath
	return "mkdir -p ~/.ssh && chmod 700 ~/.ssh" +
		" && touch " + f + " && chmod 600 " + f +
		" && { [ ! -s " + f + ` ] || [ -z "$(tail -c1 ` + f + `)" ] || echo >> ` + f + "; }" +
		" && { grep -qxF " + q + " " + f +
		" || printf '%s\\n' " + q + " >> " + f + "; }"
}

// installSudoersCmd grants the destination login user passwordless sudo via a
// validated drop-in. `id -un` is expanded by the remote login shell (the user,
// not root) and handed to the elevated sh as $1. sudo runs with -S so it reads
// the password from the session's stdin — the same one the user already
// supplied — instead of prompting on the tty. visudo -cf rejects a malformed
// file, which is removed rather than left breaking sudo.
func installSudoersCmd() string {
	return `sudo -S -p '' /bin/sh -c ` +
		`'printf "%s ALL=(ALL) NOPASSWD: ALL\n" "$1" > ` + SudoersPath +
		` && chmod 440 ` + SudoersPath +
		` && visudo -cf ` + SudoersPath +
		` || { rm -f ` + SudoersPath + `; exit 1; }' sh "$(id -un)"`
}

// VerifyKey confirms the destination accepts key-only auth, failing fast instead
// of letting a misconfigured key fall through to a password prompt mid-migration.
// The caller passes an SSH value carrying the identity and BatchMode=yes.
func VerifyKey(ctx context.Context, s SSH, dest string) error {
	if _, err := s.Capture(ctx, dest, "true"); err != nil {
		return fmt.Errorf("key-based ssh to %s did not work: %w", dest, err)
	}
	return nil
}

// RemoveSudoers deletes the sudoers drop-in. It may need a password if
// passwordless sudo was already revoked, so it runs interactively (set s.TTY).
func RemoveSudoers(ctx context.Context, s SSH, dest string) error {
	return s.Run(ctx, dest, "sudo rm -f "+shellescape.Quote(SudoersPath))
}

// RemoveAuthorizedKey strips every authorized_keys line containing keyBody (the
// key's base64 body, matched so a differing trailing comment doesn't hide it)
// from the destination user's authorized_keys. It is a no-op when the file is
// absent and needs no sudo — it's the user's own file.
func RemoveAuthorizedKey(ctx context.Context, s SSH, dest, keyBody string) error {
	return s.Run(ctx, dest, removeAuthorizedKeyCmd(keyBody))
}

// removeAuthorizedKeyCmd rewrites authorized_keys without the matching line(s).
// grep -vF can exit 1 when it selects nothing (e.g. the key was the only line),
// so `|| true` keeps that from aborting the rewrite; the tmp file is then moved
// back, preserving 600.
func removeAuthorizedKeyCmd(keyBody string) string {
	f := authorizedKeysPath
	q := shellescape.Quote(keyBody)
	return "if [ -f " + f + " ]; then " +
		"grep -vF " + q + " " + f + " > " + f + ".macmigrate.tmp || true; " +
		"mv " + f + ".macmigrate.tmp " + f + "; chmod 600 " + f + "; fi"
}
