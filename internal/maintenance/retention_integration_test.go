package maintenance

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/localapi"
	"github.com/meiome/onlybackup/internal/localclient"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
	"github.com/meiome/onlybackup/internal/store"
)

type recordingSender struct{ stableIDs []string }

func (s *recordingSender) Send(_ context.Context, _ model.MailSettings, message model.MailMessage) error {
	s.stableIDs = append(s.stableIDs, message.StableID)
	return nil
}

func serveUnix(t *testing.T, path string, handler http.Handler) {
	t.Helper()
	rawListener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	listener := localapi.CredentialListener{Listener: rawListener}
	server := &http.Server{Handler: handler, ConnContext: localapi.ConnContext}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		<-done
	})
}

func TestRetentionEndToEndAcrossAdminWriterAndMaintenance(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	_, token, err := catalog.CreateKey("integration", "XS")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	data := []byte("retention integration payload")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	var backups []model.Backup
	for i := 0; i < 2; i++ {
		at := now.AddDate(0, 0, -10+i)
		backup, reserveErr := catalog.ReserveIdempotent(token, model.Metadata{Description: "integration", OriginalName: "db.bin"}, int64(len(data)), digest, fmt.Sprintf("retention-integration-%02d", i), at)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		if err = os.WriteFile(filepath.Join(root, "backups", backup.ID+".backup"), data, 0440); err != nil {
			t.Fatal(err)
		}
		if err = catalog.Complete(backup.ID, at); err != nil {
			t.Fatal(err)
		}
		backups = append(backups, backup)
	}
	if err = catalog.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.EnableAutomation(); err != nil {
		t.Fatal(err)
	}
	if err = catalog.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}

	api := localapi.New(catalog, root)
	api.Now = func() time.Time { return now }
	adminSocket := filepath.Join(root, "admin-test.sock")
	maintenanceSocket := filepath.Join(root, "maintenance-test.sock")
	serveUnix(t, adminSocket, api.AdminHandler())
	serveUnix(t, maintenanceSocket, api.MaintenanceHandler())
	admin := localclient.New(adminSocket, 5*time.Second)
	service := New(root, maintenanceSocket, "integration-test", nil)
	ctx := context.Background()

	request := func(path string) {
		t.Helper()
		var operation model.RetentionOperation
		if err := admin.Do(ctx, "POST", path, map[string]string{"backup_id": backups[0].ID, "actor": "admin", "reason": "test end-to-end"}, &operation); err != nil {
			t.Fatal(err)
		}
	}
	process := func() {
		t.Helper()
		var lease model.MaintenanceLease
		if err := service.call(ctx, "POST", "/v1/lease", map[string]any{"owner": service.Owner, "generation": 0, "seconds": int(maintenanceLeaseDuration / time.Second)}, &lease); err != nil {
			t.Fatal(err)
		}
		leaseCtx, cancel := context.WithCancel(ctx)
		guard := service.startLeaseGuard(leaseCtx, cancel, lease)
		defer func() {
			guard.close()
			_ = service.call(context.Background(), "POST", "/v1/lease/release", guard.identity(), nil)
		}()
		var snapshot localclient.Snapshot
		if err := service.call(leaseCtx, "GET", "/v1/snapshot", nil, &snapshot); err != nil {
			t.Fatal(err)
		}
		if err := service.processOperations(leaseCtx, snapshot, guard); err != nil {
			t.Fatal(err)
		}
	}

	request("/v1/retention/request")
	process()
	stored, err := catalog.Backup(backups[0].ID)
	if err != nil || stored.Status != model.BackupQuarantined {
		t.Fatal(stored, err)
	}
	request("/v1/retention/recover")
	process()
	stored, _ = catalog.Backup(backups[0].ID)
	if stored.Status != model.BackupComplete {
		t.Fatal(stored)
	}
	request("/v1/retention/request")
	process()
	now = now.Add(48 * time.Hour)
	var snapshot localclient.Snapshot
	if err = service.call(ctx, "GET", "/v1/snapshot", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	if err = service.schedulePurges(ctx, snapshot, now); err != nil {
		t.Fatal(err)
	}
	process()
	stored, err = catalog.Backup(backups[0].ID)
	if err != nil || stored.Status != model.BackupDeleted {
		t.Fatal(stored, err)
	}
	if _, err = os.Lstat(filepath.Join(root, "quarantine", backups[0].ID+".backup")); !os.IsNotExist(err) {
		t.Fatalf("file purgato ancora presente: %v", err)
	}
}

func TestExpiredLeaseStopsPhysicalMoveAfterContentVerification(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	_, token, err := catalog.CreateKey("lease-checkpoint", "XS")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	data := []byte("content verified before the final lease checkpoint")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	var backups []model.Backup
	for i := 0; i < 2; i++ {
		at := now.AddDate(0, 0, -10+i)
		backup, reserveErr := catalog.ReserveIdempotent(token, model.Metadata{Description: "lease checkpoint", OriginalName: "db.bin"}, int64(len(data)), digest, fmt.Sprintf("lease-checkpoint-%02d", i), at)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		if err = os.WriteFile(filepath.Join(root, "backups", backup.ID+".backup"), data, 0440); err != nil {
			t.Fatal(err)
		}
		if err = catalog.Complete(backup.ID, at); err != nil {
			t.Fatal(err)
		}
		backups = append(backups, backup)
	}
	if err = catalog.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.EnableAutomation(); err != nil {
		t.Fatal(err)
	}
	if err = catalog.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}
	op, err := catalog.RequestQuarantine(backups[0].ID, "automatic", "old-worker", "lease checkpoint", now)
	if err != nil {
		t.Fatal(err)
	}
	oldLease, err := catalog.AcquireMaintenanceLease("old-worker", 0, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := catalog.AuthorizeOperation(op.ID, oldLease, now)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(11 * time.Minute)
	newLease, err := catalog.AcquireMaintenanceLease("new-worker", 0, now, 10*time.Minute)
	if err != nil || newLease.Generation == oldLease.Generation {
		t.Fatalf("subentro nuova lease: %+v %v", newLease, err)
	}
	api := localapi.New(catalog, root)
	api.Now = func() time.Time { return now }
	socket := filepath.Join(root, "lease-checkpoint.sock")
	serveUnix(t, socket, api.MaintenanceHandler())
	service := New(root, socket, "old-worker", nil)
	staleGuard := &leaseGuard{service: service, lease: oldLease}
	err = service.executeLeased(context.Background(), staleGuard, authorized, backups[0])
	if !errors.Is(err, errLeaseLost) {
		t.Fatalf("operazione con lease vecchia: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(root, "backups", backups[0].ID+".backup")); err != nil {
		t.Fatalf("il worker vecchio ha rimosso il file archivio: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(root, "quarantine", backups[0].ID+".backup")); !os.IsNotExist(err) {
		t.Fatalf("il worker vecchio ha creato il file in quarantena: %v", err)
	}
}

func TestTransientPhysicalFailureReconcilesAndRetriesWithoutManualResume(t *testing.T) {
	for _, phase := range []string{"before-move", "after-move", "uncertain-then-intact", "uncertain-then-completed"} {
		t.Run(phase, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := store.Init(root); err != nil {
				t.Fatal(err)
			}
			catalog, err := store.Open(filepath.Join(root, "metadata.db"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer catalog.Close()
			_, token, err := catalog.CreateKey("transient-recovery", "XS")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
			data := []byte("transient physical failure payload")
			digest := fmt.Sprintf("%x", sha256.Sum256(data))
			var backups []model.Backup
			for day := -14; day <= 0; day++ {
				at := now.AddDate(0, 0, day).Add(-10 * time.Hour)
				backup, reserveErr := catalog.ReserveIdempotent(token, model.Metadata{Description: "transient recovery", OriginalName: "db.bin"}, int64(len(data)), digest, fmt.Sprintf("transient-recovery-%02d", day+14), at)
				if reserveErr != nil {
					t.Fatal(reserveErr)
				}
				if err = os.WriteFile(filepath.Join(root, "backups", backup.ID+".backup"), data, 0440); err != nil {
					t.Fatal(err)
				}
				if err = catalog.Complete(backup.ID, at); err != nil {
					t.Fatal(err)
				}
				backups = append(backups, backup)
			}
			if err = catalog.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
				t.Fatal(err)
			}
			if err = catalog.MarkMailTested(now); err != nil {
				t.Fatal(err)
			}
			if err = catalog.EnableAutomation(); err != nil {
				t.Fatal(err)
			}
			api := localapi.New(catalog, root)
			api.Now = func() time.Time { return now }
			socket := filepath.Join(root, "m.sock")
			serveUnix(t, socket, api.MaintenanceHandler())
			service := New(root, socket, "transient-recovery-worker", nil)
			service.Now = func() time.Time { return now }
			service.Filesystem = func(string) (policy.Filesystem, error) {
				return policy.Filesystem{Blocks: 1_000_000, Bfree: 1_000_000, Bavail: 1_000_000, BlockSize: 1}, nil
			}
			ctx := context.Background()
			if err = service.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if err = catalog.ResumeRetention(now); err != nil {
				t.Fatal(err)
			}

			op, err := catalog.RequestQuarantine(backups[0].ID, "manual", "admin", "errore transitorio", now)
			if err != nil {
				t.Fatal(err)
			}
			fixtureLease, err := catalog.AcquireMaintenanceLease("failing-worker", 0, now, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			authorized, err := catalog.AuthorizeOperation(op.ID, fixtureLease, now)
			if err != nil {
				t.Fatal(err)
			}
			archivePath := filepath.Join(root, "backups", op.BackupID+".backup")
			quarantinePath := filepath.Join(root, "quarantine", op.BackupID+".backup")
			if phase == "after-move" {
				// A directory fsync may fail after rename has already succeeded.
				if err = os.Rename(archivePath, quarantinePath); err != nil {
					t.Fatal(err)
				}
			}
			if err = catalog.ConfirmOperation(op.ID, authorized.Ticket, false, "errore fisico transitorio simulato", fixtureLease, now); err != nil {
				t.Fatal(err)
			}
			if err = catalog.ReleaseMaintenanceLease(fixtureLease); err != nil {
				t.Fatal(err)
			}
			blocked, err := catalog.AutomationStatus()
			if err != nil || !blocked.DeletionBlocked {
				t.Fatalf("l'errore fisico non ha bloccato la retention: %+v %v", blocked, err)
			}
			if phase == "uncertain-then-intact" || phase == "uncertain-then-completed" {
				// A crash between link and unlink in the rename fallback leaves both paths.
				if err = os.Link(archivePath, quarantinePath); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					now = now.Add(5 * time.Minute)
					if err = service.RunOnce(ctx); err == nil {
						t.Fatal("esito fisico ancora incerto accettato")
					}
					anomalies, queryErr := catalog.Anomalies(true)
					if queryErr != nil || len(anomalies) != 2 {
						t.Fatalf("anomalie incerte perse o duplicate: %+v %v", anomalies, queryErr)
					}
				}
				// Repair only the fixture; the next cycle must verify its resulting state.
				redundant := quarantinePath
				if phase == "uncertain-then-completed" {
					redundant = archivePath
				}
				if err = os.Remove(redundant); err != nil {
					t.Fatal(err)
				}
			}

			now = now.Add(5 * time.Minute)
			if err = service.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			current, err := catalog.Operation(op.ID)
			if err != nil || current.State != "confirmed" {
				t.Fatalf("operazione non ritentata: %+v %v", current, err)
			}
			stored, err := catalog.Backup(backups[0].ID)
			if err != nil || stored.Status != model.BackupQuarantined {
				t.Fatalf("backup non messo in quarantena: %+v %v", stored, err)
			}
			status, err := catalog.AutomationStatus()
			if err != nil || status.DeletionBlocked || status.MonitoringState != model.MonitoringRegular {
				t.Fatalf("blocco transitorio rimasto attivo: %+v %v", status, err)
			}
			anomalies, err := catalog.Anomalies(true)
			if err != nil {
				t.Fatal(err)
			}
			if len(anomalies) != 0 {
				t.Fatalf("anomalie dell'operazione ancora attive: %+v", anomalies)
			}
			// A repeated regular cycle must not reopen incidents or duplicate resolution mail.
			now = now.Add(5 * time.Minute)
			if err = service.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			messages, err := catalog.DueMail(now, 100)
			if err != nil {
				t.Fatal(err)
			}
			resolutions := 0
			for _, message := range messages {
				if message.Kind == "resolution" {
					resolutions++
				}
			}
			if resolutions != 1 {
				t.Fatalf("mail di risoluzione=%d, attesa una", resolutions)
			}
		})
	}
}

func TestAcknowledgedMissingBackupDoesNotRestartAnomalyCycle(t *testing.T) {
	for _, quarantined := range []bool{false, true} {
		t.Run(fmt.Sprintf("quarantined=%t", quarantined), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := store.Init(root); err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(root, "metadata.db")
			catalog, err := store.Open(dbPath, false)
			if err != nil {
				t.Fatal(err)
			}
			defer catalog.Close()
			_, token, err := catalog.CreateKey("acknowledged-loss", "XS")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			data := []byte("acknowledged loss integration payload")
			digest := fmt.Sprintf("%x", sha256.Sum256(data))
			var backups []model.Backup
			for day := -14; day <= 0; day++ {
				at := now.AddDate(0, 0, day).Add(-10 * time.Hour)
				backup, reserveErr := catalog.ReserveIdempotent(token, model.Metadata{Description: "acknowledged loss", OriginalName: "db.bin"}, int64(len(data)), digest, fmt.Sprintf("acknowledged-loss-%02d", day+14), at)
				if reserveErr != nil {
					t.Fatal(reserveErr)
				}
				if err = os.WriteFile(filepath.Join(root, "backups", backup.ID+".backup"), data, 0440); err != nil {
					t.Fatal(err)
				}
				if err = catalog.Complete(backup.ID, at); err != nil {
					t.Fatal(err)
				}
				backups = append(backups, backup)
			}
			if err = catalog.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
				t.Fatal(err)
			}
			if err = catalog.MarkMailTested(now); err != nil {
				t.Fatal(err)
			}
			if err = catalog.EnableAutomation(); err != nil {
				t.Fatal(err)
			}

			api := localapi.New(catalog, root)
			api.Now = func() time.Time { return now }
			maintenanceSocket := filepath.Join(root, "maintenance.sock")
			adminSocket := filepath.Join(root, "admin.sock")
			serveUnix(t, maintenanceSocket, api.MaintenanceHandler())
			serveUnix(t, adminSocket, api.AdminHandler())
			admin := localclient.New(adminSocket, 5*time.Second)
			service := New(root, maintenanceSocket, "acknowledged-loss-test", &recordingSender{})
			service.Now = func() time.Time { return now }
			service.Filesystem = func(string) (policy.Filesystem, error) {
				return policy.Filesystem{Blocks: 1_000_000, Bfree: 1_000_000, Bavail: 1_000_000, BlockSize: 1}, nil
			}
			ctx := context.Background()
			if err = service.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if err = catalog.ResumeRetention(now); err != nil {
				t.Fatal(err)
			}

			victim := backups[0]
			location := "backups"
			if quarantined {
				quarantinedAt := now.Add(-72 * time.Hour)
				op, requestErr := catalog.RequestQuarantine(victim.ID, "manual", "tester", "fixture quarantine", quarantinedAt)
				if requestErr != nil {
					t.Fatal(requestErr)
				}
				lease, leaseErr := catalog.AcquireMaintenanceLease("fixture-quarantine", 0, quarantinedAt, time.Hour)
				if leaseErr != nil {
					t.Fatal(leaseErr)
				}
				authorized, authorizeErr := catalog.AuthorizeOperation(op.ID, lease, quarantinedAt)
				if authorizeErr != nil {
					t.Fatal(authorizeErr)
				}
				if err = service.execute(authorized, victim); err != nil {
					t.Fatal(err)
				}
				if err = catalog.ConfirmOperation(op.ID, authorized.Ticket, true, "", lease, quarantinedAt); err != nil {
					t.Fatal(err)
				}
				if err = catalog.ReleaseMaintenanceLease(lease); err != nil {
					t.Fatal(err)
				}
				location = "quarantine"
			}
			if err = os.Remove(filepath.Join(root, location, victim.ID+".backup")); err != nil {
				t.Fatal(err)
			}
			if err = service.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			anomalies, err := catalog.Anomalies(true)
			if err != nil {
				t.Fatal(err)
			}
			var anomalyID int64
			for _, anomaly := range anomalies {
				if anomaly.Kind == "file_missing" && anomaly.BackupID == victim.ID {
					anomalyID = anomaly.ID
				}
			}
			if anomalyID == 0 {
				t.Fatalf("anomalia file_missing non trovata: %+v", anomalies)
			}
			if err = admin.Do(ctx, "POST", "/v1/monitoring/acknowledge", map[string]any{"id": anomalyID}, nil); err != nil {
				t.Fatal(err)
			}
			acknowledgedStatus, err := catalog.AutomationStatus()
			if err != nil {
				t.Fatal(err)
			}
			if !acknowledgedStatus.DeletionBlocked {
				t.Fatalf("acknowledgement ha rimosso il blocco prima del controllo e resume: %+v", acknowledgedStatus)
			}

			reopened, err := store.Open(dbPath, true)
			if err != nil {
				t.Fatal(err)
			}
			acknowledgedIDs, err := reopened.AcknowledgedMissingBackupIDs()
			if closeErr := reopened.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(acknowledgedIDs) != 1 || acknowledgedIDs[0] != victim.ID {
				t.Fatalf("acknowledgement non persistente: %v", acknowledgedIDs)
			}

			if err = service.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if err = catalog.ResumeRetention(now); err != nil {
				t.Fatal(err)
			}
			for cycle := 0; cycle < 2; cycle++ {
				if err = service.RunOnce(ctx); err != nil {
					t.Fatalf("ciclo %d dopo resume: %v", cycle+1, err)
				}
			}

			status, err := catalog.AutomationStatus()
			if err != nil {
				t.Fatal(err)
			}
			if status.DeletionBlocked || status.MonitoringState != model.MonitoringRegular {
				t.Fatalf("retention nuovamente bloccata: %+v", status)
			}
			if active, activeErr := catalog.Anomalies(true); activeErr != nil || len(active) != 0 {
				t.Fatalf("nuove anomalie attive: %+v, errore: %v", active, activeErr)
			}
			allAnomalies, err := catalog.Anomalies(false)
			if err != nil {
				t.Fatal(err)
			}
			missingHistory := 0
			for _, anomaly := range allAnomalies {
				if anomaly.BackupID != victim.ID {
					continue
				}
				if anomaly.Kind == "file_missing" && anomaly.AcknowledgedAt != 0 && anomaly.ResolvedAt != 0 {
					missingHistory++
					continue
				}
				t.Fatalf("anomalia inattesa per il backup perso: %+v", anomaly)
			}
			if missingHistory != 1 {
				t.Fatalf("storico file_missing non preservato una sola volta: %+v", allAnomalies)
			}
			operations, err := catalog.Operations()
			if err != nil {
				t.Fatal(err)
			}
			for _, operation := range operations {
				if operation.BackupID == victim.ID && operation.Kind == "purge" {
					t.Fatalf("purge creato per backup riconosciuto perso: %+v", operation)
				}
			}
			stored, err := catalog.Backup(victim.ID)
			if err != nil {
				t.Fatal(err)
			}
			expectedStatus := model.BackupComplete
			if quarantined {
				expectedStatus = model.BackupQuarantined
			}
			if stored.Status != expectedStatus {
				t.Fatalf("stato storico modificato: ottenuto %s, atteso %s", stored.Status, expectedStatus)
			}
		})
	}
}

func TestQueuedMailIsAttemptedWhenCycleFailsAfterSnapshot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err = catalog.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.QueueReport("queued-before-error", "report", "body", now); err != nil {
		t.Fatal(err)
	}
	api := localapi.New(catalog, root)
	api.Now = func() time.Time { return now }
	socket := filepath.Join(root, "maintenance-mail-test.sock")
	serveUnix(t, socket, api.MaintenanceHandler())
	sender := &recordingSender{}
	service := New(root, socket, "mail-error-test", sender)
	service.Now = func() time.Time { return now }
	service.Filesystem = func(string) (policy.Filesystem, error) {
		return policy.Filesystem{}, fmt.Errorf("errore filesystem simulato")
	}
	if err = service.RunOnce(context.Background()); err == nil {
		t.Fatal("il ciclo con errore filesystem e risultato riuscito")
	}
	found := false
	for _, stableID := range sender.stableIDs {
		found = found || stableID == "queued-before-error"
	}
	if !found {
		t.Fatalf("mail accodata non tentata dopo errore: %v", sender.stableIDs)
	}
}

func TestReliableKeyIsCheckedWhileNewKeyLearns(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	knownKey, _, err := catalog.CreateKey("known", "XS")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = catalog.CreateKey("new", "XS"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err = catalog.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	modelForKnown := policy.KeyModel{KeyID: knownKey.ID, Timezone: "UTC", LearnedAt: now.AddDate(0, 0, -14), Reliable: true,
		Schedule: map[time.Weekday][]policy.Appointment{now.AddDate(0, 0, -1).Weekday(): {{MinuteOfDay: 2 * 60, MedianSize: 100}}}}
	if err = catalog.SaveModel(knownKey.ID, 7, "UTC", 7, modelForKnown, true, false, now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	api := localapi.New(catalog, root)
	api.Now = func() time.Time { return now }
	socket := filepath.Join(root, "mixed-models.sock")
	serveUnix(t, socket, api.MaintenanceHandler())
	service := New(root, socket, "mixed-models-test", nil)
	var snapshot localclient.Snapshot
	if err = service.call(context.Background(), "GET", "/v1/snapshot", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	var check model.MonitoringCheck
	if err = service.call(context.Background(), "POST", "/v1/check/start", map[string]string{}, &check); err != nil {
		t.Fatal(err)
	}
	findings, allReliable, _, err := service.monitor(context.Background(), snapshot, check.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if allReliable {
		t.Fatal("new key without history made all models reliable")
	}
	found := false
	for _, finding := range findings {
		found = found || finding.Kind == "schedule_missing" && finding.KeyID == knownKey.ID
	}
	if !found {
		t.Fatalf("reliable key was not checked while new key learned: %+v", findings)
	}
}

func TestRunOnceRefreshesSnapshotAfterReconcile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	_, token, err := catalog.CreateKey("reconcile", "XS")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	data := []byte("reconcile payload")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	var backups []model.Backup
	for i := 0; i < 2; i++ {
		at := now.AddDate(0, 0, -10+i)
		backup, reserveErr := catalog.ReserveIdempotent(token, model.Metadata{Description: "reconcile", OriginalName: "db.bin"}, int64(len(data)), digest, fmt.Sprintf("reconcile-integration-%02d", i), at)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		if err = os.WriteFile(filepath.Join(root, "backups", backup.ID+".backup"), data, 0440); err != nil {
			t.Fatal(err)
		}
		if err = catalog.Complete(backup.ID, at); err != nil {
			t.Fatal(err)
		}
		backups = append(backups, backup)
	}
	if err = catalog.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.EnableAutomation(); err != nil {
		t.Fatal(err)
	}
	if err = catalog.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}
	op, err := catalog.RequestQuarantine(backups[0].ID, "manual", "tester", "reconcile test", now)
	if err != nil {
		t.Fatal(err)
	}
	fixtureLease, err := catalog.AcquireMaintenanceLease("reconcile-fixture", 0, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = catalog.AuthorizeOperation(op.ID, fixtureLease, now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.ReleaseMaintenanceLease(fixtureLease); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(root, "backups", backups[0].ID+".backup"), filepath.Join(root, "quarantine", backups[0].ID+".backup")); err != nil {
		t.Fatal(err)
	}
	api := localapi.New(catalog, root)
	api.Now = func() time.Time { return now }
	socket := filepath.Join(root, "reconcile.sock")
	serveUnix(t, socket, api.MaintenanceHandler())
	service := New(root, socket, "reconcile-test", nil)
	service.Now = func() time.Time { return now }
	service.Filesystem = func(string) (policy.Filesystem, error) {
		return policy.Filesystem{Blocks: 10_000, Bfree: 9_000, Bavail: 9_000, BlockSize: 1}, nil
	}
	if err = service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := catalog.Backup(backups[0].ID)
	if err != nil || stored.Status != model.BackupQuarantined {
		t.Fatalf("operation not reconciled: %+v %v", stored, err)
	}
	anomalies, err := catalog.Anomalies(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, anomaly := range anomalies {
		if anomaly.Kind == "file_missing" || anomaly.Kind == "catalog" {
			t.Fatalf("false anomaly after reconcile: %+v", anomaly)
		}
	}
}

func TestShortfallMail(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	_, token, err := catalog.CreateKey("shortfall", "XS")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		at := now.AddDate(0, 0, -30+i)
		data := []byte(fmt.Sprintf("shortfall-%d", i))
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		backup, reserveErr := catalog.ReserveIdempotent(token, model.Metadata{Description: "shortfall", OriginalName: "db.bin"}, int64(len(data)), digest, fmt.Sprintf("shortfall-integration-%02d", i), at)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		if err = catalog.Complete(backup.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	if err = catalog.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.EnableAutomation(); err != nil {
		t.Fatal(err)
	}
	if err = catalog.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = catalog.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}

	api := localapi.New(catalog, root)
	api.Now = func() time.Time { return now }
	socket := filepath.Join(root, "shortfall.sock")
	serveUnix(t, socket, api.MaintenanceHandler())
	service := New(root, socket, "shortfall-test", nil)
	var snapshot localclient.Snapshot
	if err = service.call(context.Background(), "GET", "/v1/snapshot", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	filesystem := policy.Filesystem{Blocks: 10_000, Bfree: 1_000, Bavail: 1_000, BlockSize: 1}
	if err = service.scheduleAutomatic(context.Background(), snapshot, filesystem, 0, now); err != nil {
		t.Fatal(err)
	}
	operations, err := catalog.SnapshotOperations()
	if err != nil || len(operations) != 1 || operations[0].Kind != "quarantine" {
		t.Fatalf("candidato disponibile non richiesto: %+v %v", operations, err)
	}
	messages, err := catalog.DueMail(now, 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		if message.Subject == "OnlyBackup: anomalia spazio liberabile insufficiente" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mail di spazio insufficiente non accodata: %+v", messages)
	}
	status, err := catalog.AutomationStatus()
	if err != nil || status.DeletionBlocked {
		t.Fatalf("la sola notifica ha modificato i blocchi retention: %+v %v", status, err)
	}
}
