package migrate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"github.com/dansimau/macmigrate/internal/xexec"
)

// SSH describes how to reach the destination over ssh. After the sudo re-exec
// the migration runs as root, but ssh must run in the invoking user's context so
// it uses that user's ssh-agent, keys and known_hosts (root has none of these) —
// hence User, which launches ssh via `sudo -E -u <User> ssh`.
type SSH struct {
	User        string // run ssh as this local user via `sudo -E -u`; "" => plain ssh
	Identity    string // -i <path> (+ IdentitiesOnly); "" => ssh's default key selection
	TTY         bool   // -t: allocate a pty so remote sudo/password prompts work
	BatchMode   bool   // -o BatchMode=yes: never prompt, fail instead (key-only checks)
	ControlPath string // -o ControlPath=<sock>: multiplex over a MasterSession (setup only)
}

// prefix returns the local argv that launches ssh, up to but not including the
// destination and remote command. When User is set, ssh is launched via
// `sudo -E -u <User> ssh` — root can drop to any user without a password, and -E
// carries SSH_AUTH_SOCK through to that user's ssh-agent.
func (s SSH) prefix() []string {
	var argv []string
	if s.User == "" {
		argv = append(argv, "ssh")
	} else {
		argv = append(argv, "sudo", "-E", "-u", s.User, "ssh")
	}
	if s.Identity != "" {
		// IdentitiesOnly stops ssh from also offering agent/default keys, so the
		// connection genuinely exercises Identity (matters for setup's verify).
		argv = append(argv, "-i", s.Identity, "-o", "IdentitiesOnly=yes")
	}
	if s.BatchMode {
		argv = append(argv, "-o", "BatchMode=yes")
	}
	if s.ControlPath != "" {
		argv = append(argv, "-o", "ControlPath="+s.ControlPath)
	}
	if s.TTY {
		argv = append(argv, "-t")
	}
	return argv
}

// RsyncRemoteShell is the value for rsync's -e (the command rsync uses to reach
// the destination), launching ssh as User the same way prefix does — but never
// with a pty, since rsync drives many non-interactive connections. It returns ""
// when neither User nor Identity is set, so the caller leaves rsync's default ssh.
func (s SSH) RsyncRemoteShell() string {
	if s.User == "" && s.Identity == "" {
		return ""
	}
	return strings.Join(SSH{User: s.User, Identity: s.Identity}.prefix(), " ")
}

// RemoteHome resolves $HOME on the destination over ssh. It doubles as a
// connectivity preflight: a failure here means ssh/Remote Login isn't reachable.
func (s SSH) RemoteHome(ctx context.Context, dest string) (string, error) {
	out, err := s.Capture(ctx, dest, `printf %s "$HOME"`)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(out)
	if home == "" {
		return "", fmt.Errorf("ssh %s: empty $HOME", dest)
	}
	return home, nil
}

// RemoteUsername returns the destination login user's short name.
func (s SSH) RemoteUsername(ctx context.Context, dest string) (string, error) {
	out, err := s.Capture(ctx, dest, "id -un")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", fmt.Errorf("ssh %s: empty `id -un`", dest)
	}
	return name, nil
}

// ChownUID resolves, once up front, the uid that the source user's files end
// up owned by on the destination: rsync maps the owner by name when the source
// username happens to exist there, and falls back to the source's numeric uid
// otherwise.
func (s SSH) ChownUID(ctx context.Context, dest, srcUser, srcUID string) (string, error) {
	cmd := fmt.Sprintf("id -u %s 2>/dev/null || echo %s",
		shellescape.Quote(srcUser), shellescape.Quote(srcUID))
	out, err := s.Capture(ctx, dest, cmd)
	if err != nil {
		return "", err
	}
	uid := strings.TrimSpace(out)
	if uid == "" {
		return "", fmt.Errorf("ssh %s: empty uid for %s", dest, srcUser)
	}
	return uid, nil
}

// RunChown runs a job's post-rsync ownership pass on the destination.
func (s SSH) RunChown(ctx context.Context, dest string, c *Chown) error {
	_, err := s.Capture(ctx, dest, chownCmd(c))
	return err
}

// chownCmd builds the remote command for the ownership pass: files under
// c.Path owned by c.UID are chowned to the destination login user — `id -un`
// is expanded by the login shell before sudo elevates the find. chown -h
// changes symlinks themselves rather than their targets.
func chownCmd(c *Chown) string {
	depth := ""
	if !c.Recurse {
		depth = " -maxdepth 1"
	}
	return "sudo -n find " + shellescape.Quote(c.Path) + depth +
		" -user " + shellescape.Quote(c.UID) + ` -exec chown -h "$(id -un)" {} +`
}

// CanSudo reports whether the destination allows passwordless (non-interactive)
// sudo, which the /Applications and additional-directory transfers require.
func (s SSH) CanSudo(ctx context.Context, dest string) bool {
	// `sudo -n` never prompts: it exits 0 if no password is needed, non-zero
	// otherwise. RemoteHome has already confirmed ssh connectivity.
	argv := append(s.prefix(), dest, "sudo -n true")
	return xexec.CommandContext(ctx, argv[0], argv[1:]...).Run() == nil
}

// PrepareRemoteDir creates dir (and any missing parents) on the destination so
// its contents can be copied into it. It deliberately does not change the
// directory's own ownership: the entries inside are copied with their own
// attributes by rsync (running as root), and some roots such as /usr/local are
// SIP-protected and can't be chowned even by root. Needs passwordless sudo; a
// no-op in dry-run.
func (s SSH) PrepareRemoteDir(ctx context.Context, dest, dir string, dryRun bool) error {
	if dryRun {
		return nil
	}
	_, err := s.Capture(ctx, dest, "sudo -n mkdir -p "+shellescape.Quote(dir))
	return err
}

// remoteList returns the set of immediate entry names in dir on the destination.
func (s SSH) remoteList(ctx context.Context, dest, dir string) (map[string]bool, error) {
	out, err := s.Capture(ctx, dest, "cd "+shellescape.Quote(dir)+" && ls -1")
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set, nil
}

// Capture runs a single remote command and returns its stdout. Use it for
// non-interactive commands whose output you need; pair it with TTY=false so the
// pty doesn't fold stderr into stdout.
func (s SSH) Capture(ctx context.Context, dest, remoteCmd string) (string, error) {
	argv := append(s.prefix(), dest, remoteCmd)
	cmd := xexec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("ssh %s: %w: %s", dest, err, msg)
		}
		return "", fmt.Errorf("ssh %s: %w", dest, err)
	}
	return stdout.String(), nil
}

// Run executes a single remote command with the process's own stdio wired
// through, so password and remote-sudo prompts reach the user's terminal. Used
// by setup/cleanup, which are interactive by nature (pair with TTY=true).
func (s SSH) Run(ctx context.Context, dest, remoteCmd string) error {
	argv := append(s.prefix(), dest, remoteCmd)
	cmd := xexec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
