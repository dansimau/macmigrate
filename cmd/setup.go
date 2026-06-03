package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/dansimau/macmigrate/internal/migrate"
	"github.com/dansimau/macmigrate/internal/xexec"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
		"Steps 2 and 3 happen in a single ssh session: the destination password is\n" +
		"asked for once, up front. Re-running setup is safe (and prompt-free when\n" +
		"there is nothing left to do).",
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

	// One password, one session: ssh auth (if the key isn't authorized yet) and
	// the remote sudo both consume the password read here — see migrate.Provision.
	fmt.Fprintf(os.Stderr, "Password for %s (asked once; used for ssh and sudo): ", dest)
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return fail("reading password (setup needs an interactive terminal): %v", err)
	}

	fmt.Printf("Installing the public key and passwordless sudo on %s …\n", dest)
	if err := migrate.Provision(ctx, migrate.SSH{Identity: identity}, dest, pubLine, string(pw)); err != nil {
		return fail("%v", err)
	}

	// Verify both halves with non-interactive connections before declaring success.
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
