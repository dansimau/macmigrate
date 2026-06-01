package xexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"golang.org/x/term"
)

// xexec.Cmd is a wrapper for exec.Cmd. It maintains the same API.
type Cmd struct {
	*exec.Cmd

	// when verbose is true, commands will be printed to os.Stderr before they
	// are executed.
	verbose bool
}

func cmdConstructor(c *Cmd) *Cmd {
	// Allow easy debugging for people by setting an environment variable
	// which will cause xexec to print any command before it executes it
	// (similar to bash -x).
	if os.Getenv("DEBUG") != "" {
		c.verbose = true
	}

	return c
}

// Command creates a new wrapped exec.Cmd.
func Command(cmd string, args ...string) *Cmd {
	return cmdConstructor(&Cmd{
		Cmd: exec.Command(cmd, args...),
	})
}

// Command creates a new wrapped exec.Cmd with context.
func CommandContext(ctx context.Context, cmd string, args ...string) *Cmd {
	return cmdConstructor(&Cmd{
		Cmd: exec.CommandContext(ctx, cmd, args...),
	})
}

// debugPrintCmd prints the command args to stderr.
func (c *Cmd) debugPrintCmd() {
	// Quote args before printing where necessary; this makes it easy for a
	// human to copy the line and paste it in their tty if they want to run
	// something manually
	quotedArgs := []string{}
	for _, arg := range c.Args {
		quotedArgs = append(quotedArgs, shellescape.Quote(arg))
	}

	if term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintf(os.Stderr, "\033[1;30m+ %s\033[0m\n", strings.Join(quotedArgs, " "))
	} else {
		fmt.Fprintf(os.Stderr, "+ %s\n", strings.Join(quotedArgs, " "))
	}
}

func (c *Cmd) Run() error {
	if c.verbose {
		c.debugPrintCmd()
	}
	return c.Cmd.Run()
}
