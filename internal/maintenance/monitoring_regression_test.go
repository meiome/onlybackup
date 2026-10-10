package maintenance

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/localclient"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

func TestLongScanDoesNotLoseMissingAppointment(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 20, 0, 0, time.UTC)
	db, service, key, _ := auditService(t, &now)
	learned := policy.KeyModel{KeyID: key.ID, Timezone: "UTC", LearnedAt: now.Add(-time.Hour), Reliable: true,
		Schedule: map[time.Weekday][]policy.Appointment{time.Friday: {{MinuteOfDay: 720, MedianSize: 100}}}}
	if err := db.SaveModel(key.ID, 7, "UTC", 7, learned, true, false, learned.LearnedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	service.Filesystem = func(string) (policy.Filesystem, error) {
		now = now.Add(45 * time.Minute)
		return policy.Filesystem{Blocks: 10000, Bfree: 8600, Bavail: 8600, BlockSize: 1}, nil
	}
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	checks, err := db.MonitoringChecks(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range checks[0].Models {
		if scope.CoveredTo > checks[0].StartedAt-int64(policy.ScheduleTolerance/time.Second) {
			t.Fatalf("coverage advanced beyond evaluated time: %+v", checks[0])
		}
	}
	service.Filesystem = func(string) (policy.Filesystem, error) {
		return policy.Filesystem{Blocks: 10000, Bfree: 8600, Bavail: 8600, BlockSize: 1}, nil
	}
	now = now.Add(5 * time.Minute)
	if err := service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	anomalies, err := db.Anomalies(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range anomalies {
		if a.Kind == "schedule_missing" && a.EventAt == time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC).Unix() {
			return
		}
	}
	t.Fatal("12:00 backup never arrived, but successive completed checks lost its alarm")
}

func TestOperationalExclusionsKeepArrivalsAndSizeChecks(t *testing.T) {
	for _, kind := range []string{"extra", "time", "upload", "schedule_missing"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)
			db, service, key, _ := auditService(t, &now)
			at := now.Add(-time.Hour)
			learned := policy.KeyModel{KeyID: key.ID, Timezone: "UTC", LearnedAt: at.Add(-time.Hour), Reliable: true,
				Schedule: map[time.Weekday][]policy.Appointment{time.Friday: {{MinuteOfDay: 720, MedianSize: 100}}}}
			if err := db.SaveModel(key.ID, 7, "UTC", 7, learned, true, false, learned.LearnedAt); err != nil {
				t.Fatal(err)
			}
			if err := db.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
				t.Fatal(err)
			}
			models, err := db.LoadModels()
			if err != nil {
				t.Fatal(err)
			}
			check, err := db.StartMonitoringCheck(now)
			if err != nil {
				t.Fatal(err)
			}
			backup := model.Backup{KeyID: key.ID, Receipt: model.Receipt{ID: strings.Repeat("a", 32), Status: model.BackupComplete, ReceivedAt: at.Format(time.RFC3339), Size: 10}}
			exclusion := model.AnomalyExclusion{KeyID: key.ID, StartsAt: at.Add(-time.Hour).Unix(), EndsAt: at.Add(time.Hour).Unix(), Kinds: []string{kind}}
			snapshot := localclient.Snapshot{Status: model.AutomationStatus{Timezone: "UTC", ModelRevision: 7, RetentionDays: 7}, Keys: []model.Key{key}, Models: models,
				Backups: []model.Backup{backup}, Exclusions: []model.AnomalyExclusion{exclusion}}
			findings, _, _, _, err := service.monitor(context.Background(), snapshot, check.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			sizeFound := false
			for _, finding := range findings {
				if finding.Kind == "schedule_missing" {
					t.Fatalf("exclusion invented a missing backup: %+v", finding)
				}
				sizeFound = sizeFound || finding.Kind == "size"
			}
			if !sizeFound {
				t.Fatal("operational exclusion suppressed an integrity/size finding")
			}
			if len(observations(snapshot.Backups, snapshot.Exclusions)) != 0 {
				t.Fatal("incident interval unexpectedly contributed to learning")
			}
		})
	}
}

func TestLargeHistoricalCatalogAllowsMaintenance(t *testing.T) {
	now := time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)
	_, service, key, _ := auditService(t, &now)
	db, err := sql.Open("sqlite3", filepath.Join(service.Root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO backups(id,key_id,description,original_name,size_bytes,sha256,status,started_at,received_at) VALUES(?,?,?,?,1,?,'deleted',?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	description := strings.Repeat("界", 1000)
	if err := (model.Metadata{Description: description, OriginalName: "db.sql"}).Validate(); err != nil {
		t.Fatal(err)
	}
	// These valid metadata alone exceed the former 64 MiB response ceiling
	// once encoded with their catalogue fields. Deleted rows remain in history.
	const count = 22000
	for i := 0; i < count; i++ {
		if _, err = stmt.Exec(fmt.Sprintf("%032x", i+1), key.ID, description, "db.sql", strings.Repeat("a", 64), now.Unix(), now.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var snapshot localclient.Snapshot
	if err = service.call(context.Background(), "GET", "/v1/snapshot", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Backups) != count || snapshot.Backups[0].ID != fmt.Sprintf("%032x", count) || snapshot.Backups[count-1].ID != fmt.Sprintf("%032x", 1) {
		t.Fatal("paged catalogue lost records or changed ordering")
	}
	snapshot = localclient.Snapshot{}
	if err = service.RunOnce(context.Background()); err != nil {
		t.Fatalf("historical records prevented maintenance: %v", err)
	}
}
