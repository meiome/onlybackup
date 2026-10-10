package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

func TestRevokeResolvesCancelledMaintenanceFailures(t *testing.T) {
	for _, kind := range []string{"quarantine", "purge"} {
		t.Run(kind, func(t *testing.T) {
			f := newProvisionalRetryFixture(t, kind)
			for i := 0; i < 2; i++ {
				if err := f.store.Revoke(f.key); err != nil {
					t.Fatal(err)
				}
			}
			op, err := f.store.Operation(f.op.ID)
			if err != nil || op.State != "cancelled" {
				t.Fatalf("operation not cancelled: %+v %v", op, err)
			}
			backup, err := f.store.Backup(op.BackupID)
			wantStatus := model.BackupComplete
			if kind == "purge" {
				wantStatus = model.BackupQuarantined
			}
			if err != nil || backup.Status != wantStatus {
				t.Fatalf("backup status: %s, want %s: %v", backup.Status, wantStatus, err)
			}
			active, err := f.store.Anomalies(true)
			if err != nil || len(active) != 0 {
				t.Fatalf("cancelled operation left active anomalies: %+v %v", active, err)
			}
			messages, err := f.store.DueMail(time.Now().Add(time.Minute), 100)
			if err != nil {
				t.Fatal(err)
			}
			resolutions := 0
			for _, message := range messages {
				if message.Kind == "resolution" && message.Body == fmt.Sprintf("maintenance-operation:%d", op.ID) {
					resolutions++
				}
			}
			if resolutions != 1 {
				t.Fatalf("expected one resolution despite repeated revocation, got %d", resolutions)
			}
			at := f.now.Add(time.Minute)
			check, err := f.store.StartMonitoringCheck(at)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.store.CompleteMonitoringCheck(check.ID, "completed_clean", "files intact", model.MonitoringRegular, 7, nil, at); err != nil {
				t.Fatal(err)
			}
			if err = f.store.ResumeRetention(at); err != nil {
				t.Fatalf("cancelled operation still prevents explicit resume: %v", err)
			}
		})
	}
}

func TestRevokePreservesUnfinishedOperationsAndIndependentHolds(t *testing.T) {
	for _, state := range []string{"requested", "authorized", "committed"} {
		t.Run(state, func(t *testing.T) {
			f := newProvisionalRetryFixture(t, "quarantine")
			if state != "requested" {
				op, err := f.store.AuthorizeOperation(f.op.ID, f.lease, f.now)
				if err != nil {
					t.Fatal(err)
				}
				if state == "committed" {
					if err = f.store.ValidateOperation(op.ID, op.Ticket, f.lease, f.now); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.store.OpenAnomaly("independent-failure", "maintenance", "", "", "other operation failed", f.now); err != nil {
				t.Fatal(err)
			}
			if err := f.store.PauseRetention("operator pause"); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Revoke(f.key); err != nil {
				t.Fatal(err)
			}
			op, err := f.store.Operation(f.op.ID)
			wantState, wantAnomalies := "cancelled", 1
			if state != "requested" {
				wantState, wantAnomalies = "authorized", 2
			}
			if err != nil || op.State != wantState || op.ExecutionCommitted != (state == "committed") {
				t.Fatalf("unexpected operation: %+v %v", op, err)
			}
			active, err := f.store.Anomalies(true)
			if err != nil || len(active) != wantAnomalies {
				t.Fatalf("unexpected active anomalies: %+v %v", active, err)
			}
			status, err := f.store.AutomationStatus()
			if err != nil || !status.DeletionBlocked || !status.ManualPaused || status.ManualPauseReason != "operator pause" {
				t.Fatalf("revocation cleared independent holds: %+v %v", status, err)
			}
		})
	}
}

func TestRevokeRollsBackWhenResolutionCannotBeRecorded(t *testing.T) {
	f := newProvisionalRetryFixture(t, "quarantine")
	if _, err := f.store.db.Exec(`CREATE TRIGGER reject_resolution BEFORE INSERT ON mail_queue
 WHEN NEW.kind='resolution' BEGIN SELECT RAISE(ABORT, 'resolution unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Revoke(f.key); err == nil {
		t.Fatal("revocation succeeded without recording resolution")
	}
	keys, err := f.store.Keys()
	if err != nil || len(keys) != 1 || keys[0].Revoked {
		t.Fatalf("key revocation was not rolled back: %+v %v", keys, err)
	}
	for _, id := range []int64{f.op.ID, f.other.ID} {
		op, err := f.store.Operation(id)
		if err != nil || op.State != "requested" {
			t.Fatalf("operation cancellation was not rolled back: %+v %v", op, err)
		}
		backup, err := f.store.Backup(op.BackupID)
		if err != nil || backup.Status != model.BackupDeleting {
			t.Fatalf("backup transition was not rolled back: %+v %v", backup, err)
		}
	}
	active, err := f.store.Anomalies(true)
	if err != nil || len(active) != 1 {
		t.Fatalf("anomaly resolution was not rolled back: %+v %v", active, err)
	}
}
