// Package cmd holds the macmigrate command tree (setup / sync / cleanup) built
// on Cobra. main() calls Execute, which maps command errors to process exit
// codes — including the migration's partial/failed (1) and interrupted (130)
// conventions that scripted runs rely on.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Flags shared by every subcommand live on the root as persistent flags, so a
// user moving from one step to the next only edits the trailing subcommand:
//   macmigrate --dest 1.2.3.4 setup   →   macmigrate --dest 1.2.3.4 sync
var (
	debug    bool   // print diagnostics to stderr
	destHost string // destination IP or hostname (required)
	destUser string // destination login user; "" => ssh's default (your username)
	identity string // SSH key; "" => auto-detect (macmigrate key, else ssh defaults)
)

var rootCmd = &cobra.Command{
	Use:   "macmigrate",
	Short: "Copy a Mac's home directory and apps to another Mac over ssh",
	Long: "macmigrate copies a user's home directory and third-party applications\n" +
		"from one Mac to another over ssh, running many rsync transfers in parallel.\n\n" +
		"Typical flow:\n" +
		"  macmigrate --dest <host> setup     # one-time: key-based ssh + passwordless sudo\n" +
		"  macmigrate --dest <host> sync      # the migration (re-runs itself under sudo)\n" +
		"  macmigrate --dest <host> cleanup   # undo setup on the destination\n\n" +
		"Add --dest-user when the destination login user differs from your local one.",
	SilenceUsage:  true, // don't dump usage on a runtime failure
	SilenceErrors: true, // Execute prints errors itself, with the macmigrate: prefix
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&debug, "debug", false,
		"print diagnostics (app selection, full rsync commands) to stderr")
	rootCmd.PersistentFlags().StringVar(&destHost, "dest", "",
		"destination Mac's IP address or hostname (required)")
	rootCmd.PersistentFlags().StringVar(&destUser, "dest-user", "",
		"login user on the destination (default: your local username)")
	rootCmd.PersistentFlags().StringVarP(&identity, "identity", "i", "",
		"SSH key to use (defaults to ~/.ssh/"+macmigrateKeyName+" if present)")
}

// destAddr builds the ssh destination ([user@]host) from the persistent
// --dest-user / --dest flags. --dest is required, but validated here rather
// than via MarkPersistentFlagRequired, which would also fail
// `macmigrate help <cmd>` and the completion command.
func destAddr() (string, error) {
	if destHost == "" {
		return "", errors.New(`required flag(s) "dest" not set`)
	}
	if destUser != "" {
		return destUser + "@" + destHost, nil
	}
	return destHost, nil
}

// destFlags reconstructs the destination flags as typed, for "run this next"
// hints — so a suggested command can be pasted (or recalled) as-is.
func destFlags() string {
	s := "--dest " + destHost
	if destUser != "" {
		s += " --dest-user " + destUser
	}
	return s
}

// exitErr carries a specific process exit code out of a command. An empty msg
// means the command already printed everything it needs to (e.g. sync's report).
type exitErr struct {
	code int
	msg  string
}

func (e *exitErr) Error() string { return e.msg }

// fail returns an exitErr with the conventional fatal code (2) and a message
// Execute prints with the macmigrate: prefix.
func fail(format string, args ...any) error {
	return &exitErr{code: 2, msg: fmt.Sprintf(format, args...)}
}

// Execute runs the command tree and returns the process exit code.
func Execute() int {
	err := rootCmd.ExecuteContext(context.Background())
	if err == nil {
		return 0
	}
	var ee *exitErr
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, "macmigrate: "+ee.msg)
		}
		return ee.code
	}
	// Cobra flag/argument errors land here (SilenceErrors is on).
	fmt.Fprintln(os.Stderr, "macmigrate: "+err.Error())
	return 2
}
