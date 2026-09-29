package maintenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSMTPCredentialsAreIsolatedFromReceiverTLS(t *testing.T) {
	if role := os.Getenv("ONLYBACKUP_PERMISSION_PROBE_ROLE"); role != "" {
		credentials := os.Getenv("ONLYBACKUP_PERMISSION_PROBE_CREDENTIALS")
		tlsKey := os.Getenv("ONLYBACKUP_PERMISSION_PROBE_TLS_KEY")
		switch role {
		case "maintenance":
			if _, err := os.ReadFile(credentials); err != nil {
				t.Fatalf("maintenance non legge le credenziali SMTP: %v", err)
			}
			if _, err := os.ReadFile(tlsKey); err == nil {
				t.Fatal("maintenance legge la chiave TLS del receiver")
			}
		case "receiver":
			if _, err := os.ReadFile(tlsKey); err != nil {
				t.Fatalf("receiver non legge la propria chiave TLS: %v", err)
			}
			if _, err := os.ReadFile(credentials); err == nil {
				t.Fatal("receiver legge le credenziali SMTP")
			}
		default:
			t.Fatalf("ruolo probe non valido: %s", role)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("richiede UID 0 o il container di isolamento per simulare gli account di servizio")
	}

	const (
		maintenanceUID = 12006
		maintenanceGID = 12007
		receiverUID    = 12008
		receiverGID    = 12009
	)
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(root), 0755); err != nil {
		t.Fatal(err)
	}
	tlsDirectory := filepath.Join(root, "onlybackup")
	mailDirectory := filepath.Join(root, "onlybackup-maintenance")
	if err := os.Mkdir(tlsDirectory, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(tlsDirectory, 0, receiverGID); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mailDirectory, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(mailDirectory, 0, maintenanceGID); err != nil {
		t.Fatal(err)
	}
	tlsKey := filepath.Join(tlsDirectory, "server.key")
	if err := os.WriteFile(tlsKey, []byte("test TLS key"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(tlsKey, 0, receiverGID); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(mailDirectory, "mail-credentials.json")
	if err := os.WriteFile(credentials, []byte(`{"username":"test","password":"dummy"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(credentials, maintenanceUID, maintenanceGID); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	probe := func(role string, uid, gid uint32) {
		t.Helper()
		command := exec.Command(executable, "-test.run=^TestSMTPCredentialsAreIsolatedFromReceiverTLS$", "-test.v")
		command.Env = append(os.Environ(),
			"ONLYBACKUP_PERMISSION_PROBE_ROLE="+role,
			"ONLYBACKUP_PERMISSION_PROBE_CREDENTIALS="+credentials,
			"ONLYBACKUP_PERMISSION_PROBE_TLS_KEY="+tlsKey)
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{gid}}}
		if output, probeErr := command.CombinedOutput(); probeErr != nil {
			t.Fatalf("probe %s: %v\n%s", role, probeErr, output)
		}
	}
	probe("maintenance", maintenanceUID, maintenanceGID)
	probe("receiver", receiverUID, receiverGID)
}
