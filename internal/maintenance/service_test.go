package maintenance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meiome/onlybackup/internal/localclient"
	"github.com/meiome/onlybackup/internal/model"
)

func TestWriterUnavailableClassifiesOnlyWriterTransportErrors(t *testing.T) {
	transport := &url.Error{Op: "Get", URL: "http://onlybackup/v1/snapshot", Err: &net.OpError{Op: "dial", Net: "unix", Err: errors.New("connection refused")}}
	if !WriterUnavailable(transport) {
		t.Fatal("errore di trasporto writer non riconosciuto")
	}
	if WriterUnavailable(&os.PathError{Op: "open", Path: "/archive/missing.backup", Err: os.ErrNotExist}) {
		t.Fatal("file backup mancante classificato come writer non disponibile")
	}
}

func TestPhysicalMissingIsDistinctFromScheduleMissing(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"backups", "quarantine"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{Root: root}
	backup := model.Backup{Receipt: model.Receipt{ID: "00000000000000000000000000000001", Status: model.BackupComplete, Size: 10, SHA256: strings.Repeat("a", 64)}, KeyID: "key"}
	findings := service.fileFindings(localclient.Snapshot{Backups: []model.Backup{backup}})
	if len(findings) != 1 || findings[0].Kind != "file_missing" || findings[0].BackupID != backup.ID {
		t.Fatalf("file fisico mancante non distinto: %+v", findings)
	}
}

func TestAcknowledgedMissingIsNotReportedOrCountedAsRecoverable(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"backups", "quarantine"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	missing := model.Backup{Receipt: model.Receipt{ID: "00000000000000000000000000000001", Status: model.BackupComplete, Size: 10, SHA256: strings.Repeat("a", 64)}, KeyID: "key"}
	present := model.Backup{Receipt: model.Receipt{ID: "00000000000000000000000000000002", Status: model.BackupComplete, Size: 10, SHA256: strings.Repeat("b", 64)}, KeyID: "key"}
	snapshot := localclient.Snapshot{Backups: []model.Backup{missing, present}, AcknowledgedMissingBackupIDs: []string{missing.ID}}
	service := &Service{Root: root}
	findings := service.fileFindings(snapshot)
	if len(findings) != 1 || findings[0].BackupID != present.ID || findings[0].Kind != "file_missing" {
		t.Fatalf("acknowledged missing file reported again: %+v", findings)
	}
	recoverable := recoverableBackups(snapshot)
	if len(recoverable) != 1 || recoverable[0].ID != present.ID {
		t.Fatalf("acknowledged missing file counted as recoverable: %+v", recoverable)
	}
}

func TestConcurrentCompletedUploadIsNotUnexpected(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"backups", "quarantine"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("concurrent upload")
	digest := sha256.Sum256(data)
	backup := model.Backup{Receipt: model.Receipt{ID: "00000000000000000000000000000003", Status: model.BackupComplete, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}, KeyID: "key"}
	if err := os.WriteFile(filepath.Join(root, "backups", backup.ID+".backup"), data, 0400); err != nil {
		t.Fatal(err)
	}
	service := &Service{Root: root}
	findings := service.fileFindings(localclient.Snapshot{})
	if len(findings) != 1 || !strings.HasPrefix(findings[0].StableKey, "unexpected-file:") {
		t.Fatalf("fixture did not expose concurrent file: %+v", findings)
	}
	if findings = service.verifyUnexpectedFiles(findings, localclient.Snapshot{Backups: []model.Backup{backup}}); len(findings) != 0 {
		t.Fatalf("completed concurrent upload reported as corruption: %+v", findings)
	}
}

func TestPhysicalQuarantineRecoveryAndPurge(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"backups", "quarantine", "maintenance"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("isolated test backup")
	digest := sha256.Sum256(data)
	b := model.Backup{Receipt: model.Receipt{ID: "00000000000000000000000000000001", Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}}
	archive := filepath.Join(root, "backups", b.ID+".backup")
	if err := os.WriteFile(archive, data, 0400); err != nil {
		t.Fatal(err)
	}
	s := &Service{Root: root}
	if err := s.execute(model.RetentionOperation{BackupID: b.ID, Kind: "quarantine"}, b); err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(root, "quarantine", b.ID+".backup")
	if _, err := os.Stat(quarantine); err != nil {
		t.Fatal(err)
	}
	if err := s.execute(model.RetentionOperation{BackupID: b.ID, Kind: "recover"}, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	}
	if err := s.execute(model.RetentionOperation{BackupID: b.ID, Kind: "quarantine"}, b); err != nil {
		t.Fatal(err)
	}
	if err := s.execute(model.RetentionOperation{BackupID: b.ID, Kind: "purge"}, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(quarantine); !os.IsNotExist(err) {
		t.Fatalf("purge did not remove test file: %v", err)
	}
}

func TestPhysicalOperationRejectsCollisionAndSymlink(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"backups", "quarantine", "maintenance"} {
		_ = os.Mkdir(filepath.Join(root, name), 0700)
	}
	data := []byte("backup")
	digest := sha256.Sum256(data)
	b := model.Backup{Receipt: model.Receipt{ID: "00000000000000000000000000000002", Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}}
	archive := filepath.Join(root, "backups", b.ID+".backup")
	quarantine := filepath.Join(root, "quarantine", b.ID+".backup")
	if err := os.WriteFile(archive, data, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quarantine, data, 0400); err != nil {
		t.Fatal(err)
	}
	s := &Service{Root: root}
	if err := s.execute(model.RetentionOperation{BackupID: b.ID, Kind: "quarantine"}, b); err == nil {
		t.Fatal("destination collision accepted")
	}
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(quarantine); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", archive); err != nil {
		t.Fatal(err)
	}
	if err := s.execute(model.RetentionOperation{BackupID: b.ID, Kind: "quarantine"}, b); err == nil {
		t.Fatal("symlink source accepted")
	}
}
