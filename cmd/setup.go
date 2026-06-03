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
		"It connects with a password the first time, so expect to be prompted.",
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

	// 1. Install the public key. This first connection authenticates with a
	// password (no -i: the key isn't authorized yet), so it must be interactive.
	fmt.Printf("Installing the public key on %s (you may be prompted for the destination password) …\n", dest)
	if err := migrate.InstallAuthorizedKey(ctx, migrate.SSH{TTY: true}, dest, pubLine); err != nil {
		return fail("installing the public key on %s: %v", dest, err)
	}

	// 2. Confirm key-only auth now works before relying on it.
	if err := migrate.VerifyKey(ctx, migrate.SSH{Identity: identity, BatchMode: true}, dest); err != nil {
		return fail("%v", err)
	}
	fmt.Println("✓ Key-based ssh works")

	// 3. Grant passwordless sudo. The connection is key-based now, but sudo
	// itself still prompts the first time, so keep it interactive.
	fmt.Printf("Configuring passwordless sudo on %s (you may be prompted for the destination sudo password) …\n", dest)
	if err := migrate.InstallSudoers(ctx, migrate.SSH{Identity: identity, TTY: true}, dest); err != nil {
		return fail("configuring passwordless sudo on %s: %v", dest, err)
	}
	keySSH := migrate.SSH{Identity: identity}
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
