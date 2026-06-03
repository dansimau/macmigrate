package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/dansimau/macmigrate/internal/migrate"
	"github.com/spf13/cobra"
)

var cleanupIdentity string

var cleanupCmd = &cobra.Command{
	Use:   "cleanup <dest>",
	Short: "Undo setup on the destination (remove sudoers + the installed key)",
	Long: "cleanup reverses `macmigrate setup` on the destination:\n" +
		"  1. Removes " + migrate.SudoersPath + " (the passwordless-sudo grant).\n" +
		"  2. Removes the macmigrate public key from the destination's authorized_keys.\n\n" +
		"The locally generated key is left in place. Each step is best-effort; a\n" +
		"missing item is not treated as an error.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCleanup(cmd.Context(), args[0])
	},
}

func init() {
	cleanupCmd.Flags().StringVarP(&cleanupIdentity, "identity", "i", "",
		"the key installed by setup (defaults to ~/.ssh/"+macmigrateKeyName+", else an existing standard key)")
	rootCmd.AddCommand(cleanupCmd)
}

func runCleanup(ctx context.Context, dest string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fail("locating your home directory: %v", err)
	}
	// Resolve with the same order as setup (macmigrate key, then standard keys)
	// so cleanup removes whichever key setup actually installed.
	identity, err := findExistingIdentity(home, cleanupIdentity)
	if err != nil {
		return fail("%v", err)
	}

	// TTY so a `sudo` that's no longer passwordless can still prompt.
	ssh := migrate.SSH{Identity: identity, TTY: true}
	failed := false

	fmt.Printf("Removing %s on %s …\n", migrate.SudoersPath, dest)
	if err := migrate.RemoveSudoers(ctx, ssh, dest); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠ could not remove %s: %v\n", migrate.SudoersPath, err)
		failed = true
	} else {
		fmt.Println("  ✓ sudoers grant removed")
	}

	if identity == "" {
		fmt.Println("Skipping authorized_keys cleanup: no local SSH key found and no -i given.")
		fmt.Println("  Pass -i <key> to remove a specific public key from the destination.")
	} else if _, body, err := readPubKey(identity); err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠ could not read %s.pub: %v\n", identity, err)
		failed = true
	} else {
		fmt.Printf("Removing the public key from authorized_keys on %s …\n", dest)
		if err := migrate.RemoveAuthorizedKey(ctx, ssh, dest, body); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠ could not update authorized_keys: %v\n", err)
			failed = true
		} else {
			fmt.Println("  ✓ public key removed")
		}
	}

	if failed {
		return &exitErr{code: 1}
	}
	fmt.Printf("\nDone. The local key %s was left in place; delete it manually if you no longer need it.\n",
		macmigrateKeyPath(home))
	return nil
}
