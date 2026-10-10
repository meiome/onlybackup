package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/localapi"
	"github.com/meiome/onlybackup/internal/localcli"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

func TestDeferredResumeThroughCLIRespectsThresholdAndLaterPause(t *testing.T) {
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	db, service, key, token := auditService(t, &now)
	api := localapi.New(db, service.Root)
	api.Now = func() time.Time { return now }
	sock := filepath.Join(service.Root, "admin.sock")
	serveUnix(t, sock, api.AdminHandler())
	run := func(args ...string) {
		t.Helper()
		var out, errs bytes.Buffer
		if err := localcli.Admin(append([]string{"--state", service.Root, "--admin-socket", sock}, args...), &out, &errs); err != nil {
			t.Fatalf("CLI: %v %s", err, errs.String())
		}
	}
	run("automation", "setup", "--smtp-host", "smtp.test", "--smtp-port", "25", "--from", "backup@test", "--to", "admin@test", "--timezone", "UTC", "--retention", "manual", "--confirm", "CONFIGURA MANUAL")
	data := make([]byte, 100)
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	var oldest model.Backup
	for day := 14; day >= 1; day-- {
		at := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, -day)
		backup, err := db.ReserveIdempotent(token, model.Metadata{Description: "activation", OriginalName: "db.bin"}, 100, digest, fmt.Sprintf("activation-cli-copy-%02d", day), at)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(service.Root, "archives", "backups", backup.ID+".backup"), data, 0440); err != nil {
			t.Fatal(err)
		}
		if err = db.Complete(backup.ID, at); err != nil {
			t.Fatal(err)
		}
		if day == 14 {
			oldest = backup
		}
	}
	if err := db.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	available := uint64(20001) // Just below the 80% physical-use threshold.
	service.Filesystem = func(string) (policy.Filesystem, error) {
		return policy.Filesystem{Blocks: 100000, Bfree: available, Bavail: available, BlockSize: 1}, nil
	}
	cycle := func() {
		t.Helper()
		if err := service.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	cycle()
	status, err := db.AutomationStatus()
	if err != nil || status.Enabled || !status.ActivationRequired || !status.DeletionBlocked {
		t.Fatalf("manual install activated itself: %+v %v", status, err)
	}
	run("retention", "resume", "--when-ready", "--confirm", "PRENOTA")
	cycle()
	status, err = db.AutomationStatus()
	if err != nil || status.DeletionBlocked || status.ResumePending || status.ActivationRequired {
		t.Fatalf("CLI request not consumed by a valid check: %+v %v", status, err)
	}
	backups, err := db.Backups(key.ID, 100)
	if err != nil || len(backups) != 14 {
		t.Fatalf("backups missing: %d %v", len(backups), err)
	}
	for _, backup := range backups {
		if backup.Status != model.BackupComplete {
			t.Fatalf("cleanup below threshold: %+v", backup)
		}
	}
	run("retention", "pause", "--reason", "later hold", "--confirm", "SOSPENDI")
	available = 20000 // Exactly 80%: later manual pause must still stop cleanup.
	cycle()
	status, err = db.AutomationStatus()
	if err != nil || !status.ManualPaused || !status.DeletionBlocked {
		t.Fatalf("consumed consent removed later pause: %+v %v", status, err)
	}
	stored, err := db.Backup(oldest.ID)
	if err != nil || stored.Status != model.BackupComplete {
		t.Fatalf("manual pause did not protect old backup: %+v %v", stored, err)
	}
	run("retention", "resume", "--when-ready", "--confirm", "PRENOTA")
	cycle()
	stored, err = db.Backup(oldest.ID)
	if err != nil || stored.Status != model.BackupQuarantined {
		t.Fatalf("oldest eligible copy not quarantined at threshold: %+v %v", stored, err)
	}
}
