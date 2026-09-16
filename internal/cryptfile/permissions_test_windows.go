//go:build windows

package cryptfile

import (
	"testing"

	"github.com/meiome/onlybackup/internal/filesecurity"
	"golang.org/x/sys/windows"
)

func testInsecureIdentityPermissions(t *testing.T, identity string) {
	t.Helper()
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString(
		"D:P(A;;FA;;;" + tokenUser.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;WD)",
	)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(identity, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadIdentities(identity); err == nil {
		t.Fatal("identita con ACL permissiva accettata")
	}
	if err = filesecurity.RestrictPrivate(identity); err != nil {
		t.Fatal(err)
	}
}
