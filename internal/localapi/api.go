// Package localapi implements the versioned, fixed-command Unix-socket
// protocol used by the local administrator and maintenance service.
package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/meiome/onlybackup/internal/backupfile"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
	"github.com/meiome/onlybackup/internal/store"
)

const ProtocolVersion = "1"
const protocolHeader = "X-OnlyBackup-Protocol"

type API struct {
	Store *store.Store
	Root  string
	Now   func() time.Time
}

func New(s *store.Store, root string) *API {
	return &API{Store: s, Root: root, Now: time.Now}
}

func (a *API) AdminHandler() http.Handler       { return a.handler(true) }
func (a *API) MaintenanceHandler() http.Handler { return a.handler(false) }

func (a *API) handler(admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(protocolHeader, ProtocolVersion)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get(protocolHeader) != ProtocolVersion {
			writeError(w, http.StatusPreconditionFailed, "versione protocollo locale incompatibile")
			return
		}
		if admin {
			a.serveAdmin(w, r)
		} else {
			if r.URL.Path == "/v1/reconcile" || r.URL.Path == "/v1/confirm" {
				// Only whole-file verification commands may outlive the control timeout.
				// No detached worker survives request cancellation or writer shutdown.
				if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
					writeError(w, 500, err.Error())
					return
				}
			}
			a.serveMaintenance(w, r)
		}
	})
}

func decode(r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(&discardResponseWriter{}, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return nil
}

type discardResponseWriter struct{}

func (*discardResponseWriter) Header() http.Header         { return make(http.Header) }
func (*discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*discardResponseWriter) WriteHeader(int)             {}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func requirePeer(r *http.Request) error {
	_, ok := PeerFromContext(r.Context())
	if !ok {
		return errors.New("identita Unix del chiamante non disponibile")
	}
	return nil
}

type adminConfirmation struct {
	Confirmation string `json:"confirmation"`
}

// Enforce consent at the writer boundary, also for clients bypassing the CLI.
func requireAdminConfirmation(w http.ResponseWriter, provided, expected string) bool {
	if provided != expected {
		writeError(w, http.StatusConflict, "conferma esplicita richiesta: "+expected)
		return false
	}
	return true
}

func (a *API) serveAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/status" {
		status, err := a.Store.AutomationStatus()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, status)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/quarantine" {
		backups, err := a.Store.SnapshotBackups()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		var result []model.Backup
		for _, backup := range backups {
			if backup.Status == model.BackupDeleting || backup.Status == model.BackupQuarantined || backup.Status == model.BackupPurging {
				result = append(result, backup)
			}
		}
		writeJSON(w, 200, result)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/inventory" {
		status, err := a.Store.AutomationStatus()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		keys, err := a.Store.Keys()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		backups, err := a.Store.Backups(r.URL.Query().Get("key"), 10000)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		models, err := a.Store.LoadModels()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"status": status, "keys": keys, "backups": backups, "models": models})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/anomalies" {
		anomalies, err := a.Store.Anomalies(r.URL.Query().Get("active") != "false")
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, anomalies)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/anomaly-exclusions" {
		exclusions, err := a.Store.AnomalyExclusions()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, exclusions)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/monitoring-checks" {
		limit := 20
		if value := r.URL.Query().Get("limit"); value != "" {
			var err error
			limit, err = strconv.Atoi(value)
			if err != nil {
				writeError(w, 400, "limite controlli non valido")
				return
			}
		}
		checks, err := a.Store.MonitoringChecks(limit)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, checks)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "metodo non consentito")
		return
	}
	if err := requirePeer(r); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	now := a.Now().UTC()
	switch r.URL.Path {
	case "/v1/automation/setup":
		var request struct {
			store.MailSettings
			adminConfirmation
			Timezone      string `json:"timezone"`
			RetentionMode string `json:"retention_mode"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if !requireAdminConfirmation(w, request.Confirmation, model.ConfirmAutomationSetup(request.RetentionMode)) {
			return
		}
		if err := a.Store.SetupAutomationMode(request.MailSettings, request.Timezone, request.RetentionMode, now); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if request.RetentionMode == "" {
			request.RetentionMode = "preserved"
		}
		writeJSON(w, 200, map[string]string{"status": "configured", "retention_mode": request.RetentionMode,
			"message": "Modalità retention salvata. In modalità auto, attivazione dopo prova mail e controllo valido; le pause manuali restano attive."})
	case "/v1/automation/enable":
		var request adminConfirmation
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if !requireAdminConfirmation(w, request.Confirmation, model.ConfirmRetentionEnable) {
			return
		}
		if err := a.Store.EnableAutomation(); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "enabled"})
	case "/v1/retention/minimum":
		var request struct {
			KeyID string `json:"key_id"`
			Days  int    `json:"days"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.SetKeyRetentionDays(request.KeyID, request.Days); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "configured"})
	case "/v1/retention/pause":
		var request struct {
			adminConfirmation
			Reason string `json:"reason"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if !requireAdminConfirmation(w, request.Confirmation, model.ConfirmRetentionPause) {
			return
		}
		if err := a.Store.PauseRetention(request.Reason); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "paused"})
	case "/v1/retention/resume":
		var request adminConfirmation
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if !requireAdminConfirmation(w, request.Confirmation, model.ConfirmRetentionResume) {
			return
		}
		if err := a.Store.ResumeRetention(now); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "resumed"})
	case "/v1/retention/resume-when-ready":
		var request adminConfirmation
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if !requireAdminConfirmation(w, request.Confirmation, model.ConfirmRetentionResumeWhenReady) {
			return
		}
		if err := a.Store.RequestRetentionResume(now); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "resume_pending", "message": "Ripresa richiesta al prossimo controllo valido, dopo la prova mail. Una nuova pausa o anomalia bloccante annulla la richiesta."})
	case "/v1/monitoring/relearn":
		if err := a.Store.BeginRelearn(); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "learning"})
	case "/v1/monitoring/acknowledge":
		var request struct {
			ID int64 `json:"id"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.AcknowledgeMissingAnomaly(request.ID, now); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "acknowledged"})
	case "/v1/monitoring/exclude":
		var request struct {
			SourceAnomalyID int64    `json:"source_anomaly_id"`
			StartsAt        int64    `json:"starts_at_unix"`
			EndsAt          int64    `json:"ends_at_unix"`
			Actor           string   `json:"actor"`
			Reason          string   `json:"reason"`
			Kinds           []string `json:"kinds"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		exclusion, err := a.Store.CreateAnomalyExclusion(request.SourceAnomalyID, time.Unix(request.StartsAt, 0).UTC(), time.Unix(request.EndsAt, 0).UTC(), request.Actor, request.Reason, request.Kinds, now)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 201, exclusion)
	case "/v1/retention/request":
		var request struct {
			BackupID string `json:"backup_id"`
			Actor    string `json:"actor"`
			Reason   string `json:"reason"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		op, err := a.Store.RequestQuarantine(request.BackupID, "manual", request.Actor, request.Reason, now)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 201, op)
	case "/v1/retention/cancel":
		var request struct {
			BackupID string `json:"backup_id"`
			Actor    string `json:"actor"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.CancelQuarantineRequest(request.BackupID, request.Actor, now); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "cancelled"})
	case "/v1/retention/recover":
		var request struct {
			BackupID string `json:"backup_id"`
			Actor    string `json:"actor"`
			Reason   string `json:"reason"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		op, err := a.Store.RequestRecovery(request.BackupID, request.Actor, request.Reason, now)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 201, op)
	case "/v1/mail/test":
		stable := "mail-test:" + strconv.FormatInt(now.Unix(), 10)
		if err := a.Store.QueueReport(stable, "OnlyBackup: mail di prova", "La configurazione mail di OnlyBackup funziona.", now); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 202, map[string]string{"status": "queued", "id": stable})
	default:
		writeError(w, 404, "comando amministrativo inesistente")
	}
}

type Snapshot struct {
	Status                       model.AutomationStatus     `json:"status"`
	Settings                     store.MailSettings         `json:"mail_settings"`
	Keys                         []model.Key                `json:"keys"`
	Quotas                       []model.Quota              `json:"quotas"`
	Backups                      []model.Backup             `json:"backups"`
	Operations                   []model.RetentionOperation `json:"operations"`
	Anomalies                    []model.Anomaly            `json:"anomalies"`
	Exclusions                   []model.AnomalyExclusion   `json:"anomaly_exclusions"`
	AcknowledgedMissingBackupIDs []string                   `json:"acknowledged_missing_backup_ids"`
	Models                       map[string]json.RawMessage `json:"models"`
	NextBackupID                 string                     `json:"next_backup_id,omitempty"`
	BackupsThrough               int64                      `json:"backups_through,omitempty"`
}

func (a *API) serveMaintenance(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/v1/snapshot" {
		paged := r.URL.Query().Get("paged") == "1"
		if after := r.URL.Query().Get("after"); paged && after != "" {
			through, err := strconv.ParseInt(r.URL.Query().Get("through"), 10, 64)
			if err != nil || through <= 0 || !model.ValidID(after) {
				writeError(w, 400, "cursore catalogo non valido")
				return
			}
			page, err := a.Store.BackupPage(after, through)
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			writeJSON(w, 200, page)
			return
		}
		status, err := a.Store.AutomationStatus()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		settings, err := a.Store.MailSettings()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		keys, err := a.Store.Keys()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		var backups []model.Backup
		var page model.BackupPage
		if paged {
			page, err = a.Store.BackupPage("", 0)
			backups = page.Backups
		} else {
			backups, err = a.Store.SnapshotBackups()
		}
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		operations, err := a.Store.SnapshotOperations()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		anomalies, err := a.Store.Anomalies(true)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		exclusions, err := a.Store.AnomalyExclusions()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		acknowledgedMissing, err := a.Store.AcknowledgedMissingBackupIDs()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		models, err := a.Store.LoadModels()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		var quotas []model.Quota
		for _, key := range keys {
			quota, quotaErr := a.Store.Quota(key.ID, a.Now())
			if quotaErr != nil {
				writeError(w, 500, quotaErr.Error())
				return
			}
			quotas = append(quotas, quota)
		}
		writeJSON(w, 200, Snapshot{Status: status, Settings: settings, Keys: keys, Quotas: quotas, Backups: backups, Operations: operations, Anomalies: anomalies, Exclusions: exclusions, AcknowledgedMissingBackupIDs: acknowledgedMissing, Models: models, NextBackupID: page.Next, BackupsThrough: page.Through})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/mail/due" {
		messages, err := a.Store.DueMail(a.Now().UTC(), 20)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, messages)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "metodo non consentito")
		return
	}
	now := a.Now().UTC()
	switch r.URL.Path {
	case "/v1/check/start":
		check, err := a.Store.StartMonitoringCheck(now)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, check)
	case "/v1/check/plan":
		var request struct {
			ID int64 `json:"id"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		check, err := a.Store.PlanMonitoringCheck(request.ID, now)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, check)
	case "/v1/check/complete":
		var request struct {
			ID       int64            `json:"id"`
			Outcome  string           `json:"outcome"`
			Summary  string           `json:"summary"`
			State    string           `json:"state"`
			Revision int64            `json:"revision"`
			Findings []policy.Finding `json:"findings"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.CompleteMonitoringCheck(request.ID, request.Outcome, request.Summary, request.State, request.Revision, request.Findings, now); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "completed"})
	case "/v1/check/fail":
		var request struct {
			ID      int64  `json:"id"`
			Summary string `json:"summary"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.FailMonitoringCheck(request.ID, request.Summary, now); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "failed"})
	case "/v1/lease":
		var request struct {
			Owner      string `json:"owner"`
			Generation int64  `json:"generation"`
			Seconds    int    `json:"seconds"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		lease, err := a.Store.AcquireMaintenanceLease(request.Owner, request.Generation, now, time.Duration(request.Seconds)*time.Second)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, lease)
	case "/v1/lease/release":
		var lease model.MaintenanceLease
		if err := decode(r, &lease); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.ReleaseMaintenanceLease(lease); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "released"})
	case "/v1/authorize":
		var request struct {
			ID         int64  `json:"id"`
			Owner      string `json:"owner"`
			Generation int64  `json:"generation"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		op, err := a.Store.AuthorizeOperation(request.ID, model.MaintenanceLease{Owner: request.Owner, Generation: request.Generation}, now)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, op)
	case "/v1/confirm":
		var request struct {
			ID         int64  `json:"id"`
			Ticket     string `json:"ticket"`
			Done       bool   `json:"done"`
			Detail     string `json:"detail"`
			Owner      string `json:"owner"`
			Generation int64  `json:"generation"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if request.Done {
			if err := a.verifyCompletedOperationContext(r.Context(), request.ID); err != nil {
				writeError(w, 409, "verifica fisica della conferma fallita: "+err.Error())
				return
			}
		}
		if err := a.Store.ConfirmOperation(request.ID, request.Ticket, request.Done, request.Detail, model.MaintenanceLease{Owner: request.Owner, Generation: request.Generation}, a.Now().UTC()); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "confirmed"})
	case "/v1/validate":
		var request struct {
			ID         int64  `json:"id"`
			Ticket     string `json:"ticket"`
			Owner      string `json:"owner"`
			Generation int64  `json:"generation"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.ValidateOperation(request.ID, request.Ticket, model.MaintenanceLease{Owner: request.Owner, Generation: request.Generation}, now); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "valid"})
	case "/v1/reconcile":
		var request struct {
			ID         int64  `json:"id"`
			Owner      string `json:"owner"`
			Generation int64  `json:"generation"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.ValidateMaintenanceLease(model.MaintenanceLease{Owner: request.Owner, Generation: request.Generation}, now); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		result, err := a.reconcileContext(r.Context(), request.ID, now, model.MaintenanceLease{Owner: request.Owner, Generation: request.Generation})
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": result})
	case "/v1/maintenance-block/clear":
		var request struct {
			ID int64 `json:"id"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		cleared, err := a.Store.TryClearResolvedMaintenanceBlock(request.ID, now)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"cleared": cleared})
	case "/v1/purge/request":
		var request struct {
			BackupID string `json:"backup_id"`
			Origin   string `json:"origin"`
			Actor    string `json:"actor"`
			Reason   string `json:"reason"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		op, err := a.Store.RequestPurge(request.BackupID, request.Origin, request.Actor, request.Reason, now)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 201, op)
	case "/v1/quarantine/request":
		var request struct {
			BackupID string `json:"backup_id"`
			Origin   string `json:"origin"`
			Actor    string `json:"actor"`
			Reason   string `json:"reason"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		op, err := a.Store.RequestQuarantine(request.BackupID, request.Origin, request.Actor, request.Reason, now)
		if err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 201, op)
	case "/v1/anomaly/open":
		var finding struct {
			StableKey string    `json:"stable_key"`
			Kind      string    `json:"kind"`
			KeyID     string    `json:"key_id"`
			BackupID  string    `json:"backup_id"`
			Detail    string    `json:"detail"`
			At        time.Time `json:"at"`
		}
		if err := decode(r, &finding); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		eventAt := finding.At
		if eventAt.IsZero() {
			eventAt = now
		}
		created, err := a.Store.OpenAnomalyAt(finding.StableKey, finding.Kind, finding.KeyID, finding.BackupID, finding.Detail, eventAt, now)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"created": created})
	case "/v1/anomaly/resolve":
		var request struct {
			StableKey string `json:"stable_key"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		resolved, err := a.Store.ResolveAnomaly(request.StableKey, now)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"resolved": resolved})
	case "/v1/model/save":
		var request struct {
			KeyID         string          `json:"key_id"`
			Timezone      string          `json:"timezone"`
			Revision      int64           `json:"revision"`
			RetentionDays int             `json:"retention_days"`
			Model         json.RawMessage `json:"model"`
			Reliable      bool            `json:"reliable"`
			Review        bool            `json:"review"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.SaveModel(request.KeyID, request.Revision, request.Timezone, request.RetentionDays, request.Model, request.Reliable, request.Review, now); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "saved"})
	case "/v1/check/state":
		var request struct {
			State    string `json:"state"`
			Revision int64  `json:"revision"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.SetCheckState(request.State, request.Revision, now); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "recorded"})
	case "/v1/report/queue":
		var request struct {
			StableID string `json:"stable_id"`
			Subject  string `json:"subject"`
			Body     string `json:"body"`
			Periodic bool   `json:"periodic"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		var err error
		if request.Periodic {
			err = a.Store.QueuePeriodicReport(request.StableID, request.Subject, request.Body, now)
		} else {
			err = a.Store.QueueReport(request.StableID, request.Subject, request.Body, now)
		}
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 202, map[string]string{"status": "queued"})
	case "/v1/mail/confirm":
		var request struct {
			ID            int64  `json:"id"`
			Error         string `json:"error"`
			ConfigVersion int64  `json:"config_version"`
		}
		if err := decode(r, &request); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := a.Store.ConfirmMail(request.ID, request.Error, request.ConfigVersion, now); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "recorded"})
	default:
		writeError(w, 404, fmt.Sprintf("comando manutenzione inesistente: %s", r.URL.Path))
	}
}

func (a *API) verifyCompletedOperation(operationID int64) error {
	return a.verifyCompletedOperationContext(context.Background(), operationID)
}

func (a *API) verifyCompletedOperationContext(ctx context.Context, operationID int64) error {
	op, err := a.Store.Operation(operationID)
	if err != nil {
		return err
	}
	backup, err := a.Store.Backup(op.BackupID)
	if err != nil {
		return err
	}
	archive := filepath.Join(a.Root, "archives", "backups", backup.ID+".backup")
	quarantine := filepath.Join(a.Root, "archives", "quarantine", backup.ID+".backup")
	archiveOK, quarantineOK := validBackupFileContext(ctx, archive, backup), validBackupFileContext(ctx, quarantine, backup)
	if err := ctx.Err(); err != nil {
		return err
	}
	archiveMissing, quarantineMissing := missing(archive), missing(quarantine)
	switch op.Kind {
	case "quarantine":
		if archiveMissing && quarantineOK {
			return nil
		}
	case "recover":
		if archiveOK && quarantineMissing {
			return nil
		}
	case "purge":
		if archiveMissing && quarantineMissing {
			return nil
		}
	default:
		return errors.New("tipo operazione non verificabile")
	}
	return errors.New("file assente, nella posizione errata o non corrispondente al catalogo")
}

func (a *API) reconcile(operationID int64, now time.Time) (string, error) {
	return a.reconcileContext(context.Background(), operationID, now)
}

func (a *API) reconcileContext(ctx context.Context, operationID int64, now time.Time, lease ...model.MaintenanceLease) (string, error) {
	op, err := a.Store.Operation(operationID)
	if err != nil {
		return "", err
	}
	if op.State != "authorized" {
		return "", model.ErrTicket
	}
	backup, err := a.Store.Backup(op.BackupID)
	if err != nil {
		return "", err
	}
	if !model.ValidID(backup.ID) {
		return "", errors.New("ID backup non valido")
	}
	archive := filepath.Join(a.Root, "archives", "backups", backup.ID+".backup")
	quarantine := filepath.Join(a.Root, "archives", "quarantine", backup.ID+".backup")
	archiveOK := validBackupFileContext(ctx, archive, backup)
	quarantineOK := validBackupFileContext(ctx, quarantine, backup)
	archiveMissing := missing(archive)
	quarantineMissing := missing(quarantine)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(lease) != 0 {
		now = a.Now().UTC()
		if err := a.Store.ValidateMaintenanceLease(lease[0], now); err != nil {
			return "", err
		}
	}
	done, untouched := false, false
	switch op.Kind {
	case "quarantine":
		done, untouched = archiveMissing && quarantineOK, archiveOK && quarantineMissing
	case "purge":
		done, untouched = quarantineMissing && archiveMissing, quarantineOK && archiveMissing
	case "recover":
		done, untouched = archiveOK && quarantineMissing, archiveMissing && quarantineOK
	default:
		return "", errors.New("operazione non riconciliabile")
	}
	if done {
		if err := a.Store.ReconcileAuthorized(operationID, now); err != nil {
			return "", err
		}
		return "confirmed", nil
	}
	if untouched {
		if err := a.Store.ResetAuthorized(operationID, now); err != nil {
			return "", err
		}
		if op.ExecutionCommitted {
			return "resume", nil
		}
		return "reset", nil
	}
	detail := fmt.Sprintf("esito fisico incerto per operazione %d backup %s", operationID, backup.ID)
	_, _ = a.Store.OpenAnomaly(fmt.Sprintf("operation-uncertain:%d", operationID), "maintenance", backup.KeyID, backup.ID, detail, now)
	return "", errors.New(detail)
}

func missing(path string) bool {
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

func validBackupFile(path string, backup model.Backup) bool {
	return validBackupFileContext(context.Background(), path, backup)
}

func validBackupFileContext(ctx context.Context, path string, backup model.Backup) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != backup.Size {
		return false
	}
	return backupfile.VerifyContext(ctx, path, backup.Size, backup.SHA256) == nil
}
