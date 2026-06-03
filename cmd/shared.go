package cmd

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// macmigrateKeyName is the distinct filename setup uses when it generates a key,
// so sync/cleanup can recognise "the macmigrate key" and auto-select it, and so
// it never collides with a user's own id_ed25519/id_rsa.
const macmigrateKeyName = "id_macmigrate"

// standardKeyNames are the default private-key filenames ssh tries on its own.
// setup reuses the first of these that exists (with a matching .pub) before
// falling back to generating macmigrateKeyName.
var standardKeyNames = []string{"id_ed25519", "id_ecdsa", "id_rsa", "id_dsa"}

// sshDir returns home/.ssh.
func sshDir(home string) string { return filepath.Join(home, ".ssh") }

// macmigrateKeyPath returns the path to the macmigrate-generated key in home.
func macmigrateKeyPath(home string) string {
	return filepath.Join(sshDir(home), macmigrateKeyName)
}

// keypairExists reports whether both the private key and its .pub exist.
func keypairExists(path string) bool {
	if !fileExists(path) || !fileExists(path+".pub") {
		return false
	}
	return true
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// resolveSyncIdentity picks the identity sync should connect with: an explicit
// -i wins; otherwise the macmigrate-generated key is used when present (ssh
// won't try it on its own — it's not a standard name); otherwise "" leaves ssh
// to its default key selection (the user's own id_ed25519 etc. or agent).
func resolveSyncIdentity(home, identityFlag string) (string, error) {
	if identityFlag != "" {
		if !fileExists(identityFlag) {
			return "", fmt.Errorf("identity %q does not exist", identityFlag)
		}
		return identityFlag, nil
	}
	if p := macmigrateKeyPath(home); keypairExists(p) {
		return p, nil
	}
	return "", nil
}

// findExistingIdentity is the shared resolution order for setup and cleanup: an
// explicit -i, else a previously generated macmigrate key, else the first
// standard keypair. The macmigrate key outranks standard keys so that setup,
// sync and cleanup all agree on the same key when a generated one is lying
// around — sync auto-selects it, so setup must install that one, and cleanup
// must remove it. "" means none found.
func findExistingIdentity(home, identityFlag string) (string, error) {
	if identityFlag != "" {
		if !keypairExists(identityFlag) {
			return "", fmt.Errorf("identity %q (or its .pub) does not exist", identityFlag)
		}
		return identityFlag, nil
	}
	if p := macmigrateKeyPath(home); keypairExists(p) {
		return p, nil
	}
	for _, name := range standardKeyNames {
		if p := filepath.Join(sshDir(home), name); keypairExists(p) {
			return p, nil
		}
	}
	return "", nil
}

// resolveSetupIdentity decides which key setup installs: an existing key per
// findExistingIdentity, else (unless skipKeygen) a freshly generated macmigrate
// key. Re-runs find the previously generated key instead of invoking ssh-keygen
// over it. generated reports whether the returned path was just created.
func resolveSetupIdentity(home, identityFlag string, skipKeygen bool) (path string, generated bool, err error) {
	if p, err := findExistingIdentity(home, identityFlag); err != nil || p != "" {
		return p, false, err
	}
	if skipKeygen {
		return "", false, fmt.Errorf("no SSH key found in %s and --skip-keygen is set; "+
			"create a key, pass -i <path>, or drop --skip-keygen to generate one", sshDir(home))
	}
	p := macmigrateKeyPath(home)
	if err := generateKey(home, p); err != nil {
		return "", false, err
	}
	return p, true, nil
}

// readPubKey returns the full public-key line and its base64 body (the second
// whitespace-separated field) for the keypair at identityPath. cleanup matches
// on the body so a differing trailing comment can't hide the line.
func readPubKey(identityPath string) (line, body string, err error) {
	raw, err := os.ReadFile(identityPath + ".pub")
	if err != nil {
		return "", "", fmt.Errorf("reading public key %s.pub: %w", identityPath, err)
	}
	line = strings.TrimSpace(string(raw))
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", fmt.Errorf("malformed public key in %s.pub", identityPath)
	}
	return line, fields[1], nil
}

// currentUsername returns the name of the user who invoked macmigrate.
func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// uidForUser returns the numeric uid for username, or an error if unknown.
func uidForUser(username string) (string, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return "", err
	}
	return u.Uid, nil
}
