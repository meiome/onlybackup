//go:build windows

package filesecurity

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CheckPrivate accepts only files owned by the current user, LocalSystem or
// local Administrators whose DACL grants access exclusively to the same trusted
// principals.
// Windows reports synthetic POSIX mode bits, so chmod-style checks are not
// meaningful there.
func CheckPrivate(path string, _ os.FileInfo) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("lettura ACL Windows: %w", err)
	}
	if sd == nil {
		return errors.New("descrittore di sicurezza Windows assente")
	}
	userSID, systemSID, adminSID, err := privateSIDs()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return errors.New("proprietario Windows non verificabile")
	}
	if !owner.IsValid() || !isTrustedSID(owner, userSID, systemSID, adminSID) {
		return errors.New("il proprietario del file privato non e un account Windows fidato")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("DACL Windows assente o non sicura")
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("lettura voce ACL Windows: %w", err)
		}
		if ace == nil {
			return errors.New("voce ACL Windows non valida")
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
				continue
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.IsValid() {
				return errors.New("SID non valido nella DACL Windows")
			}
			if !isTrustedSID(sid, userSID, systemSID, adminSID) && ace.Mask != 0 {
				return fmt.Errorf("ACL Windows concede accesso a un account non autorizzato: %s", sid.String())
			}
		default:
			return fmt.Errorf("tipo di voce ACL Windows non supportato: %d", ace.Header.AceType)
		}
	}
	return nil
}

func isTrustedSID(candidate *windows.SID, trusted ...*windows.SID) bool {
	if candidate == nil {
		return false
	}
	for _, sid := range trusted {
		if sid != nil && candidate.Equals(sid) {
			return true
		}
	}
	return false
}

// RestrictPrivate replaces inherited permissions with a protected DACL for the
// current user, LocalSystem and local Administrators.
func RestrictPrivate(path string) error {
	userSID, _, _, err := privateSIDs()
	if err != nil {
		return err
	}
	sddl := "D:P(A;;FA;;;" + userSID.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("creazione ACL Windows privata: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("creazione DACL Windows privata fallita")
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("applicazione ACL Windows privata: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return CheckPrivate(path, info)
}

func privateSIDs() (user, system, administrators *windows.SID, err error) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || tokenUser == nil || tokenUser.User.Sid == nil {
		return nil, nil, nil, errors.New("utente Windows corrente non verificabile")
	}
	system, err = windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, nil, nil, err
	}
	administrators, err = windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, nil, nil, err
	}
	return tokenUser.User.Sid, system, administrators, nil
}
