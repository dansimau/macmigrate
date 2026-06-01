package migrate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// FullDiskAccess reports whether this process can read macOS TCC-protected
// paths (Full Disk Access). It probes the user's TCC database, which is
// readable only with FDA — so a successful open means FDA is granted, and a
// permission error means it isn't. ok is false when the probe is inconclusive
// (the file is missing or fails for another reason), so callers can skip the
// warning rather than cry wolf.
//
// FDA is a property of the *responsible* process — typically the terminal that
// launched macmigrate — and is inherited across the sudo re-exec, so probing
// from here reflects what rsync (running in this process as root) can read.
func FullDiskAccess(home string) (granted, ok bool) {
	probe := filepath.Join(home, "Library/Application Support/com.apple.TCC/TCC.db")
	f, err := os.Open(probe)
	if err == nil {
		f.Close()
		return true, true
	}
	if errors.Is(err, fs.ErrPermission) {
		return false, true
	}
	return false, false
}
