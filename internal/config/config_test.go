package config

import (
	"os"
	"path/filepath"
	"testing"
)

// useTempHome points os.UserHomeDir at a temp dir on every platform, so tests
// never touch the real config directory.
func useTempHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)        // Unix and macOS
	t.Setenv("USERPROFILE", dir) // Windows
}

func TestDeleteAccount(t *testing.T) {
	useTempHome(t)

	session, err := SessionPath("79000000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SessionPath("79000000001"); err != nil {
		t.Fatal(err)
	}

	if err := DeleteAccount("79000000000"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	accounts, err := ListAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0] != "79000000001" {
		t.Errorf("accounts after delete = %v, want [79000000001]", accounts)
	}
	if _, err := os.Stat(filepath.Dir(session)); !os.IsNotExist(err) {
		t.Errorf("account dir still exists: %v", err)
	}
}

func TestUnsafeAccountNamesRejected(t *testing.T) {
	useTempHome(t)
	for _, name := range []string{"", ".", "..", "../config.json", "a/b"} {
		if err := DeleteAccount(name); err == nil {
			t.Errorf("DeleteAccount(%q) succeeded, want error", name)
		}
		if path, err := SessionPath(name); err == nil {
			t.Errorf("SessionPath(%q) = %q, want error", name, path)
		}
	}
	// Nothing may have been created along the way.
	if accounts, err := ListAccounts(); err != nil || len(accounts) != 0 {
		t.Errorf("accounts = %v, err = %v; want none", accounts, err)
	}
}
