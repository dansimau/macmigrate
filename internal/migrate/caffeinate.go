package migrate

import (
	"os/exec"
	"syscall"
)

// Caffeinate keeps both Macs awake for the duration of the migration: a local
// `caffeinate -s` and, when dest is non-empty, a remote one over ssh (reached
// via ssh, like every other connection). It returns a stop function to call
// when the migration ends — typically deferred.
//
// Each child is started in its own process group so stop can signal the whole
// group: for the remote one that kills the local `sudo … ssh` wrapper as well
// as ssh, and closing the ssh channel makes sshd HUP the remote caffeinate.
// Caffeinate is best-effort — a child that fails to start is skipped silently,
// since sleep prevention is a convenience, not a correctness requirement.
func Caffeinate(ssh SSH, dest string) (stop func()) {
	var procs []*exec.Cmd
	start := func(name string, arg ...string) {
		cmd := exec.Command(name, arg...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// Don't let caffeinate inherit our stdio and scribble on the live region.
		cmd.Stdout, cmd.Stderr = nil, nil
		if err := cmd.Start(); err == nil {
			procs = append(procs, cmd)
		}
	}

	start("caffeinate", "-s")
	if dest != "" {
		argv := append(ssh.prefix(), dest, "caffeinate -s")
		start(argv[0], argv[1:]...)
	}

	return func() {
		for _, cmd := range procs {
			if cmd.Process == nil {
				continue
			}
			// Negative pid signals the whole process group.
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}
}
