// Command macmigrate copies a user's home directory and third-party
// applications from one Mac to another over ssh, running many rsync transfers
// in parallel to saturate a fast direct link. It exposes three subcommands:
// setup (provision key-based ssh + passwordless sudo on the destination), sync
// (the migration itself), and cleanup (undo what setup did on the destination).
package main

import (
	"os"

	"github.com/dansimau/macmigrate/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
