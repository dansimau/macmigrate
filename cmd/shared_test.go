package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// writeKeypair creates a fake private key and its .pub at home/.ssh/<name>.
func writeKeypair(t *testing.T, home, name, pubBody string) string {
	t.Helper()
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	pub := "ssh-ed25519 " + pubBody + " macmigrate@test\n"
	if err := os.WriteFile(path+".pub", []byte(pub), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveSyncIdentity(t *testing.T) {
	t.Run("none present returns empty (ssh default)", func(t *testing.T) {
		home := t.TempDir()
		got, err := resolveSyncIdentity(home, "")
		if err != nil || got != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", got, err)
		}
	})

	t.Run("auto-detects the macmigrate key", func(t *testing.T) {
		home := t.TempDir()
		want := writeKeypair(t, home, macmigrateKeyName, "AAAAmm")
		got, err := resolveSyncIdentity(home, "")
		if err != nil || got != want {
			t.Fatalf("got (%q, %v), want (%q, nil)", got, err, want)
		}
	})

	t.Run("explicit -i wins", func(t *testing.T) {
		home := t.TempDir()
		writeKeypair(t, home, macmigrateKeyName, "AAAAmm")
		flag := writeKeypair(t, home, "id_custom", "AAAAcc")
		got, err := resolveSyncIdentity(home, flag)
		if err != nil || got != flag {
			t.Fatalf("got (%q, %v), want (%q, nil)", got, err, flag)
		}
	})

	t.Run("missing -i errors", func(t *testing.T) {
		home := t.TempDir()
		if _, err := resolveSyncIdentity(home, filepath.Join(home, "nope")); err == nil {
			t.Fatal("expected error for missing identity")
		}
	})
}

func TestResolveSetupIdentity(t *testing.T) {
	t.Run("reuses an existing standard key", func(t *testing.T) {
		home := t.TempDir()
		want := writeKeypair(t, home, "id_ed25519", "AAAAstd")
		got, generated, err := resolveSetupIdentity(home, "", false)
		if err != nil || generated || got != want {
			t.Fatalf("got (%q, gen=%v, %v), want (%q, gen=false, nil)", got, generated, err, want)
		}
	})

	t.Run("reuses a previously generated macmigrate key without regenerating", func(t *testing.T) {
		home := t.TempDir()
		want := writeKeypair(t, home, macmigrateKeyName, "AAAAmm")
		got, generated, err := resolveSetupIdentity(home, "", false)
		if err != nil || generated || got != want {
			t.Fatalf("got (%q, gen=%v, %v), want (%q, gen=false, nil)", got, generated, err, want)
		}
	})

	t.Run("prefers the macmigrate key over standard keys (matches sync)", func(t *testing.T) {
		home := t.TempDir()
		writeKeypair(t, home, "id_ed25519", "AAAAstd")
		want := writeKeypair(t, home, macmigrateKeyName, "AAAAmm")
		got, _, err := resolveSetupIdentity(home, "", false)
		if err != nil || got != want {
			t.Fatalf("got (%q, %v), want (%q, nil)", got, err, want)
		}
		// sync must pick the same key setup installed.
		syncGot, err := resolveSyncIdentity(home, "")
		if err != nil || syncGot != want {
			t.Fatalf("sync got (%q, %v), want (%q, nil)", syncGot, err, want)
		}
	})

	t.Run("skip-keygen with no key errors", func(t *testing.T) {
		home := t.TempDir()
		if _, _, err := resolveSetupIdentity(home, "", true); err == nil {
			t.Fatal("expected error with --skip-keygen and no key")
		}
	})

	t.Run("explicit -i missing pub errors", func(t *testing.T) {
		home := t.TempDir()
		// private key only, no .pub
		dir := filepath.Join(home, ".ssh")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		priv := filepath.Join(dir, "lonely")
		if err := os.WriteFile(priv, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := resolveSetupIdentity(home, priv, false); err == nil {
			t.Fatal("expected error when .pub is missing")
		}
	})

	t.Run("generates the macmigrate key when none exists", func(t *testing.T) {
		if _, err := exec.LookPath("ssh-keygen"); err != nil {
			t.Skip("ssh-keygen not available")
		}
		home := t.TempDir()
		got, generated, err := resolveSetupIdentity(home, "", false)
		if err != nil {
			t.Fatalf("resolveSetupIdentity: %v", err)
		}
		if !generated || got != macmigrateKeyPath(home) {
			t.Fatalf("got (%q, gen=%v), want (%q, gen=true)", got, generated, macmigrateKeyPath(home))
		}
		if !keypairExists(got) {
			t.Fatalf("generated keypair missing at %s", got)
		}
	})
}

// TestFindExistingIdentity covers cleanup's resolution: it must find whichever
// key setup would have installed, including a standard key when no macmigrate
// key exists — otherwise cleanup wouldn't actually reverse setup.
func TestFindExistingIdentity(t *testing.T) {
	t.Run("falls back to a standard key", func(t *testing.T) {
		home := t.TempDir()
		want := writeKeypair(t, home, "id_ed25519", "AAAAstd")
		got, err := findExistingIdentity(home, "")
		if err != nil || got != want {
			t.Fatalf("got (%q, %v), want (%q, nil)", got, err, want)
		}
	})

	t.Run("none found returns empty", func(t *testing.T) {
		home := t.TempDir()
		got, err := findExistingIdentity(home, "")
		if err != nil || got != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", got, err)
		}
	})
}

func TestReadPubKey(t *testing.T) {
	home := t.TempDir()
	path := writeKeypair(t, home, macmigrateKeyName, "AAAAbody")
	line, body, err := readPubKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if body != "AAAAbody" {
		t.Errorf("body = %q, want AAAAbody", body)
	}
	if line != "ssh-ed25519 AAAAbody macmigrate@test" {
		t.Errorf("line = %q", line)
	}

	t.Run("malformed errors", func(t *testing.T) {
		dir := filepath.Join(home, ".ssh")
		bad := filepath.Join(dir, "bad")
		if err := os.WriteFile(bad+".pub", []byte("garbage\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readPubKey(bad); err == nil {
			t.Fatal("expected error for malformed pub key")
		}
	})
}
