package migrate

import (
	"context"
	"fmt"

	"al.essio.dev/pkg/shellescape"
)

// SudoersPath is the drop-in file setup writes (and cleanup removes) to grant
// the destination login user passwordless sudo, which the parallel rsync and
// chown passes require.
const SudoersPath = "/etc/sudoers.d/macmigrate"

// authorizedKeysPath is the destination user's authorized_keys, expanded by the
// remote login shell ($HOME, not ~, so it also resolves under sudo-free ssh).
const authorizedKeysPath = "$HOME/.ssh/authorized_keys"

// InstallAuthorizedKey appends pubKey to the destination user's authorized_keys,
// creating ~/.ssh with the right modes first. This is the bootstrap step, so it
// runs over a password-authenticated, interactive connection (set s.TTY).
func InstallAuthorizedKey(ctx context.Context, s SSH, dest, pubKey string) error {
	return s.Run(ctx, dest, installAuthorizedKeyCmd(pubKey))
}

// installAuthorizedKeyCmd builds the idempotent remote command: ensure ~/.ssh
// (700) and authorized_keys (600) exist, then append the key only if an
// identical line isn't already present (grep -qxF), so re-running setup is safe.
func installAuthorizedKeyCmd(pubKey string) string {
	q := shellescape.Quote(pubKey)
	return "mkdir -p ~/.ssh && chmod 700 ~/.ssh" +
		" && touch " + authorizedKeysPath + " && chmod 600 " + authorizedKeysPath +
		" && { grep -qxF " + q + " " + authorizedKeysPath +
		" || printf '%s\\n' " + q + " >> " + authorizedKeysPath + "; }"
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

// InstallSudoers grants the destination login user passwordless sudo via a
// validated drop-in. `id -un` is expanded by the remote shell, so the rule names
// that machine's login user. sudo still prompts the first time, so this runs
// interactively (set s.TTY). visudo -cf rejects a malformed file rather than
// leaving a broken sudoers in place.
func InstallSudoers(ctx context.Context, s SSH, dest string) error {
	return s.Run(ctx, dest, installSudoersCmd())
}

func installSudoersCmd() string {
	p := shellescape.Quote(SudoersPath)
	return `echo "$(id -un) ALL=(ALL) NOPASSWD: ALL" | sudo tee ` + p + " >/dev/null" +
		" && sudo chmod 440 " + p +
		" && sudo visudo -cf " + p
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
