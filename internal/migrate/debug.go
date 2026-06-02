package migrate

import (
	"fmt"
	"os"
)

// Debugf writes one diagnostic line to stderr in dark grey when enabled, and is
// a no-op otherwise. Diagnostics go to stderr so they can't be confused with
// (or captured as) regular output.
func Debugf(enabled bool, format string, a ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(os.Stderr, "\x1b[90m"+format+"\x1b[0m\n", a...)
}
