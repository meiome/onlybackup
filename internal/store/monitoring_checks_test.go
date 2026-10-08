package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

func monitoringModel(keyID string, learnedAt time.Time) policy.KeyModel {
	schedule := make(map[time.Weekday][]policy.Appointment)
	for day := time.Sunday; day <= time.Saturday; day++ {
		schedule[day] = []policy.Appointment{{MinuteOfDay: 2 * 60, MedianSize: 100}}
	}
	return policy.KeyModel{KeyID: keyID, Timezone: "UTC", LearnedAt: learnedAt, Schedule: schedule, Reliable: true}
}

func TestFirstMonitoringAttemptSurvivesInterruptionAndReopen(t *testing.T) {
	for _, phase := range []string{"before-plan", "after-plan", "failed", "new-revision"} {
		t.Run(phase, func(t *testing.T) {
			s, keyID, _ := setup(t, model.Profile{Name: "first-attempt", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
			firstAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
			learned := monitoringModel(keyID, firstAt.AddDate(0, 0, -14))
			if err := s.SaveModel(keyID, 7, "UTC", 7, learned, true, false, learned.LearnedAt); err != nil {
				t.Fatal(err)
			}
			if err := s.SetCheckState(model.MonitoringRegular, 7, firstAt); err != nil {
				t.Fatal(err)
			}
			first, err := s.StartMonitoringCheck(firstAt)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "before-plan" {
				if _, err = s.PlanMonitoringCheck(first.ID, firstAt); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "failed" {
				if err = s.FailMonitoringCheck(first.ID, "errore prima del completamento", firstAt); err != nil {
					t.Fatal(err)
				}
			}
			var seq int
			var name, path string
			if err = s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			revision := int64(7)
			changedAt := firstAt.AddDate(0, 0, 2)
			if phase == "new-revision" {
				revision = 8
				if err = s.BeginRelearn(); err != nil {
					t.Fatal(err)
				}
				newModel := monitoringModel(keyID, changedAt)
				if err = s.SaveModel(keyID, revision, "UTC", 7, newModel, true, false, changedAt); err != nil {
					t.Fatal(err)
				}
			}
			wantFrom := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC).Add(-time.Second).Unix()
			// Retry repeatedly, including after the 90-day history horizon.
			for _, days := range []int{5, 110} {
				at := firstAt.AddDate(0, 0, days)
				retry, err := s.StartMonitoringCheck(at)
				if err != nil {
					t.Fatal(err)
				}
				retry, err = s.PlanMonitoringCheck(retry.ID, at)
				if err != nil || retry.PeriodFrom != wantFrom {
					t.Fatalf("riferimento perso dopo %d giorni: %+v %v", days, retry, err)
				}
				if phase == "new-revision" && (len(retry.Models) != 2 || retry.Models[0].Revision != 7 || retry.Models[0].CoveredTo != changedAt.Unix() || retry.Models[1].CoveredFrom != changedAt.Unix()) {
					t.Fatalf("modello nuovo applicato al periodo precedente: %+v", retry.Models)
				}
				if days == 110 {
					if err = s.CompleteMonitoringCheck(retry.ID, "completed_clean", "ripreso", model.MonitoringRegular, revision, nil, at); err != nil {
						t.Fatal(err)
					}
					next, err := s.StartMonitoringCheck(at.Add(time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					next, err = s.PlanMonitoringCheck(next.ID, at.Add(time.Hour))
					if err != nil || next.PeriodFrom != retry.PeriodTo {
						t.Fatalf("copertura completata non utilizzata: %+v %v", next, err)
					}
				}
			}
		})
	}
}

func TestMonitoringCompletionRejectsConcurrentRelearnAtomically(t *testing.T) {
	for _, revision := range []int64{0, 7} {
		t.Run(strconv.FormatInt(revision, 10), func(t *testing.T) {
			s, keyID, _ := setup(t, model.Profile{Name: "relearn-race", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
			now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
			if err := s.SetCheckState(model.MonitoringRegular, revision, now); err != nil {
				t.Fatal(err)
			}
			check, err := s.StartMonitoringCheck(now)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.BeginRelearn(); err != nil {
				t.Fatal(err)
			}
			obsoleteRevision := revision
			if obsoleteRevision == 0 {
				obsoleteRevision = now.UnixNano()
			}
			findings := []policy.Finding{{StableKey: "stale-finding", Kind: "catalog", KeyID: keyID, Detail: "risultato obsoleto", At: now}}
			if err = s.CompleteMonitoringCheck(check.ID, "completed_with_anomalies", "obsoleto", model.MonitoringRegular, obsoleteRevision, findings, now); err == nil {
				t.Fatal("completamento obsoleto accettato")
			}
			status, err := s.AutomationStatus()
			if err != nil || status.ModelRevision != revision+1 || status.MonitoringState != model.MonitoringLearning || !status.DeletionBlocked || status.BlockReason != "nuovo apprendimento richiesto" {
				t.Fatalf("richiesta relearn sovrascritta: %+v %v", status, err)
			}
			anomalies, err := s.Anomalies(true)
			if err != nil || len(anomalies) != 0 {
				t.Fatalf("risultati obsoleti registrati: %+v %v", anomalies, err)
			}
			checks, err := s.MonitoringChecks(1)
			if err != nil || checks[0].Status != "running" {
				t.Fatalf("copertura obsoleta completata: %+v %v", checks, err)
			}
			if err = s.FailMonitoringCheck(check.ID, "modello cambiato", now); err != nil {
				t.Fatal(err)
			}
			retry, err := s.StartMonitoringCheck(now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.CompleteMonitoringCheck(retry.ID, "completed_clean", "nuovo modello", model.MonitoringRegular, revision+1, nil, now.Add(time.Minute)); err != nil {
				t.Fatalf("controllo con revisione corrente rifiutato: %v", err)
			}
		})
	}
}

func TestMonitoringCheckContinuityAndAtomicCompletion(t *testing.T) {
	s, keyID, _ := setup(t, model.Profile{Name: "checks", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	learned := monitoringModel(keyID, now.AddDate(0, 0, -14))
	if err := s.SaveModel(keyID, 7, "UTC", 7, learned, true, false, learned.LearnedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	first, err := s.StartMonitoringCheck(now)
	if err != nil {
		t.Fatal(err)
	}
	first, err = s.PlanMonitoringCheck(first.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	wantFrom := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC).Add(-time.Second).Unix()
	wantTo := now.Add(-policy.ScheduleTolerance).Unix()
	if len(first.Models) != 1 || first.Models[0].CoveredFrom != wantFrom || first.Models[0].CoveredTo != wantTo || first.Models[0].Revision != 7 {
		t.Fatalf("prima copertura inattesa: %+v", first)
	}
	if err = s.CompleteMonitoringCheck(first.ID, "completed_clean", "nessuna anomalia", model.MonitoringRegular, 7, nil, now); err != nil {
		t.Fatal(err)
	}

	interruptedAt := now.AddDate(0, 0, 5)
	interrupted, err := s.StartMonitoringCheck(interruptedAt)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err = s.PlanMonitoringCheck(interrupted.ID, interruptedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted.Models) != 1 || interrupted.Models[0].CoveredFrom != wantTo {
		t.Fatalf("il recupero non riparte dal limite completato: %+v", interrupted)
	}
	retry, err := s.StartMonitoringCheck(interruptedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	retry, err = s.PlanMonitoringCheck(retry.ID, interruptedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Models) != 1 || retry.Models[0].CoveredFrom != wantTo {
		t.Fatalf("il tentativo interrotto ha avanzato la copertura: %+v", retry)
	}
	finding := policy.Finding{StableKey: "extra:retry", Kind: "extra", KeyID: keyID, BackupID: "retry", Detail: "copia eccedente", At: interruptedAt}
	if err = s.CompleteMonitoringCheck(retry.ID, "completed_with_anomalies", "1 anomalie: extra=1", model.MonitoringRegular, 7, []policy.Finding{finding}, interruptedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	checks, err := s.MonitoringChecks(20)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 3 || checks[0].Status != "completed_with_anomalies" || checks[1].Status != "failed" || checks[2].Status != "completed_clean" {
		t.Fatalf("storico tentativi inatteso: %+v", checks)
	}
	anomalies, err := s.Anomalies(true)
	if err != nil || len(anomalies) != 1 || anomalies[0].StableKey != finding.StableKey {
		t.Fatalf("anomalia non registrata atomicamente: %+v %v", anomalies, err)
	}
	var storedState string
	if err = s.db.QueryRow(`SELECT monitoring_state FROM automation_state WHERE id=1`).Scan(&storedState); err != nil || storedState != model.MonitoringRegular {
		t.Fatalf("extra non bloccante ha alterato lo stato: %s %v", storedState, err)
	}
}

func TestMonitoringCompletionRollsBackFindingsWhenItCannotFinish(t *testing.T) {
	s, keyID, _ := setup(t, model.Profile{Name: "check-atomic", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	learned := monitoringModel(keyID, now.AddDate(0, 0, -14))
	if err := s.SaveModel(keyID, 3, "UTC", 7, learned, true, false, learned.LearnedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCheckState(model.MonitoringRegular, 3, now); err != nil {
		t.Fatal(err)
	}
	check, err := s.StartMonitoringCheck(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PlanMonitoringCheck(check.ID, now); err != nil {
		t.Fatal(err)
	}
	findings := []policy.Finding{
		{StableKey: "extra:rolled-back", Kind: "extra", KeyID: keyID, Detail: "valida", At: now},
		{StableKey: "invalid", Kind: "catalog", KeyID: keyID, Detail: "", At: now},
	}
	if err = s.CompleteMonitoringCheck(check.ID, "completed_with_anomalies", "non deve restare", model.MonitoringRegular, 3, findings, now); err == nil {
		t.Fatal("completamento incoerente accettato")
	}
	anomalies, queryErr := s.Anomalies(true)
	if queryErr != nil || len(anomalies) != 0 {
		t.Fatalf("anomalia parziale sopravvissuta al rollback: %+v %v", anomalies, queryErr)
	}
	checks, queryErr := s.MonitoringChecks(1)
	if queryErr != nil || len(checks) != 1 || checks[0].Status != "running" {
		t.Fatalf("tentativo avanzato senza commit completo: %+v %v", checks, queryErr)
	}
}

func TestMonitoringCheckRetentionKeepsContinuityCheckpoint(t *testing.T) {
	s, keyID, _ := setup(t, model.Profile{Name: "check-retention", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	firstAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	learned := monitoringModel(keyID, firstAt.AddDate(0, 0, -14))
	if err := s.SaveModel(keyID, 9, "UTC", 7, learned, true, false, learned.LearnedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCheckState(model.MonitoringRegular, 9, firstAt); err != nil {
		t.Fatal(err)
	}
	complete := func(at time.Time) model.MonitoringCheck {
		t.Helper()
		check, err := s.StartMonitoringCheck(at)
		if err != nil {
			t.Fatal(err)
		}
		check, err = s.PlanMonitoringCheck(check.ID, at)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.CompleteMonitoringCheck(check.ID, "completed_clean", "nessuna anomalia", model.MonitoringRegular, 9, nil, at); err != nil {
			t.Fatal(err)
		}
		return check
	}
	_ = complete(firstAt)
	checkpoint := complete(firstAt.AddDate(0, 0, 100))
	checks, err := s.MonitoringChecks(20)
	if err != nil || len(checks) != 1 || checks[0].ID != checkpoint.ID {
		t.Fatalf("pulizia 90 giorni non ha preservato solo il checkpoint: %+v %v", checks, err)
	}
	afterLongStop := firstAt.AddDate(0, 0, 220)
	check, err := s.StartMonitoringCheck(afterLongStop)
	if err != nil {
		t.Fatal(err)
	}
	check, err = s.PlanMonitoringCheck(check.ID, afterLongStop)
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Models) != 1 || check.Models[0].CoveredFrom != checkpoint.PeriodTo {
		t.Fatalf("fermo oltre 90 giorni ha perso il punto di ripresa: %+v", check)
	}
}

func TestMonitoringPlanSplitsRelearnAndStartsNewKeysAtFirstModel(t *testing.T) {
	s, firstKeyID, _ := setup(t, model.Profile{Name: "check-models", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	oldModel := monitoringModel(firstKeyID, base.AddDate(0, 0, -14))
	if err := s.SaveModel(firstKeyID, 7, "UTC", 7, oldModel, true, false, oldModel.LearnedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCheckState(model.MonitoringRegular, 7, base); err != nil {
		t.Fatal(err)
	}
	initial, err := s.StartMonitoringCheck(base)
	if err != nil {
		t.Fatal(err)
	}
	initial, err = s.PlanMonitoringCheck(initial.ID, base)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteMonitoringCheck(initial.ID, "completed_clean", "nessuna anomalia", model.MonitoringRegular, 7, nil, base); err != nil {
		t.Fatal(err)
	}

	newKey, _, err := s.CreateKey("new-check-key", "XS")
	if err != nil {
		t.Fatal(err)
	}
	newRevisionAt := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	newModel := monitoringModel(firstKeyID, newRevisionAt)
	newKeyModelAt := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	newKeyModel := monitoringModel(newKey.ID, newKeyModelAt)
	if err = s.SaveModel(firstKeyID, 8, "UTC", 7, newModel, true, false, newRevisionAt); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveModel(newKey.ID, 8, "UTC", 7, newKeyModel, true, false, newKeyModelAt); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if err = s.SetCheckState(model.MonitoringRegular, 8, now); err != nil {
		t.Fatal(err)
	}
	check, err := s.StartMonitoringCheck(now)
	if err != nil {
		t.Fatal(err)
	}
	check, err = s.PlanMonitoringCheck(check.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(check.Models) != 3 {
		t.Fatalf("segmenti modello inattesi: %+v", check.Models)
	}
	byKeyRevision := make(map[string]model.MonitoringCheckModel)
	for _, scope := range check.Models {
		byKeyRevision[scope.KeyID+":"+strconv.FormatInt(scope.Revision, 10)] = scope
	}
	oldScope := byKeyRevision[firstKeyID+":7"]
	newScope := byKeyRevision[firstKeyID+":8"]
	newKeyScope := byKeyRevision[newKey.ID+":8"]
	if oldScope.CoveredTo != newRevisionAt.Unix() || newScope.CoveredFrom != newRevisionAt.Unix() {
		t.Fatalf("relearn non separato al confine di validita: old=%+v new=%+v", oldScope, newScope)
	}
	if newKeyScope.CoveredFrom != newKeyModelAt.Unix() {
		t.Fatalf("nuova chiave dichiarata coperta prima del modello: %+v", newKeyScope)
	}
	if err = s.Revoke(firstKeyID); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteMonitoringCheck(check.ID, "completed_clean", "nessuna anomalia", model.MonitoringRegular, 8, nil, now); err != nil {
		t.Fatal(err)
	}
	afterRevoke, err := s.StartMonitoringCheck(now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	afterRevoke, err = s.PlanMonitoringCheck(afterRevoke.ID, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range afterRevoke.Models {
		if scope.KeyID == firstKeyID {
			t.Fatalf("chiave revocata ancora pianificata: %+v", afterRevoke.Models)
		}
	}
}

func TestVersionSixMigrationAddsEmptyMonitoringHistory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "metadata.db")
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := s.CreateKey("v6", "XS")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	learned := monitoringModel(key.ID, now.AddDate(0, 0, -14))
	if err = s.SaveModel(key.ID, 4, "UTC", 7, learned, true, false, learned.LearnedAt); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`ALTER TABLE automation_state DROP COLUMN mail_config_version; ALTER TABLE automation_state DROP COLUMN activation_required; ALTER TABLE automation_state DROP COLUMN activation_pending; ALTER TABLE automation_state DROP COLUMN resume_pending; ALTER TABLE automation_state DROP COLUMN resume_requested_at; DROP TABLE key_retention; ALTER TABLE automation_state DROP COLUMN manual_paused; ALTER TABLE automation_state DROP COLUMN manual_pause_reason; DROP TABLE monitoring_check_models; DROP TABLE monitoring_checks;
ALTER TABLE retention_operations DROP COLUMN execution_committed;
ALTER TABLE retention_operations DROP COLUMN lease_generation;
ALTER TABLE maintenance_leases DROP COLUMN generation;
PRAGMA user_version=6`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var version, checks, models int
	if err = migrated.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 12 {
		t.Fatalf("versione migrata: %d %v", version, err)
	}
	if err = migrated.db.QueryRow(`SELECT COUNT(*) FROM monitoring_checks`).Scan(&checks); err != nil || checks != 0 {
		t.Fatalf("la migrazione ha inventato controlli: %d %v", checks, err)
	}
	if err = migrated.db.QueryRow(`SELECT COUNT(*) FROM monitoring_models WHERE key_id=?`, key.ID).Scan(&models); err != nil || models != 1 {
		t.Fatalf("modello v6 non preservato: %d %v", models, err)
	}
	for _, pragma := range []string{"integrity_check", "foreign_key_check"} {
		rows, queryErr := migrated.db.Query("PRAGMA " + pragma)
		if queryErr != nil {
			t.Fatal(queryErr)
		}
		var values []string
		for rows.Next() {
			var value string
			if queryErr = rows.Scan(&value); queryErr != nil {
				t.Fatal(queryErr)
			}
			values = append(values, value)
		}
		rows.Close()
		if pragma == "integrity_check" && (len(values) != 1 || values[0] != "ok") || pragma == "foreign_key_check" && len(values) != 0 {
			data, _ := json.Marshal(values)
			t.Fatalf("PRAGMA %s: %s", pragma, data)
		}
	}
}
