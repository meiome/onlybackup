package store

import (
	"errors"
	"github.com/meiome/onlybackup/internal/model"
	"testing"
	"time"
)

func TestAuditManualPauseSurvivesExcludedIncident(t *testing.T) {
	s, key, _ := setup(t, model.Profile{Name: "audit", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	enableRetentionForTest(t, s, now)
	if err := s.PauseRetention("manutenzione amministrativa"); err != nil {
		t.Fatal(err)
	}
	for i, stable := range []string{"audit-upload-1", "audit-upload-2"} {
		if _, err := s.OpenAnomaly(stable, "upload", key, "", "rete interrotta", now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	anomalies, err := s.Anomalies(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateAnomalyExclusion(anomalies[0].ID, now.Add(-time.Minute), now.Add(time.Minute), "audit", "incidente rete riconosciuto", []string{"upload"}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	status, err := s.AutomationStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.DeletionBlocked || !status.ManualPaused || status.BlockReason != "manutenzione amministrativa" {
		t.Fatalf("manual pause lost after exclusion: enabled=%v blocked=%v state=%s reason=%q", status.Enabled, status.DeletionBlocked, status.MonitoringState, status.BlockReason)
	}
}

func TestMinimumRecheckedAtEveryDestructiveBoundary(t *testing.T) {
	for _, kind := range []string{"quarantine", "purge"} {
		for _, phase := range []string{"request", "authorize", "validate", "committed"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				s, key, token := setup(t, model.Profile{Name: "minimum", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
				now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
				old := completedBackup(t, s, token, "minimum-old-backup-12345", now.AddDate(0, 0, -20))
				completedBackup(t, s, token, "minimum-new-backup-12345", now.AddDate(0, 0, -1))
				enableRetentionForTest(t, s, now)
				if kind == "purge" {
					at := now.Add(-72 * time.Hour)
					op, err := s.RequestQuarantine(old.ID, "manual", "test", "fixture", at)
					if err != nil {
						t.Fatal(err)
					}
					op, err = authorizeOperationForTest(t, s, op.ID, at)
					if err != nil {
						t.Fatal(err)
					}
					if err = confirmOperationForTest(t, s, op.ID, op.Ticket, true, "", at); err != nil {
						t.Fatal(err)
					}
				}
				raise := func() {
					t.Helper()
					if err := s.SetKeyRetentionDays(key, 30); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "request" {
					raise()
				}
				var op model.RetentionOperation
				var err error
				if kind == "quarantine" {
					op, err = s.RequestQuarantine(old.ID, "manual", "test", "test", now)
				} else {
					op, err = s.RequestPurge(old.ID, "automatic", "test", "test", now)
				}
				if phase == "request" {
					if !errors.Is(err, model.ErrRetentionMinimum) {
						t.Fatalf("protected request: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if phase == "authorize" {
					raise()
				}
				op, err = authorizeOperationForTest(t, s, op.ID, now)
				if phase == "authorize" {
					if !errors.Is(err, model.ErrRetentionMinimum) {
						t.Fatalf("protected authorization: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if phase == "validate" {
					raise()
				}
				err = validateOperationForTest(t, s, op.ID, op.Ticket, now)
				if phase == "validate" {
					if !errors.Is(err, model.ErrRetentionMinimum) {
						t.Fatalf("protected final permission: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				raise()
				if err = validateOperationForTest(t, s, op.ID, op.Ticket, now); err != nil {
					t.Fatalf("committed permission must survive: %v", err)
				}
			})
		}
	}
}

func TestManualPauseSurvivesChecksReopenAndRequiresResume(t *testing.T) {
	s, key, token := setup(t, model.Profile{Name: "persistent", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	old := completedBackup(t, s, token, "pause-old-backup-123456", now.AddDate(0, 0, -20))
	completedBackup(t, s, token, "pause-new-backup-123456", now.AddDate(0, 0, -1))
	enableRetentionForTest(t, s, now)
	op, err := s.RequestQuarantine(old.ID, "automatic", "test", "test", now)
	if err != nil {
		t.Fatal(err)
	}
	op, err = authorizeOperationForTest(t, s, op.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PauseRetention("mia pausa"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCheckState(model.MonitoringLearning, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = validateOperationForTest(t, s, op.ID, op.Ticket, now); !errors.Is(err, model.ErrRetentionBlocked) {
		t.Fatalf("paused final permission: %v", err)
	}
	if err = s.ResumeRetention(now); err == nil {
		t.Fatal("resume accepted with uncertain operation")
	}
	if err = s.ResetAuthorized(op.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = authorizeOperationForTest(t, s, op.ID, now); !errors.Is(err, model.ErrRetentionBlocked) {
		t.Fatalf("paused authorization: %v", err)
	}
	var seq int
	var name, path string
	if err = s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	status, err := reopened.AutomationStatus()
	if err != nil || !status.ManualPaused || !status.DeletionBlocked || status.ManualPauseReason != "mia pausa" {
		t.Fatalf("pause lost: %+v %v", status, err)
	}
	if err = reopened.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}
	status, err = reopened.AutomationStatus()
	if err != nil || status.ManualPaused || status.DeletionBlocked {
		t.Fatalf("resume failed: %+v %v key=%s", status, err, key)
	}
}
