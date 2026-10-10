package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/meiome/onlybackup/internal/localapi"
	"github.com/meiome/onlybackup/internal/localcli"
	"github.com/meiome/onlybackup/internal/localclient"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
	"github.com/meiome/onlybackup/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func auditService(t *testing.T, now *time.Time) (*store.Store, *Service, model.Key, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	key, token, err := db.CreateKey("audit", "XS")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetupAutomation(store.MailSettings{Host: "smtp.invalid", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", *now); err != nil {
		t.Fatal(err)
	}
	api := localapi.New(db, root)
	api.Now = func() time.Time { return *now }
	sock := filepath.Join(root, "audit.sock")
	serveUnix(t, sock, api.MaintenanceHandler())
	service := New(root, sock, "audit", nil)
	service.Now = func() time.Time { return *now }
	service.Filesystem = func(string) (policy.Filesystem, error) {
		return policy.Filesystem{Blocks: 10000, Bfree: 8600, Bavail: 8600, BlockSize: 1}, nil
	}
	return db, service, key, token
}

func TestAuditRetentionMinimumSurvivesLearning(t *testing.T) {
	now := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	db, service, key, token := auditService(t, &now)
	if err := db.SetKeyRetentionDays(key.ID, 30); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := db.AutomationStatus()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("initial state=%s revision=%d days=%d", first.MonitoringState, first.ModelRevision, first.RetentionDays)
	now = now.AddDate(0, 0, 15)
	data := make([]byte, 100)
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	for i := 14; i >= 1; i-- {
		at := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, -i)
		b, err := db.ReserveIdempotent(token, model.Metadata{Description: "audit", OriginalName: "db.sql"}, 100, digest, fmt.Sprintf("audit-learning-backup-%02d", i), at)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(service.Root, "archives", "backups", b.ID+".backup"), data, 0440); err != nil {
			t.Fatal(err)
		}
		if err = db.Complete(b.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err := db.AutomationStatus()
	if err != nil {
		t.Fatal(err)
	}
	models, err := db.LoadModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("model not learned: %+v", status)
	}
	if status.RetentionDaysForKey(key.ID) != 30 || status.RetentionDays != 7 {
		t.Fatalf("learning changed configured minima: %+v", status)
	}
	var learned policy.KeyModel
	if err = json.Unmarshal(models[key.ID], &learned); err != nil || !learned.Reliable {
		t.Fatalf("not learned: %+v %v", learned, err)
	}

	// A minimum that no longer fits must be reported, never silently reduced.
	if err = db.SetKeyRetentionDays(key.ID, 120); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	if err = service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = db.AutomationStatus()
	if err != nil || status.RetentionDaysForKey(key.ID) != 120 {
		t.Fatalf("minimum reduced: %+v %v", status, err)
	}
	anomalies, err := db.Anomalies(true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range anomalies {
		if a.StableKey == "capacity:retention-window" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing capacity warning for unsustainable configured minimum")
	}
	mail, err := db.DueMail(now, 100)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, m := range mail {
		if strings.Contains(m.Body, "minimi di conservazione impostati per chiave") {
			found = true
		}
	}
	if !found {
		t.Fatal("capacity problem missing from mail queue")
	}

}

func TestAuditQuotaProjectionIncludesModelAge(t *testing.T) {
	now := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	db, service, key, _ := auditService(t, &now)
	learned := policy.KeyModel{KeyID: key.ID, Timezone: "UTC", LearnedAt: now.AddDate(0, 0, -30), Reliable: true, Schedule: map[time.Weekday][]policy.Appointment{}}
	for day := time.Sunday; day <= time.Saturday; day++ {
		learned.Schedule[day] = []policy.Appointment{{MinuteOfDay: 12 * 60, MedianSize: 100, GrowthPerDay: 10}}
	}
	if err := db.SaveModel(key.ID, 7, "UTC", 7, learned, true, false, learned.LearnedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	check, err := db.StartMonitoringCheck(now)
	if err != nil {
		t.Fatal(err)
	}
	models, err := db.LoadModels()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := localclient.Snapshot{Status: model.AutomationStatus{Timezone: "UTC", ModelRevision: 7, RetentionDays: 7}, Keys: []model.Key{key}, Models: models, Quotas: []model.Quota{{KeyID: key.ID, Profile: model.Profile{TotalBytes: 200}}}}
	findings, _, _, _, err := service.monitor(context.Background(), snapshot, check.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.StableKey == "quota:"+key.ID {
			if err = db.CompleteMonitoringCheck(check.ID, "completed_with_anomalies", "quota", model.MonitoringRegular, 7, findings, now); err != nil {
				t.Fatal(err)
			}
			mail, err := db.DueMail(now, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range mail {
				if strings.Contains(message.Body, "quota insufficiente") && strings.Contains(message.Body, "disponibili 200 byte, previsti 402 byte") {
					return
				}
			}
			t.Fatal("quota warning not queued for delivery")
			return
		}
	}
	t.Fatal("missing quota warning: available=200, expected next copy>400; check uses 100+10*2=120")
}

func TestAdministratorCanSetMinimumThroughCLI(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	db, service, key, _ := auditService(t, &now)
	api := localapi.New(db, service.Root)
	sock := filepath.Join(service.Root, "admin.sock")
	serveUnix(t, sock, api.AdminHandler())
	var out, errs bytes.Buffer
	if err := localcli.Admin([]string{"--state", service.Root, "--admin-socket", sock, "retention", "minimum", "--key", key.ID, "--days", "30"}, &out, &errs); err != nil {
		t.Fatalf("CLI: %v %s", err, errs.String())
	}
	status, err := db.AutomationStatus()
	if err != nil || status.RetentionDaysForKey(key.ID) != 30 {
		t.Fatalf("CLI did not configure key: %+v %v", status, err)
	}
	// Raising the minimum must also postpone an already quarantined backup.
	snapshot := localclient.Snapshot{Status: status, Keys: []model.Key{key}, Backups: []model.Backup{{Receipt: model.Receipt{ID: "00000000000000000000000000000001", ReceivedAt: now.AddDate(0, 0, -20).Format(time.RFC3339), Status: model.BackupQuarantined, Size: 100}, KeyID: key.ID, PurgeNotBefore: now.Add(-time.Hour).Unix()}}}
	if err = service.schedulePurges(context.Background(), snapshot, now); err != nil {
		t.Fatalf("protected quarantine generated a purge request: %v", err)
	}
}

func TestAuditQuotaPendingWithinTolerance(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 5, 0, 0, time.UTC)
	db, service, key, _ := auditService(t, &now)
	learned := policy.KeyModel{KeyID: key.ID, Timezone: "UTC", LearnedAt: now.Add(-5*time.Minute).AddDate(0, 0, -30), Reliable: true, Schedule: map[time.Weekday][]policy.Appointment{
		time.Tuesday:   {{MinuteOfDay: 720, MedianSize: 100, GrowthPerDay: 10}},
		time.Wednesday: {{MinuteOfDay: 720, MedianSize: 1}},
	}}
	if err := db.SaveModel(key.ID, 7, "UTC", 7, learned, true, false, learned.LearnedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	check, err := db.StartMonitoringCheck(now)
	if err != nil {
		t.Fatal(err)
	}
	models, err := db.LoadModels()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := localclient.Snapshot{Status: model.AutomationStatus{Timezone: "UTC", ModelRevision: 7, RetentionDays: 7}, Keys: []model.Key{key}, Models: models, Quotas: []model.Quota{{KeyID: key.ID, Profile: model.Profile{TotalBytes: 110}}}}
	findings, _, _, _, err := service.monitor(context.Background(), snapshot, check.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.StableKey == "quota:"+key.ID && strings.Contains(finding.Detail, "disponibili 110 byte, previsti 400 byte") {
			return
		}
	}
	t.Fatalf("missing warning for pending copy: %+v", findings)
}
