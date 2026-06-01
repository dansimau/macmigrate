package migrate

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"macmigrate/internal/xexec"
)

// RemoteHome resolves $HOME on the destination over ssh. It doubles as a
// connectivity preflight: a failure here means ssh/Remote Login isn't reachable.
func RemoteHome(ctx context.Context, dest string) (string, error) {
	out, err := sshCapture(ctx, dest, `printf %s "$HOME"`)
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(out)
	if home == "" {
		return "", fmt.Errorf("ssh %s: empty $HOME", dest)
	}
	return home, nil
}

// CanSudo reports whether the destination allows passwordless (non-interactive)
// sudo, which the /Applications and additional-directory transfers require.
func CanSudo(ctx context.Context, dest string) bool {
	// `sudo -n` never prompts: it exits 0 if no password is needed, non-zero
	// otherwise. RemoteHome has already confirmed ssh connectivity.
	return xexec.CommandContext(ctx, "ssh", dest, "sudo -n true").Run() == nil
}

// PrepareRemoteDir creates dir (and any missing parents) on the destination so
// its contents can be copied into it. It deliberately does not change the
// directory's own ownership: the entries inside are copied with their own
// attributes by rsync (running as root), and some roots such as /usr/local are
// SIP-protected and can't be chowned even by root. Needs passwordless sudo; a
// no-op in dry-run.
func PrepareRemoteDir(ctx context.Context, dest, dir string, dryRun bool) error {
	if dryRun {
		return nil
	}
	_, err := sshCapture(ctx, dest, "sudo -n mkdir -p "+shellescape.Quote(dir))
	return err
}

// remoteList returns the set of immediate entry names in dir on the destination.
func remoteList(ctx context.Context, dest, dir string) (map[string]bool, error) {
	out, err := sshCapture(ctx, dest, "cd "+shellescape.Quote(dir)+" && ls -1")
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

// sshCapture runs a single remote command and returns its stdout.
func sshCapture(ctx context.Context, dest, remoteCmd string) (string, error) {
	cmd := xexec.CommandContext(ctx, "ssh", dest, remoteCmd)
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
