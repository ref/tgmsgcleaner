package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

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

func TestDeleteAccountRejectsUnsafeNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"", ".", "..", "../config.json", "a/b"} {
		if err := DeleteAccount(name); err == nil {
			t.Errorf("DeleteAccount(%q) succeeded, want error", name)
		}
	}
}
