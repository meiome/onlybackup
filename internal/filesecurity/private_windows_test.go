//go:build windows

package filesecurity

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPrivateFileWindowsACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RestrictPrivate(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckPrivate(path, info); err != nil {
		t.Fatal(err)
	}

	userSID, _, _, err := privateSIDs()
	if err != nil {
		t.Fatal(err)
	}
	insecure, err := windows.SecurityDescriptorFromString(
		"D:P(A;;FA;;;" + userSID.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;WD)",
	)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := insecure.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err = CheckPrivate(path, info); err == nil {
		t.Fatal("ACL leggibile da Everyone accettata")
	}
}
