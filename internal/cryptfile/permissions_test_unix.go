//go:build !windows

package cryptfile

import (
	"os"
	"testing"
)

func testInsecureIdentityPermissions(t *testing.T, identity string) {
	t.Helper()
	if err := os.Chmod(identity, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIdentities(identity); err == nil {
		t.Fatal("identita con permessi permissivi accettata")
	}
	if err := os.Chmod(identity, 0600); err != nil {
		t.Fatal(err)
	}
}
