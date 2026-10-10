package localapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
	"github.com/meiome/onlybackup/internal/store"
)

func apiFixture(t *testing.T) (*API, *store.Store) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := store.Init(root); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	api := New(s, root)
	api.Now = func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }
	return api, s
}

func request(handler http.Handler, method, path string, body any, peer bool) *httptest.ResponseRecorder {
	var data bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&data).Encode(body)
	}
	r := httptest.NewRequest(method, path, &data)
	r.Header.Set(protocolHeader, ProtocolVersion)
	if peer {
		r = r.WithContext(context.WithValue(r.Context(), peerKey{}, Peer{PID: 42, UID: 0, GID: 0}))
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestRoleSeparationVersionAndPeerEnforcement(t *testing.T) {
	api, _ := apiFixture(t)
	if response := request(api.AdminHandler(), "POST", "/v1/retention/pause", map[string]string{"reason": "test"}, false); response.Code != http.StatusForbidden {
		t.Fatalf("mutation without peer: %d %s", response.Code, response.Body.String())
	}
	if response := request(api.MaintenanceHandler(), "POST", "/v1/automation/enable", map[string]string{}, true); response.Code != http.StatusNotFound {
		t.Fatalf("maintenance reached admin command: %d", response.Code)
	}
	if response := request(api.MaintenanceHandler(), "POST", "/v1/retention/resume-when-ready", map[string]string{}, true); response.Code != http.StatusNotFound {
		t.Fatalf("maintenance reached deferred resume: %d", response.Code)
	}
	if response := request(api.AdminHandler(), "POST", "/v1/retention/resume-when-ready", map[string]string{}, false); response.Code != http.StatusForbidden {
		t.Fatalf("deferred resume without peer: %d", response.Code)
	}
	if response := request(api.AdminHandler(), "POST", "/v1/authorize", map[string]int{"id": 1}, true); response.Code != http.StatusNotFound {
		t.Fatalf("admin reached maintenance command: %d", response.Code)
	}
	r := httptest.NewRequest("GET", "/v1/status", nil)
	r.Header.Set(protocolHeader, "999")
	w := httptest.NewRecorder()
	api.AdminHandler().ServeHTTP(w, r)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("bad protocol accepted: %d", w.Code)
	}
}

func TestMaintenanceLeaseAPIRequiresGenerationForRenewal(t *testing.T) {
	api, _ := apiFixture(t)
	handler := api.MaintenanceHandler()
	acquired := request(handler, "POST", "/v1/lease", map[string]any{"owner": "worker", "generation": 0, "seconds": 600}, false)
	if acquired.Code != http.StatusOK {
		t.Fatalf("acquisizione: %d %s", acquired.Code, acquired.Body.String())
	}
	var lease model.MaintenanceLease
	if err := json.Unmarshal(acquired.Body.Bytes(), &lease); err != nil || lease.Generation <= 0 {
		t.Fatalf("lease senza generazione: %+v %v", lease, err)
	}
	withoutGeneration := request(handler, "POST", "/v1/lease", map[string]any{"owner": "worker", "seconds": 600}, false)
	if withoutGeneration.Code != http.StatusConflict {
		t.Fatalf("rinnovo senza generazione accettato: %d %s", withoutGeneration.Code, withoutGeneration.Body.String())
	}
	renewed := request(handler, "POST", "/v1/lease", map[string]any{"owner": "worker", "generation": lease.Generation, "seconds": 600}, false)
	if renewed.Code != http.StatusOK {
		t.Fatalf("rinnovo con generazione: %d %s", renewed.Code, renewed.Body.String())
	}
}

func TestSetupThroughAdminProtocolDoesNotEnableDeletion(t *testing.T) {
	api, s := apiFixture(t)
	response := request(api.AdminHandler(), "POST", "/v1/automation/setup", map[string]any{
		"host": "smtp.test", "port": 25, "tls": true, "from": "backup@test", "recipients": "admin@test", "timezone": "Europe/Rome",
		"confirmation": "CONFIGURA",
	}, true)
	if response.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", response.Code, response.Body.String())
	}
	status, err := s.AutomationStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || !status.ActivationPending || !status.DeletionBlocked || status.MailTested {
		t.Fatal(status)
	}
}

func TestMonitoringCheckLifecycleAndAdminHistoryAPI(t *testing.T) {
	api, s := apiFixture(t)
	now := api.Now().UTC()
	key, _, err := s.CreateKey("checks-api", "XS")
	if err != nil {
		t.Fatal(err)
	}
	learned := policy.KeyModel{KeyID: key.ID, Timezone: "UTC", LearnedAt: now.AddDate(0, 0, -14), Reliable: true,
		Schedule: map[time.Weekday][]policy.Appointment{now.Weekday(): {{MinuteOfDay: 2 * 60, MedianSize: 100}}}}
	if err = s.SaveModel(key.ID, 7, "UTC", 7, learned, true, false, learned.LearnedAt); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	started := request(api.MaintenanceHandler(), "POST", "/v1/check/start", map[string]string{}, false)
	if started.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", started.Code, started.Body.String())
	}
	var check model.MonitoringCheck
	if err = json.Unmarshal(started.Body.Bytes(), &check); err != nil {
		t.Fatal(err)
	}
	planned := request(api.MaintenanceHandler(), "POST", "/v1/check/plan", map[string]int64{"id": check.ID}, false)
	if planned.Code != http.StatusOK {
		t.Fatalf("plan: %d %s", planned.Code, planned.Body.String())
	}
	finding := policy.Finding{StableKey: "extra:api", Kind: "extra", KeyID: key.ID, BackupID: "api", Detail: "copia eccedente", At: now.Add(-time.Hour)}
	completed := request(api.MaintenanceHandler(), "POST", "/v1/check/complete", map[string]any{
		"id": check.ID, "outcome": "completed_with_anomalies", "summary": "1 anomalie: extra=1", "state": model.MonitoringRegular, "revision": 7, "findings": []policy.Finding{finding},
	}, false)
	if completed.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", completed.Code, completed.Body.String())
	}
	history := request(api.AdminHandler(), "GET", "/v1/monitoring-checks?limit=20", nil, false)
	if history.Code != http.StatusOK {
		t.Fatalf("history: %d %s", history.Code, history.Body.String())
	}
	var checks []model.MonitoringCheck
	if err = json.Unmarshal(history.Body.Bytes(), &checks); err != nil || len(checks) != 1 || checks[0].Status != "completed_with_anomalies" || len(checks[0].Models) != 1 {
		t.Fatalf("history response: %+v %v", checks, err)
	}
	if invalid := request(api.AdminHandler(), "GET", "/v1/monitoring-checks?limit=1001", nil, false); invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit accepted: %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestReconcileCrashBeforeAndAfterPhysicalOperations(t *testing.T) {
	api, s := apiFixture(t)
	now := api.Now().UTC()
	_, token, err := s.CreateKey("reconcile", "XS")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("catalogue and physical reconciliation")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	var backups []model.Backup
	for index := 0; index < 3; index++ {
		at := now.AddDate(0, 0, -10+index)
		backup, reserveErr := s.ReserveIdempotent(token, model.Metadata{Description: "reconcile", OriginalName: "backup.bin"}, int64(len(data)), digest, fmt.Sprintf("reconcile-key-%02d-abcdef", index), at.Add(-time.Minute))
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		if err = os.WriteFile(filepath.Join(api.Root, "archives", "backups", backup.ID+".backup"), data, 0440); err != nil {
			t.Fatal(err)
		}
		if err = s.Complete(backup.ID, at); err != nil {
			t.Fatal(err)
		}
		backups = append(backups, backup)
	}
	if err = s.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableAutomation(); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireMaintenanceLease("api-reconcile-test", 0, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	quarantine, err := s.RequestQuarantine(backups[0].ID, "manual", "test", "crash test", now)
	if err != nil {
		t.Fatal(err)
	}
	if quarantine, err = s.AuthorizeOperation(quarantine.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(api.Root, "archives", "backups", backups[0].ID+".backup")
	quarantinePath := filepath.Join(api.Root, "archives", "quarantine", backups[0].ID+".backup")
	if err = os.Rename(archivePath, quarantinePath); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = api.reconcileContext(ctx, quarantine.ID, now); err != context.Canceled {
		t.Fatalf("cancelled reconciliation: %v", err)
	}
	if err = api.verifyCompletedOperationContext(ctx, quarantine.ID); err != context.Canceled {
		t.Fatalf("cancelled confirmation: %v", err)
	}
	unchanged, err := s.Operation(quarantine.ID)
	if err != nil || unchanged.State != "authorized" {
		t.Fatalf("cancelled verification changed operation: %+v %v", unchanged, err)
	}
	active, err := s.Anomalies(true)
	if err != nil || len(active) != 0 {
		t.Fatalf("cancelled verification invented anomalies: %+v %v", active, err)
	}
	if result, reconcileErr := api.reconcile(quarantine.ID, now); reconcileErr != nil || result != "confirmed" {
		t.Fatalf("post-move reconciliation: %q %v", result, reconcileErr)
	}

	recovery, err := s.RequestRecovery(backups[0].ID, "test", "crash before move", now)
	if err != nil {
		t.Fatal(err)
	}
	if recovery, err = s.AuthorizeOperation(recovery.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	if result, reconcileErr := api.reconcile(recovery.ID, now); reconcileErr != nil || result != "reset" {
		t.Fatalf("pre-move reconciliation: %q %v", result, reconcileErr)
	}
	if recovery, err = s.AuthorizeOperation(recovery.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(quarantinePath, archivePath); err != nil {
		t.Fatal(err)
	}
	if result, reconcileErr := api.reconcile(recovery.ID, now); reconcileErr != nil || result != "confirmed" {
		t.Fatalf("post-recovery reconciliation: %q %v", result, reconcileErr)
	}

	quarantine, err = s.RequestQuarantine(backups[0].ID, "manual", "test", "purge crash test", now)
	if err != nil {
		t.Fatal(err)
	}
	if quarantine, err = s.AuthorizeOperation(quarantine.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(archivePath, quarantinePath); err != nil {
		t.Fatal(err)
	}
	if _, err = api.reconcile(quarantine.ID, now); err != nil {
		t.Fatal(err)
	}
	purgeAt := now.Add(48 * time.Hour)
	purge, err := s.RequestPurge(backups[0].ID, "automatic", "test", "purge", purgeAt)
	if err != nil {
		t.Fatal(err)
	}
	if purge, err = s.AuthorizeOperation(purge.ID, lease, purgeAt); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(quarantinePath); err != nil {
		t.Fatal(err)
	}
	if result, reconcileErr := api.reconcile(purge.ID, purgeAt); reconcileErr != nil || result != "confirmed" {
		t.Fatalf("post-unlink reconciliation: %q %v", result, reconcileErr)
	}
	stored, err := s.Backup(backups[0].ID)
	if err != nil || stored.Status != model.BackupDeleted {
		t.Fatalf("purge catalog state: %+v %v", stored, err)
	}
}

func TestConfirmRequiresVerifiedPhysicalResult(t *testing.T) {
	api, s := apiFixture(t)
	now := api.Now().UTC()
	_, token, err := s.CreateKey("physical-confirm", "XS")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("physically verified backup")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	var backups []model.Backup
	for i := 0; i < 2; i++ {
		at := now.AddDate(0, 0, -10+i)
		backup, reserveErr := s.ReserveIdempotent(token, model.Metadata{Description: "physical", OriginalName: "db.bin"}, int64(len(data)), digest, fmt.Sprintf("physical-confirm-%02d-key", i), at)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		if err = os.WriteFile(filepath.Join(api.Root, "archives", "backups", backup.ID+".backup"), data, 0440); err != nil {
			t.Fatal(err)
		}
		if err = s.Complete(backup.ID, at); err != nil {
			t.Fatal(err)
		}
		backups = append(backups, backup)
	}
	if err = s.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableAutomation(); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireMaintenanceLease("api-confirm-test", 0, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.RequestQuarantine(backups[0].ID, "manual", "admin", "test fisico", now)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := s.AuthorizeOperation(op.ID, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"id": authorized.ID, "ticket": authorized.Ticket, "done": true, "detail": "", "owner": lease.Owner, "generation": lease.Generation}
	response := request(api.MaintenanceHandler(), "POST", "/v1/confirm", body, false)
	if response.Code != http.StatusConflict {
		t.Fatalf("conferma accettata senza spostamento fisico: %d %s", response.Code, response.Body.String())
	}
	archive := filepath.Join(api.Root, "archives", "backups", backups[0].ID+".backup")
	quarantine := filepath.Join(api.Root, "archives", "quarantine", backups[0].ID+".backup")
	if err = os.WriteFile(quarantine, data, 0440); err != nil {
		t.Fatal(err)
	}
	if _, err = api.reconcile(op.ID, now); err == nil {
		t.Fatal("esito incerto accettato con entrambe le copie presenti")
	}
	current, err := s.Operation(op.ID)
	if err != nil || current.State != "authorized" {
		t.Fatalf("operazione incerta modificata: %+v %v", current, err)
	}
	anomalies, err := s.Anomalies(true)
	if err != nil {
		t.Fatal(err)
	}
	foundUncertain := false
	for _, anomaly := range anomalies {
		foundUncertain = foundUncertain || anomaly.StableKey == fmt.Sprintf("operation-uncertain:%d", op.ID) && anomaly.ResolvedAt == 0
	}
	if !foundUncertain {
		t.Fatalf("anomalia di esito incerto assente: %+v", anomalies)
	}
	if err = os.Remove(quarantine); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(archive, quarantine); err != nil {
		t.Fatal(err)
	}
	response = request(api.MaintenanceHandler(), "POST", "/v1/confirm", body, false)
	if response.Code != http.StatusOK {
		t.Fatalf("conferma fisica valida rifiutata: %d %s", response.Code, response.Body.String())
	}
}

func TestRetentionMinimumIsAdministratorOwned(t *testing.T) {
	api, s := apiFixture(t)
	key, _, err := s.CreateKey("minimum", "XS")
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"key_id": key.ID, "days": 30}
	if r := request(api.AdminHandler(), "POST", "/v1/retention/minimum", body, false); r.Code != http.StatusForbidden {
		t.Fatalf("missing peer accepted: %d", r.Code)
	}
	if r := request(api.MaintenanceHandler(), "POST", "/v1/retention/minimum", body, true); r.Code != http.StatusNotFound {
		t.Fatalf("maintenance changed minimum: %d", r.Code)
	}
	if r := request(api.MaintenanceHandler(), "POST", "/v1/retention/window", map[string]int{"days": 80}, false); r.Code != http.StatusNotFound {
		t.Fatalf("old automatic window endpoint accepted: %d", r.Code)
	}
	for _, days := range []int{0, 6, 365001} {
		body["days"] = days
		if r := request(api.AdminHandler(), "POST", "/v1/retention/minimum", body, true); r.Code != http.StatusBadRequest {
			t.Fatalf("invalid minimum accepted: %d", r.Code)
		}
	}
	body["days"] = 30
	if r := request(api.AdminHandler(), "POST", "/v1/retention/minimum", body, true); r.Code != http.StatusOK {
		t.Fatalf("minimum rejected: %d %s", r.Code, r.Body.String())
	}
	status, err := s.AutomationStatus()
	if err != nil || status.RetentionDaysForKey(key.ID) != 30 {
		t.Fatalf("minimum not saved: %+v %v", status, err)
	}
}
