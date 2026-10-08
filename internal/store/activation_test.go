package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

func activationFixture(t *testing.T) (*Store, string, string, time.Time) {
	t.Helper()
	s, keyID, token := setup(t, model.Profile{Name: "activation", TotalBytes: 10000, MaxBackupBytes: 1000, UploadsPerDay: 100, Concurrent: 2})
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	if err := s.SetupAutomation(MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveModel(keyID, 7, "UTC", 7, monitoringModel(keyID, now.AddDate(0, 0, -14)), true, false, now); err != nil {
		t.Fatal(err)
	}
	return s, keyID, token, now
}

func completeActivationCheck(t *testing.T, s *Store, state string, revision int64, findings []policy.Finding, now time.Time) {
	t.Helper()
	check, err := s.StartMonitoringCheck(now)
	if err != nil {
		t.Fatal(err)
	}
	outcome := "completed_clean"
	if len(findings) != 0 {
		outcome = "completed_with_anomalies"
	}
	if err = s.CompleteMonitoringCheck(check.ID, outcome, "activation test", state, revision, findings, now); err != nil {
		t.Fatal(err)
	}
}

func activationStatus(t *testing.T, s *Store) model.AutomationStatus {
	t.Helper()
	status, err := s.AutomationStatus()
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func TestNativeActivationNeedsMailProofAndCompletedRegularCheck(t *testing.T) {
	s, _, token, now := activationFixture(t)
	status := activationStatus(t, s)
	if !status.Enabled || !status.ActivationPending || !status.DeletionBlocked {
		t.Fatalf("native defaults missing: %+v", status)
	}
	old := completedBackup(t, s, token, "activation-old-copy-12345", now.AddDate(0, 0, -30))
	completedBackup(t, s, token, "activation-new-copy-12345", now.AddDate(0, 0, -1))
	completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now)
	if !activationStatus(t, s).DeletionBlocked {
		t.Fatal("activated before mail proof")
	}
	if err := s.ResumeRetention(now); err == nil {
		t.Fatal("immediate resume bypassed mail proof")
	}
	if err := s.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestQuarantine(old.ID, "automatic", "test", "before activation", now); !errors.Is(err, model.ErrRetentionBlocked) {
		t.Fatalf("destructive request before activation: %v", err)
	}
	completeActivationCheck(t, s, model.MonitoringLearning, 7, nil, now.Add(time.Minute))
	if !activationStatus(t, s).DeletionBlocked {
		t.Fatal("learning activated retention")
	}
	completeActivationCheck(t, s, model.MonitoringRegular, 7, []policy.Finding{{StableKey: policy.QuarantineMarginAnomalyKey, Kind: "capacity", Detail: "margin warning"}}, now.Add(2*time.Minute))
	status = activationStatus(t, s)
	if status.DeletionBlocked || status.ActivationPending || status.ResumePending || status.ActiveAnomalies != 1 {
		t.Fatalf("safe native activation failed: %+v", status)
	}
	if _, err := s.RequestQuarantine(old.ID, "automatic", "test", "after activation", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestInitialHoldSurvivesAnomalyExclusion(t *testing.T) {
	s, keyID, token, now := activationFixture(t)
	old := completedBackup(t, s, token, "initial-exclusion-old-123", now.AddDate(0, 0, -30))
	completedBackup(t, s, token, "initial-exclusion-new-123", now.AddDate(0, 0, -1))
	for _, incident := range []string{"first", "second"} {
		if _, err := s.OpenAnomaly(incident, "upload", keyID, "", "upload failed", now); err != nil {
			t.Fatal(err)
		}
	}
	anomalies, err := s.Anomalies(true)
	if err != nil || len(anomalies) != 2 {
		t.Fatalf("incidents missing: %+v %v", anomalies, err)
	}
	if _, err = s.CreateAnomalyExclusion(anomalies[0].ID, now.Add(-time.Hour), now.Add(time.Hour), "admin", "verified network outage", []string{"upload"}, now); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now)
	status := activationStatus(t, s)
	if !status.ActivationRequired || status.ActivationPending || !status.DeletionBlocked {
		t.Fatalf("exclusion removed the initial hold: %+v", status)
	}
	if _, err = s.RequestQuarantine(old.ID, "automatic", "test", "after exclusion", now); !errors.Is(err, model.ErrRetentionBlocked) {
		t.Fatalf("initial protection bypassed: %v", err)
	}
	if err = s.RequestRetentionResume(now); err != nil {
		t.Fatal(err)
	}
	completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now.Add(time.Minute))
	if activationStatus(t, s).DeletionBlocked {
		t.Fatal("explicit deferred consent could not release initial hold")
	}
}

func TestDeferredResumeWaitsForExistingIncidentResolution(t *testing.T) {
	s, _, _, now := activationFixture(t)
	if err := s.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAnomaly("existing", "catalog", "", "", "known incident", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestRetentionResume(now); err != nil {
		t.Fatal(err)
	}
	completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now)
	if status := activationStatus(t, s); !status.ResumePending || !status.DeletionBlocked {
		t.Fatalf("existing incident bypassed: %+v", status)
	}
	if _, err := s.ResolveAnomaly("existing", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now.Add(2*time.Minute))
	if status := activationStatus(t, s); status.ResumePending || status.DeletionBlocked {
		t.Fatalf("resolved incident prevented explicit release: %+v", status)
	}
}

func TestSetupModesPreserveAdministrativePauses(t *testing.T) {
	for _, mode := range []string{"auto", "manual"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, now := activationFixture(t)
			if err := s.PauseRetention("explicit hold"); err != nil {
				t.Fatal(err)
			}
			if err := s.RequestRetentionResume(now); err != nil {
				t.Fatal(err)
			}
			if err := s.SetupAutomationMode(MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", mode, now); err != nil {
				t.Fatal(err)
			}
			if err := s.MarkMailTested(now); err != nil {
				t.Fatal(err)
			}
			completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now)
			status := activationStatus(t, s)
			if !status.ManualPaused || status.ManualPauseReason != "explicit hold" || !status.DeletionBlocked || status.ResumePending || status.Enabled != (mode == "auto") {
				t.Fatalf("setup changed the administrative pause: %+v", status)
			}
		})
	}
}

func TestSetupWithoutModePreservesPreviousChoice(t *testing.T) {
	for _, mode := range []string{"auto", "manual"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _, now := activationFixture(t)
			settings := MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}
			if err := s.SetupAutomationMode(settings, "UTC", mode, now); err != nil {
				t.Fatal(err)
			}
			settings.Host = "replacement.smtp.test"
			if err := s.SetupAutomation(settings, "UTC", now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.MarkMailTested(now); err != nil {
				t.Fatal(err)
			}
			completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now.Add(2*time.Minute))
			status := activationStatus(t, s)
			if status.Enabled != (mode == "auto") || status.DeletionBlocked != (mode == "manual") {
				t.Fatalf("SMTP setup changed the retention choice: %+v", status)
			}
		})
	}
}

func TestSetupInvalidatesChecksFromPreviousConfiguration(t *testing.T) {
	for _, previousRevision := range []int64{0, 7} {
		t.Run(fmt.Sprint(previousRevision), func(t *testing.T) {
			s, keyID, _, now := activationFixture(t)
			if err := s.SetCheckState(model.MonitoringLearning, previousRevision, now); err != nil {
				t.Fatal(err)
			}
			check, err := s.StartMonitoringCheck(now)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.SetupAutomationMode(MailSettings{Host: "replacement.smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", "auto", now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err = s.MarkMailTested(now); err != nil {
				t.Fatal(err)
			}
			if _, err = s.PlanMonitoringCheck(check.ID, now.Add(2*time.Minute)); err == nil {
				t.Fatal("previous configuration check could still plan work")
			}
			findings := []policy.Finding{{StableKey: "obsolete-setup-finding", Kind: "catalog", Detail: "stale configuration"}}
			if err = s.CompleteMonitoringCheck(check.ID, "completed_with_anomalies", "obsolete", model.MonitoringRegular, 7, findings, now.Add(2*time.Minute)); err == nil {
				t.Fatal("previous configuration check accepted")
			}
			status := activationStatus(t, s)
			if !status.ActivationPending || !status.ActivationRequired || !status.DeletionBlocked || status.ModelRevision != 0 || status.ActiveAnomalies != 0 || status.MonitoringState != model.MonitoringLearning {
				t.Fatalf("stale completion changed new configuration: %+v", status)
			}
			if err = s.SaveModel(keyID, 8, "UTC", 7, monitoringModel(keyID, now), true, false, now); err != nil {
				t.Fatal(err)
			}
			completeActivationCheck(t, s, model.MonitoringRegular, 8, nil, now.Add(3*time.Minute))
			if activationStatus(t, s).DeletionBlocked {
				t.Fatal("new configuration could not activate after a valid new check")
			}
		})
	}
}

func TestDeferredResumeCannotBeBypassedByAnomalyExclusion(t *testing.T) {
	for _, kind := range []string{"quarantine", "purge"} {
		t.Run(kind, func(t *testing.T) {
			s, keyID, token, now := activationFixture(t)
			old := completedBackup(t, s, token, "exclusion-old-123456789", now.AddDate(0, 0, -30))
			completedBackup(t, s, token, "exclusion-new-123456789", now.AddDate(0, 0, -1))
			enableRetentionForTest(t, s, now)
			if kind == "purge" {
				at := now.Add(-72 * time.Hour)
				op, err := s.RequestQuarantine(old.ID, "manual", "admin", "fixture", at)
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
			for _, stableKey := range []string{"network-first", "network-second"} {
				if _, err := s.OpenAnomaly(stableKey, "upload", keyID, "", "network outage", now); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RequestRetentionResume(now); err != nil {
				t.Fatal(err)
			}
			incidents, err := s.Anomalies(true)
			if err != nil || len(incidents) != 2 {
				t.Fatalf("missing incidents: %+v %v", incidents, err)
			}
			if _, err = s.CreateAnomalyExclusion(incidents[0].ID, now.Add(-time.Hour), now.Add(time.Hour), "admin", "verified outage", []string{"upload"}, now); err != nil {
				t.Fatal(err)
			}
			status := activationStatus(t, s)
			if !status.ResumePending || !status.DeletionBlocked || status.ActivationRequired {
				t.Fatalf("exclusion bypassed pending resume: %+v", status)
			}
			request := func() error {
				if kind == "purge" {
					_, err := s.RequestPurge(old.ID, "automatic", "test", "pending resume", now)
					return err
				}
				_, err := s.RequestQuarantine(old.ID, "automatic", "test", "pending resume", now)
				return err
			}
			if err = request(); !errors.Is(err, model.ErrRetentionBlocked) {
				t.Fatalf("destructive work before a new valid check: %v", err)
			}
			completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now.Add(time.Minute))
			if status = activationStatus(t, s); status.ResumePending || status.DeletionBlocked {
				t.Fatalf("valid check could not consume pending resume: %+v", status)
			}
			if err = request(); err != nil {
				t.Fatalf("safe work after valid completion refused: %v", err)
			}
		})
	}
}

func TestDeferredResumePersistsAndIsConsumedOnlyOnce(t *testing.T) {
	s, _, _, now := activationFixture(t)
	if err := s.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseRetention("maintenance"); err != nil {
		t.Fatal(err)
	}
	completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now)
	if !activationStatus(t, s).ManualPaused {
		t.Fatal("initial activation cleared a manual pause")
	}
	now = now.Add(time.Hour)
	if err := s.ResumeRetention(now); err == nil {
		t.Fatal("stale immediate resume accepted")
	}
	// The next valid completion may belong to an already running check.
	check, err := s.StartMonitoringCheck(now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RequestRetentionResume(now); err != nil {
		t.Fatal(err)
	}
	status := activationStatus(t, s)
	if !status.ResumePending || !status.ManualPaused || !status.DeletionBlocked || status.ResumeRequestedAt != now.Unix() {
		t.Fatalf("request released a hold: %+v", status)
	}
	var seq int
	var name, path string
	if err = s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !activationStatus(t, s).ResumePending {
		t.Fatal("restart lost deferred resume")
	}
	now = now.Add(30 * time.Minute)
	if err = s.CompleteMonitoringCheck(check.ID, "completed_clean", "long check", model.MonitoringRegular, 7, nil, now); err != nil {
		t.Fatal(err)
	}
	status = activationStatus(t, s)
	if status.ResumePending || status.ManualPaused || status.DeletionBlocked || status.ResumeRequestedAt != 0 {
		t.Fatalf("deferred resume not consumed: %+v", status)
	}
	if err = s.PauseRetention("later pause"); err != nil {
		t.Fatal(err)
	}
	completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now.Add(time.Minute))
	status = activationStatus(t, s)
	if !status.DeletionBlocked || !status.ManualPaused || status.ManualPauseReason != "later pause" {
		t.Fatalf("consumed consent removed a later pause: %+v", status)
	}
}

func TestInvalidChecksCannotActivateRetention(t *testing.T) {
	for _, scenario := range []string{"unreliable", "review", "wrong revision", "missing model", "unmodeled key", "no keys", "failed", "obsolete", "replayed", "uncertain"} {
		t.Run(scenario, func(t *testing.T) {
			s, keyID, token, now := activationFixture(t)
			if err := s.MarkMailTested(now); err != nil {
				t.Fatal(err)
			}
			if scenario == "uncertain" {
				old := completedBackup(t, s, token, "uncertain-old-12345678", now.AddDate(0, 0, -30))
				completedBackup(t, s, token, "uncertain-new-12345678", now.AddDate(0, 0, -1))
				enableRetentionForTest(t, s, now)
				op, err := s.RequestQuarantine(old.ID, "manual", "admin", "uncertain", now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = authorizeOperationForTest(t, s, op.ID, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RequestRetentionResume(now); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "unreliable", "review", "wrong revision":
				revision := int64(7)
				if scenario == "wrong revision" {
					revision = 8
					if _, err := s.db.Exec(`DELETE FROM monitoring_models`); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.SaveModel(keyID, revision, "UTC", 7, monitoringModel(keyID, now), scenario != "unreliable", scenario == "review", now); err != nil {
					t.Fatal(err)
				}
			case "missing model":
				if _, err := s.db.Exec(`DELETE FROM monitoring_models`); err != nil {
					t.Fatal(err)
				}
			case "unmodeled key":
				if _, _, err := s.CreateKey("new key", "activation"); err != nil {
					t.Fatal(err)
				}
			case "no keys":
				if err := s.Revoke(keyID); err != nil {
					t.Fatal(err)
				}
			}
			check, err := s.StartMonitoringCheck(now)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "failed", "replayed":
				if err = s.FailMonitoringCheck(check.ID, "interrupted", now); err != nil {
					t.Fatal(err)
				}
			case "obsolete":
				if err = s.BeginRelearn(); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "failed" {
				err = s.CompleteMonitoringCheck(check.ID, "completed_clean", "test", model.MonitoringRegular, 7, nil, now)
				if (scenario == "obsolete" || scenario == "replayed") != (err != nil) {
					t.Fatalf("unexpected completion result: %v", err)
				}
			}
			if !activationStatus(t, s).DeletionBlocked {
				t.Fatal("unsafe check activated retention")
			}
		})
	}
}

func TestLaterAdministrativeActionsCancelDeferredResume(t *testing.T) {
	for _, action := range []string{"pause", "relearn", "new anomaly", "cancel quarantine"} {
		t.Run(action, func(t *testing.T) {
			s, _, token, now := activationFixture(t)
			enableRetentionForTest(t, s, now)
			var operation model.RetentionOperation
			if action == "cancel quarantine" {
				old := completedBackup(t, s, token, "cancel-old-123456789", now.AddDate(0, 0, -30))
				completedBackup(t, s, token, "cancel-new-123456789", now.AddDate(0, 0, -1))
				var err error
				operation, err = s.RequestQuarantine(old.ID, "manual", "admin", "test", now)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RequestRetentionResume(now); err != nil {
				t.Fatal(err)
			}
			var err error
			switch action {
			case "pause":
				err = s.PauseRetention("new pause")
			case "relearn":
				err = s.BeginRelearn()
			case "new anomaly":
				_, err = s.OpenAnomaly("new-integrity-incident", "catalog", "", "", "integrity issue", now)
			case "cancel quarantine":
				err = s.CancelQuarantineRequest(operation.BackupID, "admin", now)
			}
			if err != nil {
				t.Fatal(err)
			}
			status := activationStatus(t, s)
			if status.ResumePending || status.ResumeRequestedAt != 0 || !status.DeletionBlocked {
				t.Fatalf("later action did not cancel consent: %+v", status)
			}
		})
	}
}

func TestV11MigrationPreservesExistingChoices(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, paused := range []bool{false, true} {
			s, keyID, _, now := activationFixture(t)
			if err := s.SetKeyRetentionDays(keyID, 45); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE automation_state SET enabled=?,manual_paused=?,manual_pause_reason='existing hold',deletion_blocked=1,block_reason='existing block';
 ALTER TABLE automation_state DROP COLUMN mail_config_version; ALTER TABLE automation_state DROP COLUMN activation_required; ALTER TABLE automation_state DROP COLUMN activation_pending;
 ALTER TABLE automation_state DROP COLUMN resume_pending;
 ALTER TABLE automation_state DROP COLUMN resume_requested_at; PRAGMA user_version=10`, enabled, paused); err != nil {
				t.Fatal(err)
			}
			var seq int
			var name, path string
			if err := s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
				t.Fatal(err)
			}
			s.Close()
			ro, err := Open(path, true)
			if err != nil {
				t.Fatal(err)
			}
			if ro.schemaVersion != 10 {
				t.Fatal("read-only open migrated")
			}
			ro.Close()
			s, err = Open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			status := activationStatus(t, s)
			if status.Enabled != enabled || status.ManualPaused != paused || !status.DeletionBlocked || status.ActivationPending || status.ResumePending || status.RetentionDaysForKey(keyID) != 45 {
				t.Fatalf("upgrade changed choices: %+v", status)
			}
			if err = s.MarkMailTested(now); err != nil {
				t.Fatal(err)
			}
			completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, now)
			if !activationStatus(t, s).DeletionBlocked {
				t.Fatal("upgrade implicitly activated retention")
			}
			s.Close()
		}
	}
}

func TestV11MigrationRollsBackOnLateFailure(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE automation_state(id INTEGER PRIMARY KEY,resume_pending INTEGER); PRAGMA user_version=10`); err != nil {
		t.Fatal(err)
	}
	if err = migrateV10ToV11(db); err == nil {
		t.Fatal("malformed predecessor accepted")
	}
	var version, columns int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("failed migration advanced schema: %d %v", version, err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('automation_state') WHERE name='activation_pending'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("partial migration escaped rollback: %d %v", columns, err)
	}
}
