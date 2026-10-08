package maintenance

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

func TestAutomaticRetentionReclaimsSpaceWithQuarantineMarginWarning(t *testing.T) {
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	db, service, key, token := auditService(t, &now)
	data := make([]byte, 100)
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	deposit := func(at time.Time) model.Backup {
		t.Helper()
		backup, err := db.ReserveIdempotent(token, model.Metadata{Description: "margin test", OriginalName: "db.bin"},
			int64(len(data)), digest, "margin-test-"+at.Format("20060102"), at)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(service.Root, "backups", backup.ID+".backup"), data, 0440); err != nil {
			t.Fatal(err)
		}
		if err = db.Complete(backup.ID, at); err != nil {
			t.Fatal(err)
		}
		backup, err = db.Backup(backup.ID)
		if err != nil {
			t.Fatal(err)
		}
		return backup
	}
	for day := 14; day >= 1; day-- {
		deposit(time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, -day))
	}
	for _, day := range []int{31, 30} {
		deposit(now.AddDate(0, 0, -day))
	}
	if err := db.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if err := db.SetReserveFree(1); err != nil {
		t.Fatal(err)
	}
	// The configured minima fit, but the available 200 bytes are less than
	// the 240-byte forecast plus reserve. Quarantine does not free any bytes.
	available := uint64(200)
	service.Filesystem = func(string) (policy.Filesystem, error) {
		return policy.Filesystem{Blocks: 10000, Bfree: available, Bavail: available, BlockSize: 1}, nil
	}
	ctx := context.Background()
	if err := service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := db.AutomationStatus()
	if err != nil || !status.LastCheckRegular || status.DeletionBlocked || status.ActivationPending || status.ActiveAnomalies != 1 {
		t.Fatalf("native activation or warning not preserved: %+v %v", status, err)
	}
	backups, err := db.Backups(key.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var quarantined []model.Backup
	for _, backup := range backups {
		if backup.Status == model.BackupQuarantined {
			quarantined = append(quarantined, backup)
			if backup.PurgeNotBefore != now.Add(48*time.Hour).Unix() {
				t.Fatalf("quarantine shortened: %+v", backup)
			}
			if _, err = os.Stat(filepath.Join(service.Root, "quarantine", backup.ID+".backup")); err != nil {
				t.Fatal(err)
			}
		}
		protected, err := policy.RetentionProtected(backup, now, "UTC", 7)
		if err != nil || (protected && backup.Status != model.BackupComplete) {
			t.Fatalf("protected copy moved: %+v %v", backup, err)
		}
	}
	if len(quarantined) == 0 {
		t.Fatal("automatic retention did not quarantine any eligible copy")
	}
	if _, err = db.RequestPurge(quarantined[0].ID, "automatic", "test", "too early", now.Add(48*time.Hour-time.Second)); !errors.Is(err, model.ErrTooEarly) {
		t.Fatalf("purge allowed before 48 hours: %v", err)
	}
	quotaBefore, err := db.Quota(key.ID, now)
	if err != nil || quotaBefore.Used != 1600 {
		t.Fatalf("quarantine released quota early: %+v %v", quotaBefore, err)
	}
	// Receive both expected daily backups while quarantine occupies the disk.
	for day := 0; day < 2; day++ {
		deposit(time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, day))
	}
	now = now.Add(48 * time.Hour)
	if err = service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, backup := range quarantined {
		stored, err := db.Backup(backup.ID)
		if err != nil || stored.Status != model.BackupDeleted {
			t.Fatalf("capacity warning prevented purge: %+v %v", stored, err)
		}
		if _, err = os.Lstat(filepath.Join(service.Root, "quarantine", backup.ID+".backup")); !os.IsNotExist(err) {
			t.Fatalf("purged file still exists: %v", err)
		}
	}
	quotaAfter, err := db.Quota(key.ID, now)
	if err != nil || quotaAfter.Used != 1800-int64(len(quarantined))*100 {
		t.Fatalf("quota not released after purge: %+v %v", quotaAfter, err)
	}
	status, err = db.AutomationStatus()
	if err != nil || status.DeletionBlocked || !status.LastCheckRegular || status.ActiveAnomalies != 1 {
		t.Fatalf("warning stopped retention after purge: %+v %v", status, err)
	}
	incidents, err := db.Anomalies(true)
	if err != nil || len(incidents) != 1 || incidents[0].StableKey != policy.QuarantineMarginAnomalyKey {
		t.Fatalf("wrong active incidents: %+v %v", incidents, err)
	}
	mail, err := db.DueMail(now, 100)
	if err != nil {
		t.Fatal(err)
	}
	var alerts int
	for _, message := range mail {
		if strings.Contains(message.Subject, "AVVISO NON BLOCCANTE - margine di spazio") {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("capacity warning mail missing or duplicated: %d", alerts)
	}
	available = 1000
	now = now.Add(time.Minute)
	if err = service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = db.AutomationStatus()
	if err != nil || status.DeletionBlocked || status.ActiveAnomalies != 0 {
		t.Fatalf("resolved space warning paused retention: %+v %v", status, err)
	}
}
