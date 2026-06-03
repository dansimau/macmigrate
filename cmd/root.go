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

// debug is bound to the persistent --debug flag and read by every subcommand.
var debug bool

var rootCmd = &cobra.Command{
	Use:   "macmigrate",
	Short: "Copy a Mac's home directory and apps to another Mac over ssh",
	Long: "macmigrate copies a user's home directory and third-party applications\n" +
		"from one Mac to another over ssh, running many rsync transfers in parallel.\n\n" +
		"Typical flow:\n" +
		"  macmigrate setup <dest>     # one-time: key-based ssh + passwordless sudo\n" +
		"  macmigrate sync <dest>      # the migration (re-runs itself under sudo)\n" +
		"  macmigrate cleanup <dest>   # undo setup on the destination",
	SilenceUsage:  true, // don't dump usage on a runtime failure
	SilenceErrors: true, // Execute prints errors itself, with the macmigrate: prefix
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&debug, "debug", false,
		"print diagnostics (app selection, full rsync commands) to stderr")
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
