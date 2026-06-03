package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

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

// authorizedKeyTag is appended (as part of the comment field, which sshd
// ignores) to every authorized_keys line setup installs. cleanup removes only
// tagged lines, so a key that was authorized before macmigrate ever ran — the
// user's own key, installed by the user — is never removed.
const authorizedKeyTag = "# added-by-macmigrate"

// MasterSession is an ssh ControlMaster connection that setup's commands
// multiplex over, so the user authenticates exactly once for the whole
// provisioning phase. ssh owns all prompting (password, host key) directly on
// the terminal — the password never passes through this process. Setup-only:
// sync and cleanup open ordinary connections.
type MasterSession struct {
	sock string
	dir  string
	dest string
	done chan error
}

// OpenMaster starts an interactive master connection to dest (authenticating as
// needed on the caller's terminal) and waits until its control socket accepts
// multiplexed sessions. Close it when the provisioning phase is over.
func OpenMaster(ctx context.Context, s SSH, dest string) (*MasterSession, error) {
	dir, err := os.MkdirTemp("", "macmigrate-ssh")
	if err != nil {
		return nil, fmt.Errorf("creating control-socket directory: %w", err)
	}
	sock := filepath.Join(dir, "ctl")

	s.ControlPath = sock
	argv := append(s.prefix(), "-M", "-N", dest)
	cmd := xexec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("starting ssh master to %s: %w", dest, err)
	}
	m := &MasterSession{sock: sock, dir: dir, dest: dest, done: make(chan error, 1)}
	go func() { m.done <- cmd.Wait() }()

	// The master is ready when -O check succeeds. No fixed deadline: the user
	// may be typing a password; a failed/cancelled master ends the wait instead.
	for {
		select {
		case err := <-m.done:
			os.RemoveAll(dir)
			if err == nil {
				err = fmt.Errorf("connection closed")
			}
			return nil, fmt.Errorf("ssh master to %s exited before it was ready: %w", dest, err)
		case <-ctx.Done():
			os.RemoveAll(dir)
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
			if m.check(ctx) {
				return m, nil
			}
		}
	}
}

// check reports whether the control socket is accepting connections.
func (m *MasterSession) check(ctx context.Context) bool {
	cmd := xexec.CommandContext(ctx, "ssh", "-o", "ControlPath="+m.sock, "-O", "check", m.dest)
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run() == nil
}

// SSH returns the connection settings that reuse this master: commands run with
// it attach to the already-authenticated session and never prompt for auth.
func (m *MasterSession) SSH() SSH { return SSH{ControlPath: m.sock} }

// Close shuts the master down and removes the control socket.
func (m *MasterSession) Close() {
	cmd := xexec.Command("ssh", "-o", "ControlPath="+m.sock, "-O", "exit", m.dest)
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Run()
	<-m.done
	os.RemoveAll(m.dir)
}

// Provision installs the public key (line pubKey, base64 body keyBody) in the
// destination's authorized_keys and writes the passwordless-sudo drop-in as one
// remote command. Run it over a MasterSession connection with TTY set: the
// session is already authenticated, and the pty lets the remote sudo prompt
// for its password on the user's terminal.
func Provision(ctx context.Context, s SSH, dest, pubKey, keyBody string) error {
	if err := s.Run(ctx, dest, provisionCmd(pubKey, keyBody)); err != nil {
		return fmt.Errorf("provisioning %s: %w", dest, err)
	}
	return nil
}

// provisionCmd chains the key install and the sudoers install into one remote
// command, so both happen in the single authenticated session.
func provisionCmd(pubKey, keyBody string) string {
	return installAuthorizedKeyCmd(pubKey, keyBody) + " && " + installSudoersCmd()
}

// installAuthorizedKeyCmd builds the idempotent key-install command: ensure
// ~/.ssh (700) and authorized_keys (600) exist and that the file ends in a
// newline (an unterminated last line would swallow the appended key into the
// previous entry's comment), then append the key — tagged so cleanup can tell
// it apart from entries it must not touch — only if its base64 body isn't
// already present on an active line. Matching on the body (not the whole
// line) makes re-running setup safe AND leaves a pre-existing entry for the
// user's own key alone even when its comment differs from the local .pub's:
// the key is already authorized, so nothing is added and nothing gets tagged
// for later removal. Comment lines (sshd skips lines whose first non-blank
// character is #) are filtered out first, so a commented-out copy of the key
// can't masquerade as an active one and suppress the install.
func installAuthorizedKeyCmd(pubKey, keyBody string) string {
	line := shellescape.Quote(pubKey + " " + authorizedKeyTag)
	body := shellescape.Quote(keyBody)
	f := authorizedKeysPath
	return "mkdir -p ~/.ssh && chmod 700 ~/.ssh" +
		" && touch " + f + " && chmod 600 " + f +
		" && { [ ! -s " + f + ` ] || [ -z "$(tail -c1 ` + f + `)" ] || echo >> ` + f + "; }" +
		" && { grep -v -e '^[[:space:]]*#' " + f + " | grep -qF " + body +
		" || printf '%s\\n' " + line + " >> " + f + "; }"
}

// installSudoersCmd grants the destination login user passwordless sudo via a
// validated drop-in. `id -un` is expanded by the remote shell, so the rule
// names that machine's login user. sudo prompts on the session's pty if it
// needs a password; its timestamp then covers the follow-up commands. visudo
// -cf rejects a malformed file, which is removed rather than left breaking sudo.
func installSudoersCmd() string {
	p := SudoersPath
	return `echo "$(id -un) ALL=(ALL) NOPASSWD: ALL" | sudo tee ` + p + " >/dev/null" +
		" && sudo chmod 440 " + p +
		" && { sudo visudo -cf " + p + " || { sudo rm -f " + p + "; exit 1; }; }"
}

// VerifyKey confirms the destination accepts key-only auth, failing fast instead
// of letting a misconfigured key fall through to a password prompt mid-migration.
// The caller passes an SSH value carrying the identity and BatchMode=yes — and
// no ControlPath, so it genuinely re-authenticates rather than riding the master.
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

// RemoveAuthorizedKey strips the authorized_keys line(s) setup added for this
// key — those carrying both keyBody (the key's base64 body) and the macmigrate
// tag — from the destination user's authorized_keys. An entry for the same key
// that predates setup has no tag and survives, so cleanup can never revoke
// access the user had independently of macmigrate. It is a no-op when the file
// is absent and needs no sudo — it's the user's own file.
func RemoveAuthorizedKey(ctx context.Context, s SSH, dest, keyBody string) error {
	return s.Run(ctx, dest, removeAuthorizedKeyCmd(keyBody))
}

// removeAuthorizedKeyCmd rewrites authorized_keys without the line(s) matching
// "<body>…<tag>". Both sides of the pattern are literal under grep's default
// BRE — the base64 alphabet ([A-Za-z0-9+/=]; + is only special in ERE) and the
// tag contain no BRE metacharacters. grep -v can exit 1 when it selects
// nothing (e.g. the tagged key was the only line), so `|| true` keeps that
// from aborting the rewrite; the tmp file is then moved back, preserving 600.
func removeAuthorizedKeyCmd(keyBody string) string {
	f := authorizedKeysPath
	q := shellescape.Quote(keyBody + ".*" + authorizedKeyTag)
	return "if [ -f " + f + " ]; then " +
		"grep -v -e " + q + " " + f + " > " + f + ".macmigrate.tmp || true; " +
		"mv " + f + ".macmigrate.tmp " + f + "; chmod 600 " + f + "; fi"
}
