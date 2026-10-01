package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

func enableRetentionForTest(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	if err := s.SetupAutomation(MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableAutomation(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}
}

func leaseForTest(t *testing.T, s *Store, generation int64, now time.Time) model.MaintenanceLease {
	t.Helper()
	const owner = "store-test-maintenance"
	if generation == 0 {
		var current model.MaintenanceLease
		if err := s.db.QueryRow(`SELECT owner,generation,expires_at FROM maintenance_leases WHERE id=1`).Scan(&current.Owner, &current.Generation, &current.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		if current.Owner == owner && current.ExpiresAt > now.Unix() {
			generation = current.Generation
		}
	}
	lease, err := s.AcquireMaintenanceLease(owner, generation, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func authorizeOperationForTest(t *testing.T, s *Store, id int64, now time.Time) (model.RetentionOperation, error) {
	t.Helper()
	return s.AuthorizeOperation(id, leaseForTest(t, s, 0, now), now)
}

func validateOperationForTest(t *testing.T, s *Store, id int64, ticket string, now time.Time) error {
	t.Helper()
	op, err := s.Operation(id)
	if err != nil {
		return err
	}
	return s.ValidateOperation(id, ticket, leaseForTest(t, s, op.LeaseGeneration, now), now)
}

func confirmOperationForTest(t *testing.T, s *Store, id int64, ticket string, done bool, detail string, now time.Time) error {
	t.Helper()
	op, err := s.Operation(id)
	if err != nil {
		return err
	}
	return s.ConfirmOperation(id, ticket, done, detail, leaseForTest(t, s, op.LeaseGeneration, now), now)
}

func completedBackup(t *testing.T, s *Store, token, idem string, at time.Time) model.Backup {
	t.Helper()
	b, err := s.ReserveIdempotent(token, model.Metadata{Description: "db", OriginalName: "db.sql"}, 100, strings.Repeat("a", 64), idem, at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(b.ID, at); err != nil {
		t.Fatal(err)
	}
	b, err = s.Backup(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRetentionLifecycleMinimumQuarantineAndQuota(t *testing.T) {
	s, keyID, token := setup(t, model.Profile{Name: "retention", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	old := completedBackup(t, s, token, "obi_old_backup_123456789", now.AddDate(0, 0, -10))
	today := completedBackup(t, s, token, "obi_today_backup_123456", now.Add(-time.Hour))
	enableRetentionForTest(t, s, now)
	if _, err := s.RequestQuarantine(today.ID, "manual", "tester", "test", now); !errors.Is(err, model.ErrCurrentDay) {
		t.Fatalf("current day accepted: %v", err)
	}
	op, err := s.RequestQuarantine(old.ID, "manual", "tester", "test", now)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := authorizeOperationForTest(t, s, op.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateOperationForTest(t, s, op.ID, authorized.Ticket, now); err != nil {
		t.Fatal(err)
	}
	if err = confirmOperationForTest(t, s, op.ID, "wrong-ticket", true, "", now); !errors.Is(err, model.ErrTicket) {
		t.Fatalf("false ticket accepted: %v", err)
	}
	if err = confirmOperationForTest(t, s, op.ID, authorized.Ticket, true, "", now); err != nil {
		t.Fatal(err)
	}
	if err = confirmOperationForTest(t, s, op.ID, "wrong-ticket", true, "", now); !errors.Is(err, model.ErrTicket) {
		t.Fatalf("ticket errato accettato dopo conferma: %v", err)
	}
	stored, _ := s.Backup(old.ID)
	if stored.Status != model.BackupQuarantined || stored.PurgeNotBefore != now.Add(48*time.Hour).Unix() {
		t.Fatal(stored)
	}
	if _, err = s.RequestPurge(old.ID, "automatic", "maintenance", "test", now.Add(48*time.Hour-time.Minute)); !errors.Is(err, model.ErrTooEarly) {
		t.Fatalf("early purge accepted: %v", err)
	}
	purge, err := s.RequestPurge(old.ID, "automatic", "maintenance", "test", now.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	purgeAuthorized, err := authorizeOperationForTest(t, s, purge.ID, now.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	quota, _ := s.Quota(keyID, now)
	if quota.Used != 200 {
		t.Fatalf("quota released before purge: %+v", quota)
	}
	if err = confirmOperationForTest(t, s, purge.ID, purgeAuthorized.Ticket, true, "", now.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	quota, _ = s.Quota(keyID, now)
	if quota.Used != 100 {
		t.Fatalf("quota not released after purge: %+v", quota)
	}
	if _, err = s.ReserveIdempotent(token, old.Metadata, old.Size, old.SHA256, old.IdempotencyKey, now); !errors.Is(err, model.ErrIdempotencyUnavailable) {
		t.Fatalf("retained idempotency replay accepted: %v", err)
	}
}

func TestMaintenanceLeaseGenerationInvalidatesOldWorkerAndTicket(t *testing.T) {
	s, _, token := setup(t, model.Profile{Name: "lease-generation", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	old := completedBackup(t, s, token, "obi_lease_old_123456789", now.AddDate(0, 0, -10))
	_ = completedBackup(t, s, token, "obi_lease_new_123456789", now.AddDate(0, 0, -1))
	enableRetentionForTest(t, s, now)

	first, err := s.AcquireMaintenanceLease("worker", 0, now, 10*time.Minute)
	if err != nil || first.Generation <= 0 {
		t.Fatalf("prima lease: %+v %v", first, err)
	}
	if _, err = s.AcquireMaintenanceLease("worker", 0, now.Add(time.Minute), 10*time.Minute); !errors.Is(err, model.ErrLease) {
		t.Fatalf("riavvio con lo stesso owner entrato nella lease attiva: %v", err)
	}
	if _, err = s.AcquireMaintenanceLease("other", 0, now.Add(time.Minute), 10*time.Minute); !errors.Is(err, model.ErrLease) {
		t.Fatalf("secondo worker entrato nella lease attiva: %v", err)
	}
	op, err := s.RequestQuarantine(old.ID, "automatic", "worker", "test generation", now)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := s.AuthorizeOperation(op.ID, first, now)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := s.AcquireMaintenanceLease("worker", first.Generation, now.Add(5*time.Minute), 10*time.Minute)
	if err != nil || renewed.Generation != first.Generation || renewed.ExpiresAt != now.Add(15*time.Minute).Unix() {
		t.Fatalf("rinnovo ha cambiato identita o scadenza: %+v %v", renewed, err)
	}
	if err = s.ValidateOperation(op.ID, authorized.Ticket, renewed, now.Add(14*time.Minute)); err != nil {
		t.Fatalf("ticket rifiutato dalla propria lease rinnovata: %v", err)
	}

	second, err := s.AcquireMaintenanceLease("other", 0, now.Add(15*time.Minute), 10*time.Minute)
	if err != nil || second.Generation != first.Generation+1 {
		t.Fatalf("nuova generazione alla scadenza: %+v %v", second, err)
	}
	if err = s.ValidateOperation(op.ID, authorized.Ticket, first, now.Add(15*time.Minute)); !errors.Is(err, model.ErrLease) {
		t.Fatalf("lease scaduta ancora valida: %v", err)
	}
	if err = s.ValidateOperation(op.ID, authorized.Ticket, second, now.Add(15*time.Minute)); !errors.Is(err, model.ErrTicket) {
		t.Fatalf("ticket della vecchia generazione accettato dalla nuova: %v", err)
	}
	if err = s.ConfirmOperation(op.ID, authorized.Ticket, true, "", first, now.Add(15*time.Minute)); !errors.Is(err, model.ErrLease) {
		t.Fatalf("vecchio worker ha confermato dopo il subentro: %v", err)
	}
	if err = s.ReleaseMaintenanceLease(first); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateMaintenanceLease(second, now.Add(15*time.Minute)); err != nil {
		t.Fatalf("rilascio vecchia generazione ha tolto la lease nuova: %v", err)
	}
}

func TestAnomalyInvalidatesAuthorizedDestruction(t *testing.T) {
	s, _, token := setup(t, model.Profile{Name: "race-block", TotalBytes: 2000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	old := completedBackup(t, s, token, "obi_race_old_12345678901", now.AddDate(0, 0, -10))
	_ = completedBackup(t, s, token, "obi_race_new_12345678901", now.AddDate(0, 0, -1))
	enableRetentionForTest(t, s, now)
	op, err := s.RequestQuarantine(old.ID, "automatic", "maintenance", "test", now)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := authorizeOperationForTest(t, s, op.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.OpenAnomaly("new-problem", "catalog", "", "", "nuova anomalia", now); err != nil {
		t.Fatal(err)
	}
	if err = validateOperationForTest(t, s, op.ID, authorized.Ticket, now); !errors.Is(err, model.ErrRetentionBlocked) {
		t.Fatalf("operation survived new anomaly: %v", err)
	}
}

func TestResolvedMaintenanceFailureClearsOnlyItsOwnBlock(t *testing.T) {
	setupFailure := func(t *testing.T) (*Store, model.RetentionOperation, time.Time) {
		t.Helper()
		s, _, token := setup(t, model.Profile{Name: "maintenance-recovery", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
		now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
		old := completedBackup(t, s, token, "obi_failure_old_12345678", now.AddDate(0, 0, -10))
		_ = completedBackup(t, s, token, "obi_failure_new_12345678", now.AddDate(0, 0, -1))
		enableRetentionForTest(t, s, now)
		op, err := s.RequestQuarantine(old.ID, "automatic", "worker", "transient failure", now)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := s.AcquireMaintenanceLease("worker", 0, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		authorized, err := s.AuthorizeOperation(op.ID, lease, now)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.ConfirmOperation(op.ID, authorized.Ticket, false, "errore temporaneo", lease, now); err != nil {
			t.Fatal(err)
		}
		if err = s.ResetAuthorized(op.ID, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		return s, op, now.Add(2 * time.Minute)
	}

	t.Run("regular-check-clears", func(t *testing.T) {
		s, op, now := setupFailure(t)
		if err := s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
			t.Fatal(err)
		}
		cleared, err := s.TryClearResolvedMaintenanceBlock(op.ID, now)
		if err != nil || !cleared {
			t.Fatalf("blocco transitorio non rimosso: %t %v", cleared, err)
		}
		status, err := s.AutomationStatus()
		if err != nil || status.DeletionBlocked || status.MonitoringState != model.MonitoringRegular {
			t.Fatalf("stato dopo recupero: %+v %v", status, err)
		}
	})

	t.Run("manual-pause-is-preserved", func(t *testing.T) {
		s, op, now := setupFailure(t)
		if err := s.PauseRetention("manutenzione amministrativa"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
			t.Fatal(err)
		}
		cleared, err := s.TryClearResolvedMaintenanceBlock(op.ID, now)
		if err != nil || cleared {
			t.Fatalf("pausa amministrativa rimossa: %t %v", cleared, err)
		}
		status, err := s.AutomationStatus()
		if err != nil || !status.DeletionBlocked || status.BlockReason != "manutenzione amministrativa" {
			t.Fatalf("pausa non conservata: %+v %v", status, err)
		}
	})

	t.Run("cancellation-is-preserved", func(t *testing.T) {
		s, op, now := setupFailure(t)
		stored, err := s.Operation(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.CancelQuarantineRequest(stored.BackupID, "admin", now); err != nil {
			t.Fatal(err)
		}
		if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
			t.Fatal(err)
		}
		cleared, err := s.TryClearResolvedMaintenanceBlock(op.ID, now)
		if err != nil || cleared {
			t.Fatalf("blocco da annullamento rimosso: %t %v", cleared, err)
		}
		status, err := s.AutomationStatus()
		if err != nil || !status.DeletionBlocked || !strings.Contains(status.BlockReason, "richiesta annullata") {
			t.Fatalf("annullamento non conservato: %+v %v", status, err)
		}
	})
}

func TestAnomalyResolutionKeepsPersistentBlock(t *testing.T) {
	s, _, _ := setup(t, model.Profile{Name: "anomaly", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	enableRetentionForTest(t, s, now)
	created, err := s.OpenAnomaly("missing:key:day", "file_missing", "", "", "backup mancante", now)
	if err != nil || !created {
		t.Fatal(created, err)
	}
	created, err = s.OpenAnomaly("missing:key:day", "file_missing", "", "", "ancora mancante", now.Add(5*time.Minute))
	if err != nil || created {
		t.Fatal(created, err)
	}
	mail, err := s.DueMail(now.Add(6*time.Minute), 20)
	if err != nil || len(mail) != 1 {
		t.Fatalf("new anomaly mail count: %d %v", len(mail), err)
	}
	if _, err = s.ResolveAnomaly("missing:key:day", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	status, _ := s.AutomationStatus()
	if !status.DeletionBlocked || status.MonitoringState != model.MonitoringPaused {
		t.Fatal(status)
	}
	if err = s.ResumeRetention(now.Add(time.Hour)); err == nil {
		t.Fatal("resume accepted without a new full check")
	}
	if err = s.SetCheckState(model.MonitoringRegular, 7, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = s.ResumeRetention(now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	status, _ = s.AutomationStatus()
	if status.DeletionBlocked {
		t.Fatal(status)
	}
}

func TestCompletedReconciliationResolvesOnlyRelatedAnomaliesAtomically(t *testing.T) {
	for _, scenario := range []string{"regular", "other-anomaly", "pause", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			s, key, token := setup(t, model.Profile{Name: "reconcile-related", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
			now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
			old := completedBackup(t, s, token, "reconcile-related-old", now.AddDate(0, 0, -10))
			completedBackup(t, s, token, "reconcile-related-new", now.AddDate(0, 0, -1))
			enableRetentionForTest(t, s, now)
			op, err := s.RequestQuarantine(old.ID, "manual", "admin", "test", now)
			if err != nil {
				t.Fatal(err)
			}
			auth, err := authorizeOperationForTest(t, s, op.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			if err = confirmOperationForTest(t, s, op.ID, auth.Ticket, false, "fsync fallito", now); err != nil {
				t.Fatal(err)
			}
			if _, err = s.OpenAnomaly(fmt.Sprintf("operation-uncertain:%d", op.ID), "maintenance", key, old.ID, "esito incerto", now); err != nil {
				t.Fatal(err)
			}
			if scenario == "other-anomaly" {
				if _, err = s.OpenAnomaly("unrelated-integrity", "catalog", key, "", "integrita indipendente", now); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "pause" {
				if err = s.PauseRetention("pausa indipendente"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "rollback" {
				if _, err = s.db.Exec(`CREATE TRIGGER fail_resolution BEFORE UPDATE OF resolved_at ON anomalies
 BEGIN SELECT RAISE(ABORT,'errore simulato nella chiusura'); END`); err != nil {
					t.Fatal(err)
				}
				if err = s.ReconcileAuthorized(op.ID, now); err == nil {
					t.Fatal("errore transazionale ignorato")
				}
				current, err := s.Operation(op.ID)
				if err != nil || current.State != "authorized" {
					t.Fatalf("conferma parziale: %+v %v", current, err)
				}
				backup, err := s.Backup(old.ID)
				if err != nil || backup.Status != model.BackupDeleting {
					t.Fatalf("catalogo parzialmente aggiornato: %+v %v", backup, err)
				}
				active, err := s.Anomalies(true)
				if err != nil || len(active) != 2 {
					t.Fatalf("anomalie parzialmente chiuse: %+v %v", active, err)
				}
				return
			}
			if err = s.ReconcileAuthorized(op.ID, now); err != nil {
				t.Fatal(err)
			}
			if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
				t.Fatal(err)
			}
			cleared, err := s.TryClearResolvedMaintenanceBlock(op.ID, now)
			if err != nil || cleared != (scenario == "regular") {
				t.Fatalf("blocco rimosso=%t scenario=%s err=%v", cleared, scenario, err)
			}
			active, err := s.Anomalies(true)
			want := 0
			if scenario == "other-anomaly" {
				want = 1
			}
			if err != nil || len(active) != want {
				t.Fatalf("anomalie residue: %+v %v", active, err)
			}
		})
	}
}

func TestMissingAnomalyRequiresExplicitAcknowledgement(t *testing.T) {
	s, _, _ := setup(t, model.Profile{Name: "missing-ack", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	enableRetentionForTest(t, s, now)
	checkAt := now.Add(24 * time.Hour)
	if err := s.SetCheckState(model.MonitoringRegular, 7, checkAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAnomaly("missing:key:day", "file_missing", "key", "backup", "backup mancante", checkAt); err != nil {
		t.Fatal(err)
	}
	anomalies, err := s.Anomalies(true)
	if err != nil || len(anomalies) != 1 {
		t.Fatalf("active missing anomaly: %v %v", anomalies, err)
	}
	status, err := s.AutomationStatus()
	if err != nil || status.MonitoringState != model.MonitoringAnomaly {
		t.Fatalf("stato anomalia: %+v %v", status, err)
	}
	if err = s.AcknowledgeMissingAnomaly(anomalies[0].ID, checkAt.Add(11*time.Minute)); err == nil {
		t.Fatal("stale regular check accepted")
	}
	if err = s.AcknowledgeMissingAnomaly(anomalies[0].ID, checkAt); err != nil {
		t.Fatal(err)
	}
	acknowledged, err := s.AcknowledgedMissingBackupIDs()
	if err != nil || len(acknowledged) != 1 || acknowledged[0] != "backup" {
		t.Fatalf("missing acknowledgement not persisted: %v %v", acknowledged, err)
	}
	status, err = s.AutomationStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.DeletionBlocked || status.MonitoringState != model.MonitoringPaused || status.ActiveAnomalies != 0 {
		t.Fatal(status)
	}
	if err = s.SetCheckState(model.MonitoringRegular, 7, checkAt); err != nil {
		t.Fatal(err)
	}
	if err = s.ResumeRetention(checkAt); err != nil {
		t.Fatal(err)
	}
}

func TestLoadModelsConvertsStoredTextToJSON(t *testing.T) {
	s, keyID, _ := setup(t, model.Profile{Name: "models", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	learned := policy.KeyModel{KeyID: keyID, Timezone: "UTC", LearnedAt: now, Reliable: true}
	if err := s.SaveModel(keyID, 7, "UTC", 7, learned, true, false, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	models, err := s.LoadModels()
	if err != nil {
		t.Fatal(err)
	}
	var loaded policy.KeyModel
	if raw, ok := models[keyID]; !ok {
		t.Fatalf("modello assente: %v", models)
	} else if err = json.Unmarshal(raw, &loaded); err != nil || loaded.KeyID != keyID || !loaded.Reliable {
		t.Fatalf("modello non valido: %+v %v", loaded, err)
	}
}

func TestAcknowledgedMissingDoesNotCountAsLastRecoverableCopy(t *testing.T) {
	s, _, token := setup(t, model.Profile{Name: "missing-copy-count", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	missing := completedBackup(t, s, token, "obi_missing_copy_123456789", now.AddDate(0, 0, -2))
	last := completedBackup(t, s, token, "obi_last_real_copy_1234567", now.AddDate(0, 0, -1))
	enableRetentionForTest(t, s, now)
	if _, err := s.OpenAnomaly("file-missing:"+missing.ID, "file_missing", missing.KeyID, missing.ID, "backup mancante", now); err != nil {
		t.Fatal(err)
	}
	anomalies, err := s.Anomalies(true)
	if err != nil || len(anomalies) != 1 {
		t.Fatalf("active missing anomaly: %v %v", anomalies, err)
	}
	if err = s.AcknowledgeMissingAnomaly(anomalies[0].ID, now); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RequestQuarantine(last.ID, "manual", "tester", "test", now); !errors.Is(err, model.ErrLastCopy) {
		t.Fatalf("last recoverable copy accepted: %v", err)
	}
}

func TestAcknowledgedMissingQuarantinedBackupCannotBePurged(t *testing.T) {
	s, _, token := setup(t, model.Profile{Name: "missing-purge-target", TotalBytes: 2000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	victim := completedBackup(t, s, token, "obi_missing_purge_12345678", now.AddDate(0, 0, -11))
	_ = completedBackup(t, s, token, "obi_valid_purge_1_12345678", now.AddDate(0, 0, -2))
	_ = completedBackup(t, s, token, "obi_valid_purge_2_12345678", now.AddDate(0, 0, -1))
	enableRetentionForTest(t, s, now)
	quarantinedAt := now.Add(-72 * time.Hour)
	quarantine, err := s.RequestQuarantine(victim.ID, "manual", "tester", "fixture quarantine", quarantinedAt)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := authorizeOperationForTest(t, s, quarantine.ID, quarantinedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err = confirmOperationForTest(t, s, quarantine.ID, authorized.Ticket, true, "", quarantinedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = s.OpenAnomaly("file-missing:"+victim.ID, "file_missing", victim.KeyID, victim.ID, "backup mancante", now); err != nil {
		t.Fatal(err)
	}
	anomalies, err := s.Anomalies(true)
	if err != nil || len(anomalies) != 1 {
		t.Fatalf("anomalia file_missing: %+v %v", anomalies, err)
	}
	if err = s.AcknowledgeMissingAnomaly(anomalies[0].ID, now); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}

	var operationsBefore, mailBefore int
	var acknowledgedBefore int64
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM retention_operations`).Scan(&operationsBefore); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM mail_queue`).Scan(&mailBefore); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT acknowledged_at FROM anomalies WHERE id=?`, anomalies[0].ID).Scan(&acknowledgedBefore); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"manual", "automatic", "reconcile"} {
		t.Run(origin, func(t *testing.T) {
			_, requestErr := s.RequestPurge(victim.ID, origin, "tester", "must be rejected", now)
			if requestErr == nil || requestErr.Error() != "backup riconosciuto come definitivamente mancante" {
				t.Fatalf("errore purge inatteso: %v", requestErr)
			}
			stored, backupErr := s.Backup(victim.ID)
			if backupErr != nil || stored.Status != model.BackupQuarantined {
				t.Fatalf("stato backup modificato: %+v %v", stored, backupErr)
			}
		})
	}
	var operationsAfter, mailAfter int
	var acknowledgedAfter int64
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM retention_operations`).Scan(&operationsAfter); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM mail_queue`).Scan(&mailAfter); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT acknowledged_at FROM anomalies WHERE id=?`, anomalies[0].ID).Scan(&acknowledgedAfter); err != nil {
		t.Fatal(err)
	}
	if operationsAfter != operationsBefore || mailAfter != mailBefore || acknowledgedAfter != acknowledgedBefore {
		t.Fatalf("rifiuto purge con effetti collaterali: operations %d->%d, mail %d->%d, acknowledgement %d->%d", operationsBefore, operationsAfter, mailBefore, mailAfter, acknowledgedBefore, acknowledgedAfter)
	}
}

func TestLastAvailableCopyCannotBeRequested(t *testing.T) {
	s, _, token := setup(t, model.Profile{Name: "last", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	b := completedBackup(t, s, token, "obi_last_backup_123456789", now.AddDate(0, 0, -1))
	enableRetentionForTest(t, s, now)
	if _, err := s.RequestQuarantine(b.ID, "manual", "tester", "test", now); !errors.Is(err, model.ErrLastCopy) {
		t.Fatalf("last copy accepted: %v", err)
	}
}

func TestCompletionDetectsAnomalyBeforeDestructiveAuthorization(t *testing.T) {
	s, keyID, token := setup(t, model.Profile{Name: "completion", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
	learned := policy.KeyModel{KeyID: keyID, Timezone: "UTC", LearnedAt: now.AddDate(0, 0, -14), Reliable: true,
		Schedule: map[time.Weekday][]policy.Appointment{now.Weekday(): {{MinuteOfDay: 10 * 60, MedianSize: 100}}}}
	if err := s.SaveModel(keyID, 7, "UTC", 7, learned, true, false, now); err != nil {
		t.Fatal(err)
	}
	enableRetentionForTest(t, s, now)
	b, err := s.Reserve(token, model.Metadata{Description: "db", OriginalName: "db.sql"}, 100, strings.Repeat("b", 64), now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(b.ID, now); err != nil {
		t.Fatal(err)
	}
	status, err := s.AutomationStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.DeletionBlocked || status.ActiveAnomalies != 1 {
		t.Fatal(status)
	}
	b2, err := s.Reserve(token, model.Metadata{Description: "db", OriginalName: "db2.sql"}, 100, strings.Repeat("c", 64), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(b2.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	status, err = s.AutomationStatus()
	if err != nil || !status.DeletionBlocked || status.ActiveAnomalies != 2 {
		t.Fatal(status, err)
	}
	mail, err := s.DueMail(now.Add(time.Minute), 10)
	if err != nil || len(mail) != 2 {
		t.Fatalf("completion anomaly mail: %v %v", mail, err)
	}
}

func TestOperationalAnomalyExclusionIsAuditedAndDoesNotMaskIntegrity(t *testing.T) {
	s, keyID, _ := setup(t, model.Profile{Name: "exclusions", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	enableRetentionForTest(t, s, now)
	if _, err := s.OpenAnomaly("time:first", "time", keyID, "", "primo ritardo", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAnomaly("time:second", "time", keyID, "", "secondo ritardo", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	status, _ := s.AutomationStatus()
	if !status.DeletionBlocked {
		t.Fatal("la seconda anomalia operativa non ha bloccato la retention")
	}
	anomalies, err := s.Anomalies(true)
	if err != nil || len(anomalies) != 2 {
		t.Fatal(anomalies, err)
	}
	exclusion, err := s.CreateAnomalyExclusion(anomalies[0].ID, now.Add(-time.Minute), now.Add(2*time.Hour), "admin", "guasto rete verificato", []string{"time"}, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if exclusion.KeyID != keyID || exclusion.Actor != "admin" || len(exclusion.Kinds) != 1 {
		t.Fatal(exclusion)
	}
	status, _ = s.AutomationStatus()
	if status.DeletionBlocked || status.ActiveAnomalies != 0 {
		t.Fatal(status)
	}
	created, err := s.OpenAnomalyAt("time:suppressed", "time", keyID, "", "dentro esclusione", now.Add(90*time.Minute), now.Add(3*time.Hour))
	if err != nil || created {
		t.Fatal(created, err)
	}
	history, err := s.Anomalies(false)
	if err != nil {
		t.Fatal(err)
	}
	foundSuppressed := false
	for _, anomaly := range history {
		if anomaly.StableKey == "time:suppressed" {
			foundSuppressed = anomaly.ResolvedAt != 0 && anomaly.ResolvedBy == "admin" && strings.Contains(anomaly.ResolutionNote, "esclusione amministrativa")
		}
	}
	if !foundSuppressed {
		t.Fatal("anomalia esclusa non conservata nell'audit", history)
	}
	created, err = s.OpenAnomalyAt("catalog:unsafe", "catalog", keyID, "", "file non coerente", now.Add(90*time.Minute), now.Add(3*time.Hour))
	if err != nil || !created {
		t.Fatal(created, err)
	}
	status, _ = s.AutomationStatus()
	if !status.DeletionBlocked {
		t.Fatal("l'esclusione operativa ha mascherato un problema di integrita")
	}
	if _, err = s.CreateAnomalyExclusion(anomalies[0].ID, now, now.Add(7*24*time.Hour+time.Second), "admin", "troppo lunga", []string{"time"}, now); err == nil {
		t.Fatal("esclusione superiore a sette giorni accettata")
	}
	if _, err = s.CreateAnomalyExclusion(anomalies[0].ID, now.Add(-time.Minute), now.Add(time.Hour), "admin", "seconda esclusione", []string{"time"}, now); err == nil {
		t.Fatal("la stessa anomalia originaria ha autorizzato piu esclusioni")
	}
}

func TestImmediateCompletionMatchesPreviousCivilDayAcrossMidnight(t *testing.T) {
	s, keyID, token := setup(t, model.Profile{Name: "midnight", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 0, 5, 0, 0, time.UTC)
	previous := now.AddDate(0, 0, -1)
	learned := policy.KeyModel{KeyID: keyID, Timezone: "UTC", LearnedAt: now.AddDate(0, 0, -14), Reliable: true,
		Schedule: map[time.Weekday][]policy.Appointment{previous.Weekday(): {{MinuteOfDay: 23*60 + 55, MedianSize: 100}}}}
	if err := s.SaveModel(keyID, 7, "UTC", 7, learned, true, false, now); err != nil {
		t.Fatal(err)
	}
	enableRetentionForTest(t, s, now)
	b, err := s.Reserve(token, model.Metadata{Description: "db", OriginalName: "midnight.sql"}, 100, strings.Repeat("d", 64), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(b.ID, now); err != nil {
		t.Fatal(err)
	}
	anomalies, err := s.Anomalies(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, anomaly := range anomalies {
		if anomaly.Kind == "time" {
			t.Fatal("falsa anomalia oltre mezzanotte", anomaly)
		}
	}
}

func TestRecoveryAndPurgeCannotBothRemainActive(t *testing.T) {
	s, _, token := setup(t, model.Profile{Name: "exclusive-ops", TotalBytes: 2000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	old := completedBackup(t, s, token, "obi_exclusive_old_123456", now.AddDate(0, 0, -10))
	_ = completedBackup(t, s, token, "obi_exclusive_new_123456", now.AddDate(0, 0, -1))
	enableRetentionForTest(t, s, now)
	quarantine, err := s.RequestQuarantine(old.ID, "manual", "admin", "test", now)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := authorizeOperationForTest(t, s, quarantine.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = confirmOperationForTest(t, s, quarantine.ID, authorized.Ticket, true, "", now); err != nil {
		t.Fatal(err)
	}
	recovery, err := s.RequestRecovery(old.ID, "admin", "ripristino", now.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RequestPurge(old.ID, "automatic", "maintenance", "scadenza", now.Add(48*time.Hour)); err == nil {
		t.Fatal("purge concorrente al recovery accettato")
	}
	operations, err := s.Operations("requested", "authorized")
	if err != nil || len(operations) != 1 || operations[0].ID != recovery.ID {
		t.Fatal(operations, err)
	}
	stored, err := s.Backup(old.ID)
	if err != nil || stored.Status != model.BackupQuarantined {
		t.Fatal(stored, err)
	}
}

func TestRevokedKeyStopsDestructiveOperationsAtEveryAuthorizationBoundary(t *testing.T) {
	for _, kind := range []string{"quarantine", "purge"} {
		for _, phase := range []string{"before-request", "after-request", "after-authorization"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				s, keyID, token := setup(t, model.Profile{Name: "revoked-operations", TotalBytes: 2000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
				now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
				victim := completedBackup(t, s, token, "revoked-victim-123456789", now.AddDate(0, 0, -11))
				_ = completedBackup(t, s, token, "revoked-survivor-1234567", now.AddDate(0, 0, -1))
				enableRetentionForTest(t, s, now)

				requestAt := now
				if kind == "purge" {
					quarantine, err := s.RequestQuarantine(victim.ID, "manual", "admin", "fixture", now.Add(-72*time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					authorized, err := authorizeOperationForTest(t, s, quarantine.ID, now.Add(-72*time.Hour))
					if err != nil {
						t.Fatal(err)
					}
					if err = confirmOperationForTest(t, s, quarantine.ID, authorized.Ticket, true, "", now.Add(-72*time.Hour)); err != nil {
						t.Fatal(err)
					}
				}

				if phase == "before-request" {
					if err := s.Revoke(keyID); err != nil {
						t.Fatal(err)
					}
				}
				var op model.RetentionOperation
				var err error
				if kind == "quarantine" {
					op, err = s.RequestQuarantine(victim.ID, "automatic", "maintenance", "test revoca", requestAt)
				} else {
					op, err = s.RequestPurge(victim.ID, "automatic", "maintenance", "test revoca", requestAt)
				}
				if phase == "before-request" {
					if err == nil {
						t.Fatal("operazione richiesta dopo la revoca")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if phase == "after-request" {
					if err = s.Revoke(keyID); err != nil {
						t.Fatal(err)
					}
					if _, err = authorizeOperationForTest(t, s, op.ID, requestAt); err == nil {
						t.Fatal("operazione autorizzata dopo la revoca")
					}
					current, operationErr := s.Operation(op.ID)
					stored, backupErr := s.Backup(victim.ID)
					expectedStatus := model.BackupComplete
					if kind == "purge" {
						expectedStatus = model.BackupQuarantined
					}
					if operationErr != nil || backupErr != nil || current.State != "cancelled" || stored.Status != expectedStatus {
						t.Fatalf("richiesta revocata non ripristinata: op=%+v backup=%+v errori=%v/%v", current, stored, operationErr, backupErr)
					}
					return
				}

				authorized, err := authorizeOperationForTest(t, s, op.ID, requestAt)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.Revoke(keyID); err != nil {
					t.Fatal(err)
				}
				if err = validateOperationForTest(t, s, op.ID, authorized.Ticket, requestAt); err == nil {
					t.Fatal("ticket validato dopo la revoca")
				}
				if err = s.ResetAuthorized(op.ID, now); err != nil {
					t.Fatal(err)
				}
				current, operationErr := s.Operation(op.ID)
				stored, backupErr := s.Backup(victim.ID)
				expectedStatus := model.BackupComplete
				if kind == "purge" {
					expectedStatus = model.BackupQuarantined
				}
				if operationErr != nil || backupErr != nil || current.State != "cancelled" || stored.Status != expectedStatus {
					t.Fatalf("ticket revocato non riconciliato: op=%+v backup=%+v errori=%v/%v", current, stored, operationErr, backupErr)
				}
			})
		}
	}
}

func TestRepeatedExtraCopiesAreReportedWithoutBlockingRetention(t *testing.T) {
	s, keyID, token := setup(t, model.Profile{Name: "extra-warning", TotalBytes: 10_000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	old := completedBackup(t, s, token, "extra-old-eligible-12345", now.AddDate(0, 0, -10))
	completedBackup(t, s, token, "extra-old-survivor-1234", now.AddDate(0, 0, -9))
	learned := policy.KeyModel{KeyID: keyID, Timezone: "UTC", LearnedAt: now.AddDate(0, 0, -14), Reliable: true,
		Schedule: map[time.Weekday][]policy.Appointment{now.Weekday(): {{MinuteOfDay: 2 * 60, MedianSize: 100}}}}
	if err := s.SaveModel(keyID, 7, "UTC", 7, learned, true, false, now); err != nil {
		t.Fatal(err)
	}
	enableRetentionForTest(t, s, now)
	for i, minute := range []int{0, 5, 10} {
		completedBackup(t, s, token, fmt.Sprintf("extra-warning-%02d-abcdef", i), time.Date(2026, 9, 28, 2, minute, 0, 0, time.UTC))
	}
	anomalies, err := s.Anomalies(true)
	if err != nil {
		t.Fatal(err)
	}
	extra := 0
	for _, anomaly := range anomalies {
		if anomaly.Kind == "extra" {
			extra++
			if !strings.Contains(anomaly.Detail, "attese 1") {
				t.Fatalf("riepilogo copia extra incompleto: %q", anomaly.Detail)
			}
			if !strings.Contains(anomaly.Detail, "ricevute 3") || !strings.Contains(anomaly.Detail, "200 byte") {
				t.Fatalf("riepilogo non aggiornato: %q", anomaly.Detail)
			}
		}
	}
	status, err := s.AutomationStatus()
	if err != nil || extra != 2 || status.DeletionBlocked {
		t.Fatalf("extra=%d stato=%+v errore=%v anomalie=%+v", extra, status, err, anomalies)
	}
	messages, err := s.DueMail(now, 20)
	if err != nil {
		t.Fatal(err)
	}
	extraMail := 0
	summaries := make(map[string]bool)
	for _, message := range messages {
		if strings.Contains(message.Subject, "NON BLOCCANTE") {
			extraMail++
			if strings.Contains(message.Body, "ricevute 2") && strings.Contains(message.Body, "100 byte") {
				summaries["seconda"] = true
			}
			if strings.Contains(message.Body, "ricevute 3") && strings.Contains(message.Body, "200 byte") {
				summaries["terza"] = true
			}
			if !strings.Contains(message.Body, "non sospende la retention") {
				t.Fatalf("mail extra ambigua: %+v", message)
			}
		}
	}
	if extraMail != 2 || len(summaries) != 2 {
		t.Fatalf("mail extra=%d: %+v", extraMail, messages)
	}
	quarantine, err := s.RequestQuarantine(old.ID, "manual", "admin", "le extra non bloccano", now)
	if err != nil {
		t.Fatalf("le sole extra hanno impedito una richiesta lecita: %v", err)
	}
	authorized, err := authorizeOperationForTest(t, s, quarantine.ID, now)
	if err != nil {
		t.Fatalf("le sole extra hanno impedito l'autorizzazione: %v", err)
	}
	if err = validateOperationForTest(t, s, quarantine.ID, authorized.Ticket, now); err != nil {
		t.Fatalf("le sole extra hanno invalidato il ticket: %v", err)
	}
	if err = confirmOperationForTest(t, s, quarantine.ID, authorized.Ticket, true, "", now); err != nil {
		t.Fatal(err)
	}
	if err = s.PauseRetention("prova resume con sole extra"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ResumeRetention(now); err != nil {
		t.Fatalf("le sole extra hanno impedito resume: %v", err)
	}
}

func TestMaintenanceSnapshotIsNotTruncatedAtTenThousandRows(t *testing.T) {
	s, keyID, _ := setup(t, model.Profile{Name: "large-snapshot", TotalBytes: 20000, MaxBackupBytes: 1, UploadsPerDay: 20000, Concurrent: 2})
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	statement, err := tx.Prepare(`INSERT INTO backups(id,key_id,description,original_name,size_bytes,sha256,status,started_at,received_at,purged_at) VALUES(?,?,?,?,?,?,'deleted',?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10001; i++ {
		id := fmt.Sprintf("%032x", i+1)
		if _, err = statement.Exec(id, keyID, "storico", "db.sql", 1, strings.Repeat("a", 64), int64(i+1), time.Unix(int64(i+1), 0).UTC().Format(time.RFC3339Nano), int64(i+2)); err != nil {
			statement.Close()
			t.Fatal(err)
		}
	}
	if err = statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	backups, err := s.SnapshotBackups()
	if err != nil || len(backups) != 10001 {
		t.Fatalf("snapshot rows=%d err=%v", len(backups), err)
	}
}

func TestMailQueueDeduplicatesAndPersistsRetrySchedule(t *testing.T) {
	s, _, _ := setup(t, model.Profile{Name: "mail-queue", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := s.QueueReport("stable-report", "subject", "body", now); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueReport("stable-report", "subject", "body", now); err != nil {
		t.Fatal(err)
	}
	messages, err := s.DueMail(now, 10)
	if err != nil || len(messages) != 1 {
		t.Fatalf("deduplicated queue: %v %v", messages, err)
	}
	if err = s.ConfirmMail(messages[0].ID, "SMTP unavailable", now); err != nil {
		t.Fatal(err)
	}
	if messages, err = s.DueMail(now.Add(59*time.Second), 10); err != nil || len(messages) != 0 {
		t.Fatalf("mail retried too early: %v %v", messages, err)
	}
	if messages, err = s.DueMail(now.Add(time.Minute), 10); err != nil || len(messages) != 1 || messages[0].Attempts != 1 {
		t.Fatalf("persistent retry missing: %v %v", messages, err)
	}
	if err = s.ConfirmMail(messages[0].ID, "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if messages, err = s.DueMail(now.Add(24*time.Hour), 10); err != nil || len(messages) != 0 {
		t.Fatalf("sent mail returned to queue: %v %v", messages, err)
	}
}
