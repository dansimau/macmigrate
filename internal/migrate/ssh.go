package migrate

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"github.com/dansimau/macmigrate/internal/xexec"
)

// sshArgv returns the local argv that launches ssh. After the sudo re-exec the
// whole process runs as root, but ssh must run in the invoking user's context so
// it uses that user's ssh-agent, keys and known_hosts (root has none of these).
// When asUser is set, ssh is launched via `sudo -E -u <asUser> ssh` — root can
// drop to any user without a password, and -E carries SSH_AUTH_SOCK through.
// Empty asUser uses ssh directly.
func sshArgv(asUser string) []string {
	if asUser == "" {
		return []string{"ssh"}
	}
	return []string{"sudo", "-E", "-u", asUser, "ssh"}
}

// RsyncRemoteShell is the value for rsync's -e (the command rsync uses to reach
// the destination), launching ssh as asUser the same way sshArgv does. It
// returns "" when asUser is empty, so the caller leaves rsync's default ssh.
func RsyncRemoteShell(asUser string) string {
	if asUser == "" {
		return ""
	}
	return strings.Join(sshArgv(asUser), " ")
}

// RemoteHome resolves $HOME on the destination over ssh. It doubles as a
// connectivity preflight: a failure here means ssh/Remote Login isn't reachable.
// ssh runs as asUser (see sshArgv).
func RemoteHome(ctx context.Context, asUser, dest string) (string, error) {
	out, err := sshCapture(ctx, asUser, dest, `printf %s "$HOME"`)
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
func RemoteUsername(ctx context.Context, asUser, dest string) (string, error) {
	out, err := sshCapture(ctx, asUser, dest, "id -un")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", fmt.Errorf("ssh %s: empty `id -un`", dest)
	}
	return name, nil
}

// RunChown runs a job's post-rsync ownership pass on the destination.
func RunChown(ctx context.Context, c *Chown) error {
	_, err := sshCapture(ctx, c.SSHUser, c.Dest, chownCmd(c))
	return err
}

// chownCmd builds the remote command for the ownership pass: files under
// c.Paths owned by the source user are chowned to the destination login user.
// rsync maps the source owner to the source username if that name happens to
// exist on the destination, otherwise to the source numeric uid — resolve
// which one to match before the find. chown -h changes symlinks themselves
// rather than their targets.
func chownCmd(c *Chown) string {
	src := shellescape.Quote(c.SrcUser)
	var b strings.Builder
	fmt.Fprintf(&b, "if id -u %s >/dev/null 2>&1; then U=%s; else U=%s; fi; ",
		src, src, shellescape.Quote(c.SrcUID))
	b.WriteString("sudo -n find")
	for _, p := range c.Paths {
		b.WriteString(" " + shellescape.Quote(p))
	}
	fmt.Fprintf(&b, ` -user "$U" -exec chown -h %s {} +`, shellescape.Quote(c.DstUser))
	return b.String()
}

// CanSudo reports whether the destination allows passwordless (non-interactive)
// sudo, which the /Applications and additional-directory transfers require.
func CanSudo(ctx context.Context, asUser, dest string) bool {
	// `sudo -n` never prompts: it exits 0 if no password is needed, non-zero
	// otherwise. RemoteHome has already confirmed ssh connectivity.
	argv := append(sshArgv(asUser), dest, "sudo -n true")
	return xexec.CommandContext(ctx, argv[0], argv[1:]...).Run() == nil
}

// PrepareRemoteDir creates dir (and any missing parents) on the destination so
// its contents can be copied into it. It deliberately does not change the
// directory's own ownership: the entries inside are copied with their own
// attributes by rsync (running as root), and some roots such as /usr/local are
// SIP-protected and can't be chowned even by root. Needs passwordless sudo; a
// no-op in dry-run.
func PrepareRemoteDir(ctx context.Context, asUser, dest, dir string, dryRun bool) error {
	if dryRun {
		return nil
	}
	_, err := sshCapture(ctx, asUser, dest, "sudo -n mkdir -p "+shellescape.Quote(dir))
	return err
}

// remoteList returns the set of immediate entry names in dir on the destination.
func remoteList(ctx context.Context, asUser, dest, dir string) (map[string]bool, error) {
	out, err := sshCapture(ctx, asUser, dest, "cd "+shellescape.Quote(dir)+" && ls -1")
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

// sshCapture runs a single remote command and returns its stdout. ssh runs as
// asUser (see sshArgv).
func sshCapture(ctx context.Context, asUser, dest, remoteCmd string) (string, error) {
	argv := append(sshArgv(asUser), dest, remoteCmd)
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
