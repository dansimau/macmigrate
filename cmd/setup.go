package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/dansimau/macmigrate/internal/migrate"
	"github.com/dansimau/macmigrate/internal/xexec"
	"github.com/spf13/cobra"
)

var (
	setupSkipKeygen bool
	setupIdentity   string
)

var setupCmd = &cobra.Command{
	Use:   "setup <dest>",
	Short: "Provision key-based ssh and passwordless sudo on the destination",
	Long: "setup prepares a destination Mac so `macmigrate sync` can run unattended:\n" +
		"  1. Uses an existing SSH key (or generates ~/.ssh/" + macmigrateKeyName + ").\n" +
		"  2. Installs the public key in the destination's authorized_keys.\n" +
		"  3. Grants the destination login user passwordless sudo (" + migrate.SudoersPath + ").\n\n" +
		"Steps 2 and 3 share a single ssh session (ControlMaster), so you authenticate\n" +
		"to ssh once; the remote sudo may additionally ask for its password. Re-running\n" +
		"setup is safe (and prompt-free when there is nothing left to do).",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSetup(cmd.Context(), args[0])
	},
}

func init() {
	setupCmd.Flags().BoolVar(&setupSkipKeygen, "skip-keygen", false,
		"don't generate a key; require an existing key (use -i to pick one)")
	setupCmd.Flags().StringVarP(&setupIdentity, "identity", "i", "",
		"SSH key to install (defaults to an existing key, else a generated one)")
	rootCmd.AddCommand(setupCmd)
}

func runSetup(ctx context.Context, dest string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fail("locating your home directory: %v", err)
	}

	identity, generated, err := resolveSetupIdentity(home, setupIdentity, setupSkipKeygen)
	if err != nil {
		return fail("%v", err)
	}
	if generated {
		fmt.Printf("Generated a new SSH key: %s\n", identity)
	} else {
		fmt.Printf("Using existing SSH key: %s\n", identity)
	}

	pubLine, _, err := readPubKey(identity)
	if err != nil {
		return fail("%v", err)
	}

	// Idempotent fast path: if the key is already authorized and sudo is already
	// passwordless, there is nothing to do — and nothing to prompt for. A key
	// generated moments ago can't be authorized yet, so skip the probe.
	keySSH := migrate.SSH{Identity: identity, BatchMode: true}
	if !generated && migrate.VerifyKey(ctx, keySSH, dest) == nil && keySSH.CanSudo(ctx, dest) {
		fmt.Printf("✓ Already set up: key auth and passwordless sudo both work on %s\n", dest)
		return nil
	}

	// One master connection for the whole provisioning phase: ssh authenticates
	// once (prompting on this terminal as needed) and the commands below
	// multiplex over it. The key is offered first, so a re-run where it is
	// already authorized connects without any ssh prompt.
	fmt.Printf("Connecting to %s …\n", dest)
	master, err := migrate.OpenMaster(ctx, migrate.SSH{Identity: identity}, dest)
	if err != nil {
		return fail("%v", err)
	}
	defer master.Close()

	// TTY so the remote sudo can prompt for its password on this terminal.
	mux := master.SSH()
	mux.TTY = true
	fmt.Printf("Installing the public key and passwordless sudo on %s …\n", dest)
	if err := migrate.Provision(ctx, mux, dest, pubLine); err != nil {
		return fail("%v", err)
	}

	// Verify both halves with fresh non-interactive connections (deliberately
	// not over the master, which would succeed regardless of the key).
	if err := migrate.VerifyKey(ctx, keySSH, dest); err != nil {
		return fail("%v", err)
	}
	fmt.Println("✓ Key-based ssh works")
	if !keySSH.CanSudo(ctx, dest) {
		return fail("passwordless sudo still isn't working on %s after writing %s", dest, migrate.SudoersPath)
	}
	fmt.Println("✓ Passwordless sudo configured")

	fmt.Printf("\nDone. Run:  macmigrate sync %s\n", dest)
	if generated {
		fmt.Printf("(sync auto-detects %s; no extra flags needed.)\n", identity)
	}
	fmt.Printf("Undo on the destination with:  macmigrate cleanup %s\n", dest)
	return nil
}

// generateKey creates an ed25519 keypair at path (no passphrase), ensuring
// ~/.ssh exists with 0700 first. The macmigrate@<host> comment makes the key's
// origin obvious in authorized_keys.
func generateKey(home, path string) error {
	if err := os.MkdirAll(sshDir(home), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", sshDir(home), err)
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "mac"
	}
	cmd := xexec.Command("ssh-keygen", "-t", "ed25519", "-f", path, "-N", "", "-C", "macmigrate@"+host)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("generating SSH key at %s: %w", path, err)
	}
	return nil
}
