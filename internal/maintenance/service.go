// Package maintenance performs monitoring, mail delivery and the only
// physical quarantine/recovery/purge operations in OnlyBackup.
package maintenance

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/meiome/onlybackup/internal/backupfile"
	"github.com/meiome/onlybackup/internal/localclient"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

type Sender interface {
	Send(context.Context, model.MailSettings, model.MailMessage) error
}

type Service struct {
	Root       string
	Client     *localclient.Client
	Owner      string
	Now        func() time.Time
	Filesystem func(string) (policy.Filesystem, error)
	Mail       Sender
}

const (
	maintenanceLeaseDuration = 10 * time.Minute
	maintenanceLeaseRenewal  = 2 * time.Minute
)

type leaseGuard struct {
	service *Service
	lease   model.MaintenanceLease
	cancel  context.CancelFunc
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	err     error
}

func New(root, socket, owner string, sender Sender) *Service {
	return &Service{Root: root, Client: localclient.New(socket, 2*time.Minute), Owner: owner,
		Now: time.Now, Filesystem: StatFilesystem, Mail: sender}
}

func StatFilesystem(path string) (policy.Filesystem, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return policy.Filesystem{}, err
	}
	if stat.Bsize <= 0 {
		return policy.Filesystem{}, errors.New("dimensione blocco filesystem non valida")
	}
	return policy.Filesystem{Blocks: stat.Blocks, Bfree: stat.Bfree, Bavail: stat.Bavail, BlockSize: uint64(stat.Bsize)}, nil
}

func (s *Service) call(ctx context.Context, method, path string, request, response any) error {
	return s.Client.Do(ctx, method, path, request, response)
}

func (s *Service) startLeaseGuard(ctx context.Context, cancel context.CancelFunc, lease model.MaintenanceLease) *leaseGuard {
	guard := &leaseGuard{service: s, lease: lease, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(guard.done)
		ticker := time.NewTicker(maintenanceLeaseRenewal)
		defer ticker.Stop()
		for {
			select {
			case <-guard.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := guard.renew(ctx); err != nil {
					guard.fail(err)
					return
				}
			}
		}
	}()
	return guard
}

func (g *leaseGuard) identity() model.MaintenanceLease {
	g.mu.Lock()
	defer g.mu.Unlock()
	return model.MaintenanceLease{Owner: g.lease.Owner, Generation: g.lease.Generation, ExpiresAt: g.lease.ExpiresAt}
}

func (g *leaseGuard) renew(ctx context.Context) error {
	g.mu.Lock()
	if g.err != nil {
		err := g.err
		g.mu.Unlock()
		return err
	}
	lease := g.lease
	g.mu.Unlock()
	var renewed model.MaintenanceLease
	request := map[string]any{"owner": lease.Owner, "generation": lease.Generation, "seconds": int(maintenanceLeaseDuration / time.Second)}
	if err := g.service.call(ctx, "POST", "/v1/lease", request, &renewed); err != nil {
		return fmt.Errorf("rinnovo lease: %w", err)
	}
	if renewed.Owner != lease.Owner || renewed.Generation != lease.Generation {
		return model.ErrLease
	}
	g.mu.Lock()
	g.lease = renewed
	g.mu.Unlock()
	return nil
}

func (g *leaseGuard) fail(err error) {
	g.mu.Lock()
	if g.err == nil {
		g.err = err
		g.cancel()
	}
	g.mu.Unlock()
}

func (g *leaseGuard) close() {
	g.cancel()
	close(g.stop)
	<-g.done
}

func (s *Service) RunOnce(ctx context.Context) (runErr error) {
	// Hold an OS lock for the whole cycle, including reconciliation and mail.
	// Timeouts in the writer protocol cannot admit a second physical executor.
	lock, err := s.lockExecution()
	if err != nil {
		return err
	}
	defer lock.Close()
	mailCtx := ctx
	now := s.Now().UTC()
	var snapshot localclient.Snapshot
	snapshotReady := false
	defer func() {
		if !snapshotReady {
			return
		}
		if mailErr := s.deliverMail(mailCtx, snapshot.Settings, now); runErr == nil && mailErr != nil {
			runErr = mailErr
		}
	}()
	var lease model.MaintenanceLease
	if err := s.call(ctx, "POST", "/v1/lease", map[string]any{"owner": s.Owner, "generation": 0, "seconds": int(maintenanceLeaseDuration / time.Second)}, &lease); err != nil {
		return err
	}
	leaseCtx, cancelLease := context.WithCancel(ctx)
	guard := s.startLeaseGuard(leaseCtx, cancelLease, lease)
	defer func() {
		guard.close()
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.call(releaseCtx, "POST", "/v1/lease/release", guard.identity(), nil)
	}()
	ctx = leaseCtx
	var check model.MonitoringCheck
	if err := s.call(ctx, "POST", "/v1/check/start", map[string]string{}, &check); err != nil {
		return err
	}
	checkCompleted := false
	defer func() {
		if checkCompleted {
			return
		}
		summary := "controllo interrotto"
		if runErr != nil {
			summary = runErr.Error()
		}
		failCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.call(failCtx, "POST", "/v1/check/fail", map[string]any{"id": check.ID, "summary": summary}, nil)
	}()
	if err := s.call(ctx, "GET", "/v1/snapshot", nil, &snapshot); err != nil {
		return err
	}
	snapshotReady = true
	if err := s.saveLocalState(snapshot.Settings); err != nil {
		return err
	}
	if err := s.reconcileWriterFailure(ctx); err != nil {
		return err
	}
	if err := s.reconcile(ctx, snapshot, guard); err != nil {
		return err
	}
	if err := s.call(ctx, "GET", "/v1/snapshot", nil, &snapshot); err != nil {
		return err
	}
	// Complete durable permissions before new monitoring/policy decisions.
	// In particular, a later revocation must not strand admitted work.
	committed := snapshot
	committed.Operations = nil
	for _, op := range snapshot.Operations {
		if op.State == "authorized" && op.ExecutionCommitted {
			committed.Operations = append(committed.Operations, op)
		}
	}
	if err := s.processOperations(ctx, committed, guard); err != nil {
		return err
	}
	if len(committed.Operations) != 0 {
		if err := s.call(ctx, "GET", "/v1/snapshot", nil, &snapshot); err != nil {
			return err
		}
	}
	filesystem, err := s.Filesystem(filepath.Join(s.Root, "backups"))
	if err != nil {
		_ = s.openFinding(ctx, policy.Finding{StableKey: "filesystem:archive", Kind: "filesystem", Detail: err.Error()})
		return err
	}
	findings := s.fileFindings(snapshot)
	var verified localclient.Snapshot
	if err = s.call(ctx, "GET", "/v1/snapshot", nil, &verified); err != nil {
		return err
	}
	findings = s.verifyUnexpectedFiles(findings, verified)
	snapshot = verified
	monitoringFindings, modelsReliable, expected48h, required, err := s.monitor(ctx, snapshot, check.ID, now)
	if err != nil {
		return err
	}
	findings = append(findings, monitoringFindings...)
	if modelsReliable {
		total, used, _, measureErr := filesystem.Bytes()
		if measureErr != nil {
			return measureErr
		}
		var catalogBytes uint64
		for _, backup := range recoverableBackups(snapshot) {
			if backup.Status == model.BackupReceiving || backup.Status == model.BackupComplete || backup.Status == model.BackupDeleting || backup.Status == model.BackupQuarantined || backup.Status == model.BackupPurging {
				catalogBytes += uint64(backup.Size)
			}
		}
		otherUsed := uint64(0)
		if used > catalogBytes {
			otherUsed = used - catalogBytes
		}
		reserve := uint64(snapshot.Status.ReserveFree)
		// Configuration belongs to the administrator. Forecast only whether
		// those minima fit; never derive or reduce them from disk capacity.
		if total <= otherUsed || total-otherUsed <= reserve || total-otherUsed-reserve < required {
			findings = append(findings, policy.Finding{StableKey: "capacity:retention-window", Kind: "capacity", Detail: "spazio insufficiente per i minimi di conservazione impostati per chiave, giorno corrente, quarantena e riserva; intervento amministrativo richiesto"})
		}
		_, _, available, _ := filesystem.Bytes()
		if expected48h > ^uint64(0)-reserve || available < expected48h+reserve {
			findings = append(findings, policy.Finding{StableKey: policy.QuarantineMarginAnomalyKey, Kind: "capacity", Detail: "spazio disponibile insufficiente per 48 ore di depositi e riserva durante la quarantena"})
		}
	}
	state := model.MonitoringLearning
	if modelsReliable {
		state = model.MonitoringRegular
	}
	revision := snapshot.Status.ModelRevision
	if revision == 0 {
		revision = now.UnixNano()
	}
	outcome := "completed_clean"
	if len(findings) != 0 {
		outcome = "completed_with_anomalies"
	}
	completion := map[string]any{"id": check.ID, "outcome": outcome, "summary": findingsSummary(findings), "state": state, "revision": revision, "findings": findings}
	if err = s.call(ctx, "POST", "/v1/check/complete", completion, nil); err != nil {
		return err
	}
	checkCompleted = true
	for _, op := range snapshot.Operations {
		if err = s.call(ctx, "POST", "/v1/maintenance-block/clear", map[string]int64{"id": op.ID}, nil); err != nil {
			return err
		}
	}
	if err = s.recordFindings(ctx, snapshot, findings); err != nil {
		return err
	}
	var refreshed localclient.Snapshot
	if err = s.call(ctx, "GET", "/v1/snapshot", nil, &refreshed); err != nil {
		return err
	}
	currentStatus := refreshed.Status
	snapshot.Status = currentStatus
	snapshot.Anomalies = refreshed.Anomalies
	state = currentStatus.MonitoringState
	var reminder []string
	for _, anomaly := range snapshot.Anomalies {
		if now.Unix()-anomaly.OpenedAt >= int64(24*time.Hour/time.Second) {
			reminder = append(reminder, anomaly.StableKey+": "+anomaly.Detail)
		}
	}
	if len(reminder) != 0 {
		sort.Strings(reminder)
		request := map[string]any{"stable_id": fmt.Sprintf("reminder:%d", now.Unix()/int64(24*time.Hour/time.Second)),
			"subject": "OnlyBackup: promemoria anomalie attive", "body": strings.Join(reminder, "\n")}
		if err = s.call(ctx, "POST", "/v1/report/queue", request, nil); err != nil {
			return err
		}
	}
	if snapshot.Status.NextReportAt == 0 || now.Unix() >= snapshot.Status.NextReportAt {
		reportSnapshot := snapshot
		reportSnapshot.Operations = refreshed.Operations
		report := buildEmailReport(reportSnapshot, filesystem, findings, s.Now().UTC())
		request := map[string]any{"stable_id": fmt.Sprintf("report:%d", now.Unix()/int64(72*time.Hour/time.Second)), "subject": "OnlyBackup: report periodico", "body": report}
		request["periodic"] = true
		if err = s.call(ctx, "POST", "/v1/report/queue", request, nil); err != nil {
			return err
		}
	}
	if snapshot.Status.Enabled && !snapshot.Status.DeletionBlocked && state == model.MonitoringRegular {
		if err = s.schedulePurges(ctx, snapshot, now); err != nil {
			return err
		}
		if err = s.scheduleAutomatic(ctx, snapshot, filesystem, expected48h, now); err != nil {
			return err
		}
	}
	// Refresh work because this cycle may just have created requests.
	if err = s.call(ctx, "GET", "/v1/snapshot", nil, &snapshot); err != nil {
		return err
	}
	if err = s.processOperations(ctx, snapshot, guard); err != nil {
		return err
	}
	return nil
}

func (s *Service) reconcile(ctx context.Context, snapshot localclient.Snapshot, guard *leaseGuard) error {
	reconciled := false
	for _, op := range snapshot.Operations {
		if op.State != "authorized" {
			continue
		}
		var result map[string]string
		lease := guard.identity()
		if err := s.call(ctx, "POST", "/v1/reconcile", map[string]any{"id": op.ID, "owner": lease.Owner, "generation": lease.Generation}, &result); err != nil {
			return err
		}
		reconciled = true
	}
	if reconciled {
		return s.clearTicket()
	}
	return nil
}

func (s *Service) fileFindings(snapshot localclient.Snapshot) []policy.Finding {
	var findings []policy.Finding
	expected := make(map[string]string)
	acknowledgedMissing := make(map[string]bool, len(snapshot.AcknowledgedMissingBackupIDs))
	for _, backupID := range snapshot.AcknowledgedMissingBackupIDs {
		acknowledgedMissing[backupID] = true
	}
	for _, backup := range snapshot.Backups {
		var path string
		switch backup.Status {
		case model.BackupReceiving:
			expected["backups/"+backup.ID+".backup"] = backup.ID
			continue
		case model.BackupComplete, model.BackupDeleting:
			path = filepath.Join(s.Root, "backups", backup.ID+".backup")
			expected["backups/"+backup.ID+".backup"] = backup.ID
		case model.BackupQuarantined, model.BackupPurging:
			path = filepath.Join(s.Root, "quarantine", backup.ID+".backup")
			expected["quarantine/"+backup.ID+".backup"] = backup.ID
		default:
			continue
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			if !acknowledgedMissing[backup.ID] {
				findings = append(findings, policy.Finding{StableKey: "file-missing:" + backup.ID, Kind: "file_missing", KeyID: backup.KeyID, BackupID: backup.ID, Detail: "file registrato nel catalogo ma fisicamente assente"})
			}
			continue
		} else if err != nil || !validFile(path, backup) {
			findings = append(findings, policy.Finding{StableKey: "catalog-file:" + backup.ID, Kind: "catalog", KeyID: backup.KeyID, BackupID: backup.ID, Detail: "catalogo e file non concordano"})
		}
	}
	for _, directory := range []string{"backups", "quarantine"} {
		entries, err := os.ReadDir(filepath.Join(s.Root, directory))
		if err != nil {
			findings = append(findings, policy.Finding{StableKey: "catalog-directory:" + directory, Kind: "catalog", Detail: err.Error()})
			continue
		}
		for _, entry := range entries {
			relative := directory + "/" + entry.Name()
			if _, ok := expected[relative]; ok {
				continue
			}
			findings = append(findings, policy.Finding{StableKey: "unexpected-file:" + relative, Kind: "catalog", Detail: "file inatteso o nella posizione errata: " + relative})
		}
	}
	return findings
}

func (s *Service) verifyUnexpectedFiles(findings []policy.Finding, snapshot localclient.Snapshot) []policy.Finding {
	expected := make(map[string]model.Backup)
	for _, backup := range snapshot.Backups {
		switch backup.Status {
		case model.BackupReceiving, model.BackupComplete, model.BackupDeleting:
			expected["backups/"+backup.ID+".backup"] = backup
		case model.BackupQuarantined, model.BackupPurging:
			expected["quarantine/"+backup.ID+".backup"] = backup
		}
	}
	result := findings[:0]
	for _, finding := range findings {
		const prefix = "unexpected-file:"
		if finding.Kind != "catalog" || !strings.HasPrefix(finding.StableKey, prefix) {
			result = append(result, finding)
			continue
		}
		relative := strings.TrimPrefix(finding.StableKey, prefix)
		backup, exists := expected[relative]
		if !exists {
			result = append(result, finding)
			continue
		}
		if !validFile(filepath.Join(s.Root, filepath.FromSlash(relative)), backup) {
			result = append(result, finding)
		}
	}
	return result
}

func recoverableBackups(snapshot localclient.Snapshot) []model.Backup {
	missing := make(map[string]bool, len(snapshot.AcknowledgedMissingBackupIDs))
	for _, backupID := range snapshot.AcknowledgedMissingBackupIDs {
		missing[backupID] = true
	}
	result := make([]model.Backup, 0, len(snapshot.Backups))
	for _, backup := range snapshot.Backups {
		if !missing[backup.ID] {
			result = append(result, backup)
		}
	}
	return result
}

func observations(backups []model.Backup, exclusions []model.AnomalyExclusion) []policy.Observation {
	var result []policy.Observation
	for _, backup := range backups {
		if backup.ReceivedAt == "" || backup.Status == model.BackupReceiving || backup.Status == model.BackupFailed {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, backup.ReceivedAt)
		if err != nil {
			continue
		}
		excluded := false
		for _, exclusion := range exclusions {
			if exclusion.KeyID == backup.KeyID && at.Unix() >= exclusion.StartsAt && at.Unix() <= exclusion.EndsAt {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		result = append(result, policy.Observation{BackupID: backup.ID, KeyID: backup.KeyID, At: at, Size: backup.Size})
	}
	return result
}

func (s *Service) monitor(ctx context.Context, snapshot localclient.Snapshot, checkID int64, now time.Time) ([]policy.Finding, bool, uint64, uint64, error) {
	if snapshot.Status.Timezone == "" {
		return nil, false, 0, 0, nil
	}
	all := observations(snapshot.Backups, snapshot.Exclusions)
	models := make(map[string]policy.KeyModel)
	for keyID, raw := range snapshot.Models {
		var learned policy.KeyModel
		if json.Unmarshal(raw, &learned) == nil {
			models[keyID] = learned
		}
	}
	revision := snapshot.Status.ModelRevision
	if revision == 0 {
		revision = now.UnixNano()
	}
	allReliable := true
	activeKeys := 0
	for _, key := range snapshot.Keys {
		if key.Revoked {
			continue
		}
		activeKeys++
		learned, exists := models[key.ID]
		if !exists {
			var err error
			learned, err = policy.Learn(key.ID, snapshot.Status.Timezone, all, now)
			if err != nil {
				return nil, false, 0, 0, err
			}
			request := map[string]any{"key_id": key.ID, "timezone": snapshot.Status.Timezone, "revision": revision,
				"retention_days": max(snapshot.Status.RetentionDaysForKey(key.ID), policy.MinimumDays), "model": learned,
				"reliable": learned.Reliable, "review": learned.ReviewRequired}
			if err = s.call(ctx, "POST", "/v1/model/save", request, nil); err != nil {
				return nil, false, 0, 0, err
			}
			models[key.ID] = learned
		}
		if !learned.Reliable || learned.ReviewRequired {
			allReliable = false
		}
	}
	if activeKeys == 0 {
		allReliable = false
	}
	var findings []policy.Finding
	var planned model.MonitoringCheck
	if err := s.call(ctx, "POST", "/v1/check/plan", map[string]int64{"id": checkID}, &planned); err != nil {
		return nil, false, 0, 0, err
	}
	for _, scope := range planned.Models {
		var learned policy.KeyModel
		if err := json.Unmarshal(scope.Model, &learned); err != nil {
			return nil, false, 0, 0, err
		}
		findings = append(findings, policy.CheckPeriod(learned, all, time.Unix(scope.CoveredFrom, 0).UTC(), time.Unix(scope.CoveredTo, 0).UTC(), now)...)
	}
	quotaByKey := make(map[string]model.Quota)
	for _, quota := range snapshot.Quotas {
		quotaByKey[quota.KeyID] = quota
	}
	forecastModels := make([]policy.KeyModel, 0, len(models))
	for _, key := range snapshot.Keys {
		if key.Revoked {
			continue
		}
		learned := models[key.ID]
		if !learned.Reliable || learned.ReviewRequired {
			continue
		}
		forecastModels = append(forecastModels, learned)
		nextSize, forecastErr := policy.ForecastNextBackup(learned, now, all)
		if forecastErr != nil {
			return nil, false, 0, 0, forecastErr
		}
		if quota, ok := quotaByKey[key.ID]; ok {
			available := quota.Profile.TotalBytes - quota.Used - quota.Reserved
			if available < 0 || uint64(available) < nextSize {
				findings = append(findings, policy.Finding{StableKey: "quota:" + key.ID, Kind: "capacity", KeyID: key.ID, Detail: fmt.Sprintf("quota insufficiente per il prossimo backup previsto: chiave %s, disponibili %d byte, previsti %d byte", key.ID, available, nextSize)})
			}
		}
	}
	if allReliable {
		var required uint64
		for _, learned := range forecastModels {
			forecast, err := policy.Forecast48Hours([]policy.KeyModel{learned}, now)
			if err != nil {
				return nil, false, 0, 0, err
			}
			daily := policy.DailyRequirement(forecast)
			days := snapshot.Status.RetentionDaysForKey(learned.KeyID)
			if days < policy.MinimumDays || days > 365000 {
				return nil, false, 0, 0, errors.New("minimo di conservazione non valido")
			}
			span := uint64(days + 3)
			if daily > (^uint64(0)-required)/span {
				return nil, false, 0, 0, errors.New("previsione spazio non rappresentabile")
			}
			required += daily * span
		}
		expected48h, forecastErr := policy.Forecast48Hours(forecastModels, now)
		if forecastErr != nil {
			return nil, false, 0, 0, forecastErr
		}
		return findings, true, expected48h, required, nil
	}
	return findings, false, 0, 0, nil
}

func (s *Service) recordFindings(ctx context.Context, snapshot localclient.Snapshot, findings []policy.Finding) error {
	current := make(map[string]policy.Finding)
	for _, finding := range findings {
		current[finding.StableKey] = finding
	}
	for _, anomaly := range snapshot.Anomalies {
		if anomaly.Kind != "schedule_missing" && anomaly.Kind != "file_missing" && anomaly.Kind != "extra" && anomaly.Kind != "size" && anomaly.Kind != "catalog" && anomaly.Kind != "filesystem" && anomaly.Kind != "model" && anomaly.Kind != "capacity" {
			continue
		}
		if _, exists := current[anomaly.StableKey]; exists {
			continue
		}
		if anomaly.Kind == "schedule_missing" || anomaly.Kind == "file_missing" || anomaly.Kind == "extra" {
			continue
		}
		if err := s.call(ctx, "POST", "/v1/anomaly/resolve", map[string]string{"stable_key": anomaly.StableKey}, nil); err != nil {
			return err
		}
	}
	return nil
}

func findingsSummary(findings []policy.Finding) string {
	if len(findings) == 0 {
		return "nessuna anomalia"
	}
	counts := make(map[string]int)
	for _, finding := range findings {
		counts[finding.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%s=%d", kind, counts[kind]))
	}
	return fmt.Sprintf("%d anomalie: %s", len(findings), strings.Join(parts, ", "))
}

func (s *Service) openFinding(ctx context.Context, finding policy.Finding) error {
	return s.call(ctx, "POST", "/v1/anomaly/open", finding, nil)
}

func (s *Service) schedulePurges(ctx context.Context, snapshot localclient.Snapshot, now time.Time) error {
	revoked := make(map[string]bool, len(snapshot.Keys))
	for _, key := range snapshot.Keys {
		revoked[key.ID] = key.Revoked
	}
	for _, backup := range recoverableBackups(snapshot) {
		if backup.Status != model.BackupQuarantined || revoked[backup.KeyID] || backup.PurgeNotBefore == 0 || now.Unix() < backup.PurgeNotBefore {
			continue
		}
		protected, err := policy.RetentionProtected(backup, now, snapshot.Status.Timezone, snapshot.Status.RetentionDaysForKey(backup.KeyID))
		if err != nil {
			return err
		}
		if protected {
			continue
		}
		request := map[string]any{"backup_id": backup.ID, "origin": "automatic", "actor": s.Owner, "reason": "quarantena minima completata"}
		if err := s.call(ctx, "POST", "/v1/purge/request", request, nil); err != nil && !strings.Contains(err.Error(), "UNIQUE") {
			return err
		}
	}
	return nil
}

func (s *Service) scheduleAutomatic(ctx context.Context, snapshot localclient.Snapshot, filesystem policy.Filesystem, expected48h uint64, now time.Time) error {
	revoked := make(map[string]bool)
	for _, key := range snapshot.Keys {
		revoked[key.ID] = key.Revoked
	}
	var pending uint64
	for _, backup := range recoverableBackups(snapshot) {
		if backup.Status == model.BackupDeleting || backup.Status == model.BackupQuarantined || backup.Status == model.BackupPurging {
			protected, err := policy.RetentionProtected(backup, now, snapshot.Status.Timezone, snapshot.Status.RetentionDaysForKey(backup.KeyID))
			if err != nil {
				return err
			}
			if !protected && !revoked[backup.KeyID] {
				pending += uint64(backup.Size)
			}
		}
	}
	selection, err := policy.Select(policy.SelectionInput{Now: now, Timezone: snapshot.Status.Timezone, Filesystem: filesystem,
		ThresholdBasisPoints: snapshot.Status.ThresholdBasis, RetentionDays: snapshot.Status.RetentionDays, KeyRetentionDays: snapshot.Status.KeyRetentionDays,
		Expected48hBytes: expected48h, PendingPurgeBytes: pending, Backups: recoverableBackups(snapshot), RevokedKeys: revoked})
	if err != nil {
		return err
	}
	for _, backup := range selection.Candidates {
		request := map[string]any{"backup_id": backup.ID, "origin": "automatic", "actor": s.Owner, "reason": "filesystem almeno all'80%; lotto minimo 10%; giorno completo oltre il minimo della chiave"}
		if err := s.call(ctx, "POST", "/v1/quarantine/request", request, nil); err != nil {
			return err
		}
	}
	if selection.ShortfallBytes != 0 {
		request := map[string]any{
			"stable_id": fmt.Sprintf("retention-shortfall:%s", now.Format("2006-01-02")),
			"subject":   "OnlyBackup: anomalia spazio liberabile insufficiente",
			"body": fmt.Sprintf("La retention richiede %d byte, puo selezionarne %d e ne mancano %d. Tutti i candidati consentiti sono stati richiesti; nessun backup protetto e stato toccato.",
				selection.BytesNeeded, selection.SelectedBytes, selection.ShortfallBytes),
		}
		if err := s.call(ctx, "POST", "/v1/report/queue", request, nil); err != nil {
			return err
		}
	}
	return nil
}

var errLeaseLost = errors.New("lease persa prima dell'operazione fisica")

func (s *Service) processOperations(ctx context.Context, snapshot localclient.Snapshot, guard *leaseGuard) error {
	byID := make(map[string]model.Backup)
	for _, backup := range snapshot.Backups {
		byID[backup.ID] = backup
	}
	for _, op := range snapshot.Operations {
		if op.State != "requested" && !(op.State == "authorized" && op.ExecutionCommitted) {
			continue
		}
		if err := guard.renew(ctx); err != nil {
			return fmt.Errorf("%w: %v", errLeaseLost, err)
		}
		lease := guard.identity()
		var authorized model.RetentionOperation
		if err := s.call(ctx, "POST", "/v1/authorize", map[string]any{"id": op.ID, "owner": lease.Owner, "generation": lease.Generation}, &authorized); err != nil {
			if op.ExecutionCommitted {
				return err // do not pass unfinished final permissions silently
			}
			// A block or active upload postpones work; it is not a physical failure.
			continue
		}
		backup, ok := byID[op.BackupID]
		if !ok {
			return errors.New("operazione riferita a backup assente dallo snapshot")
		}
		if err := s.saveTicket(authorized); err != nil {
			return err
		}
		err := s.executeLeased(ctx, guard, authorized, backup)
		if errors.Is(err, errLeaseLost) {
			return err
		}
		lease = guard.identity()
		request := map[string]any{"id": authorized.ID, "ticket": authorized.Ticket, "done": err == nil, "detail": errorText(err), "owner": lease.Owner, "generation": lease.Generation}
		if confirmErr := s.call(ctx, "POST", "/v1/confirm", request, nil); confirmErr != nil {
			return confirmErr
		}
		if err != nil {
			return err
		}
		if err = s.clearTicket(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) executeLeased(ctx context.Context, guard *leaseGuard, op model.RetentionOperation, backup model.Backup) error {
	if !model.ValidID(backup.ID) || op.BackupID != backup.ID {
		return errors.New("identita backup non valida")
	}
	archive := filepath.Join(s.Root, "backups", backup.ID+".backup")
	quarantine := filepath.Join(s.Root, "quarantine", backup.ID+".backup")
	checkpoint := func() error {
		if op.ExecutionCommitted {
			return nil // reconciled work already has a durable final permission
		}
		if err := guard.renew(ctx); err != nil {
			return fmt.Errorf("%w: %v", errLeaseLost, err)
		}
		lease := guard.identity()
		request := map[string]any{"id": op.ID, "ticket": op.Ticket, "owner": lease.Owner, "generation": lease.Generation}
		if err := s.call(ctx, "POST", "/v1/validate", request, nil); err != nil {
			return fmt.Errorf("%w: %v", errLeaseLost, err)
		}
		return nil
	}
	switch op.Kind {
	case "quarantine":
		if err := verifyExclusiveSource(archive, quarantine, backup); err != nil {
			return err
		}
		if err := checkpoint(); err != nil {
			return err
		}
		if err := verifyExclusiveSourceIdentity(archive, quarantine, backup); err != nil {
			return err
		}
		if err := renameNoReplace(archive, quarantine); err != nil {
			return err
		}
		return syncDirectories(filepath.Dir(archive), filepath.Dir(quarantine))
	case "recover":
		if err := verifyExclusiveSource(quarantine, archive, backup); err != nil {
			return err
		}
		if err := checkpoint(); err != nil {
			return err
		}
		if err := verifyExclusiveSourceIdentity(quarantine, archive, backup); err != nil {
			return err
		}
		if err := renameNoReplace(quarantine, archive); err != nil {
			return err
		}
		return syncDirectories(filepath.Dir(quarantine), filepath.Dir(archive))
	case "purge":
		if err := verifyExclusiveSource(quarantine, archive, backup); err != nil {
			return err
		}
		if err := unlinkVerifiedBefore(quarantine, backup, checkpoint); err != nil {
			return err
		}
		return syncDirectories(filepath.Dir(quarantine))
	default:
		return errors.New("tipo operazione fisica non valido")
	}
}

func (s *Service) execute(op model.RetentionOperation, backup model.Backup) error {
	if !model.ValidID(backup.ID) || op.BackupID != backup.ID {
		return errors.New("identita backup non valida")
	}
	archive := filepath.Join(s.Root, "backups", backup.ID+".backup")
	quarantine := filepath.Join(s.Root, "quarantine", backup.ID+".backup")
	switch op.Kind {
	case "quarantine":
		if err := verifyExclusiveSource(archive, quarantine, backup); err != nil {
			return err
		}
		if err := renameNoReplace(archive, quarantine); err != nil {
			return err
		}
		return syncDirectories(filepath.Dir(archive), filepath.Dir(quarantine))
	case "recover":
		if err := verifyExclusiveSource(quarantine, archive, backup); err != nil {
			return err
		}
		if err := renameNoReplace(quarantine, archive); err != nil {
			return err
		}
		return syncDirectories(filepath.Dir(quarantine), filepath.Dir(archive))
	case "purge":
		if err := verifyExclusiveSource(quarantine, archive, backup); err != nil {
			return err
		}
		if err := unlinkVerified(quarantine, backup); err != nil {
			return err
		}
		return syncDirectories(filepath.Dir(quarantine))
	default:
		return errors.New("tipo operazione fisica non valido")
	}
}

func verifyExclusiveSource(source, destination string, backup model.Backup) error {
	if !validFile(source, backup) {
		return errors.New("file sorgente assente, simbolico o con identita errata")
	}
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("collisione: file destinazione gia presente")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func verifyExclusiveSourceIdentity(source, destination string, backup model.Backup) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != backup.Size {
		return errors.New("file sorgente assente, simbolico o con identita errata")
	}
	if _, err = os.Lstat(destination); err == nil {
		return errors.New("collisione: file destinazione gia presente")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func validFile(path string, backup model.Backup) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != backup.Size {
		return false
	}
	return backupfile.Verify(path, backup.Size, backup.SHA256) == nil
}

func renameNoReplace(source, destination string) error {
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) {
		return err
	}
	if err = os.Link(source, destination); err != nil {
		return err
	}
	if err = syncDirectories(filepath.Dir(destination)); err != nil {
		return err
	}
	if err = os.Remove(source); err != nil {
		return err
	}
	return nil
}

func unlinkVerified(path string, backup model.Backup) error {
	return unlinkVerifiedBefore(path, backup, nil)
}

func unlinkVerifiedBefore(path string, backup model.Backup, beforeRemove func() error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != backup.Size {
		return errors.New("file da eliminare non valido")
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, pathInfo) || pathInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("file sostituito durante la verifica")
	}
	if err = backupfile.Verify(path, backup.Size, backup.SHA256); err != nil {
		return err
	}
	if beforeRemove != nil {
		if err = beforeRemove(); err != nil {
			return err
		}
		current, currentErr := os.Lstat(path)
		if currentErr != nil || !os.SameFile(info, current) || current.Mode()&os.ModeSymlink != 0 {
			return errors.New("file sostituito dopo la verifica")
		}
	}
	return os.Remove(path)
}

func syncDirectories(paths ...string) error {
	seen := make(map[string]bool)
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		directory, err := os.Open(path)
		if err != nil {
			return err
		}
		err = directory.Sync()
		closeErr := directory.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (s *Service) deliverMail(ctx context.Context, settings model.MailSettings, now time.Time) error {
	if s.Mail == nil || settings.Host == "" {
		return nil
	}
	var messages []model.MailMessage
	if err := s.call(ctx, "GET", "/v1/mail/due", nil, &messages); err != nil {
		return err
	}
	for _, message := range messages {
		err := s.Mail.Send(ctx, settings, message)
		if confirmErr := s.call(ctx, "POST", "/v1/mail/confirm", map[string]any{"id": message.ID, "error": errorText(err)}, nil); confirmErr != nil {
			return confirmErr
		}
		if err != nil {
			_ = s.openFinding(ctx, policy.Finding{StableKey: "smtp:delivery", Kind: "smtp", Detail: err.Error()})
			continue
		}
		_ = s.call(ctx, "POST", "/v1/anomaly/resolve", map[string]string{"stable_key": "smtp:delivery"}, nil)
	}
	return nil
}

type SMTP struct {
	CredentialsPath string
	Timeout         time.Duration
}

type smtpCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (sender SMTP) Send(ctx context.Context, settings model.MailSettings, message model.MailMessage) error {
	var credentials smtpCredentials
	if sender.CredentialsPath != "" {
		info, err := os.Stat(sender.CredentialsPath)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0077 != 0 {
			return errors.New("file credenziali mail accessibile ad altri utenti")
		}
		data, err := os.ReadFile(sender.CredentialsPath)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &credentials); err != nil {
			return errors.New("file credenziali mail non valido")
		}
	}
	address := net.JoinHostPort(settings.Host, fmt.Sprint(settings.Port))
	dialer := &net.Dialer{Timeout: sender.Timeout}
	var connection net.Conn
	var err error
	if settings.TLS && settings.Port == 465 {
		connection, err = tls.DialWithDialer(dialer, "tcp", address, &tls.Config{ServerName: settings.Host, MinVersion: tls.VersionTLS12})
	} else {
		connection, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer connection.Close()
	if sender.Timeout > 0 {
		_ = connection.SetDeadline(time.Now().Add(sender.Timeout))
	}
	client, err := smtp.NewClient(connection, settings.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if settings.TLS && settings.Port != 465 {
		if err = client.StartTLS(&tls.Config{ServerName: settings.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if credentials.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", credentials.Username, credentials.Password, settings.Host)); err != nil {
			return err
		}
	}
	from, err := netmail.ParseAddress(settings.From)
	if err != nil {
		return err
	}
	if err = client.Mail(from.Address); err != nil {
		return err
	}
	recipients := splitRecipients(settings.Recipients)
	for _, recipient := range recipients {
		if err = client.Rcpt(recipient); err != nil {
			return err
		}
	}
	wc, err := client.Data()
	if err != nil {
		return err
	}
	body, err := composeMail(settings.From, recipients, message, time.Now())
	if err != nil {
		wc.Close()
		return err
	}
	if _, err = wc.Write([]byte(body)); err != nil {
		wc.Close()
		return err
	}
	if err = wc.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func splitRecipients(value string) []string {
	addresses, _ := netmail.ParseAddressList(strings.ReplaceAll(value, ";", ","))
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, address.Address)
	}
	return result
}

func sanitizeHeader(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type localState struct {
	Settings            model.MailSettings `json:"mail_settings"`
	UpdatedAt           int64              `json:"updated_at_unix"`
	WriterFailureOpened int64              `json:"writer_failure_opened_unix,omitempty"`
	WriterFailureMailed int64              `json:"writer_failure_mailed_unix,omitempty"`
}

func (s *Service) saveLocalState(settings model.MailSettings) error {
	directory := filepath.Join(s.Root, "maintenance")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	state, _ := s.loadLocalState()
	state.Settings = settings
	state.UpdatedAt = s.Now().Unix()
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicPrivateFile(filepath.Join(directory, "state.json"), data)
}

func (s *Service) loadLocalState() (localState, error) {
	var state localState
	data, err := os.ReadFile(filepath.Join(s.Root, "maintenance", "state.json"))
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}

// NotifyWriterFailure uses the last cached non-secret mail configuration so a
// writer outage can be reported without reading SQLite. Delivery state is
// persisted locally and reminders are limited to once every 24 hours.
func (s *Service) NotifyWriterFailure(ctx context.Context, failure error) error {
	if s.Mail == nil {
		return nil
	}
	state, err := s.loadLocalState()
	if err != nil {
		return err
	}
	now := s.Now().UTC()
	if state.WriterFailureOpened == 0 {
		state.WriterFailureOpened = now.Unix()
	}
	if state.WriterFailureMailed != 0 && now.Unix()-state.WriterFailureMailed < int64(24*time.Hour/time.Second) {
		return nil
	}
	message := model.MailMessage{StableID: fmt.Sprintf("writer-down:%d", state.WriterFailureOpened), Kind: "anomaly", Subject: "OnlyBackup: writer non disponibile", Body: failure.Error()}
	if err = s.Mail.Send(ctx, state.Settings, message); err != nil {
		return err
	}
	state.WriterFailureMailed = now.Unix()
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicPrivateFile(filepath.Join(s.Root, "maintenance", "state.json"), data)
}

// WriterUnavailable distinguishes transport failures from policy, filesystem,
// SMTP and catalogue errors so the emergency notification is not misleading.
func WriterUnavailable(err error) bool {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return false
	}
	var netErr *net.OpError
	return errors.As(urlErr.Err, &netErr) || errors.Is(urlErr.Err, os.ErrNotExist) || errors.Is(urlErr.Err, syscall.ECONNREFUSED)
}

func (s *Service) reconcileWriterFailure(ctx context.Context) error {
	state, err := s.loadLocalState()
	if err != nil || state.WriterFailureOpened == 0 {
		return nil
	}
	finding := policy.Finding{StableKey: fmt.Sprintf("writer-unavailable:%d", state.WriterFailureOpened), Kind: "service", Detail: "writer non disponibile durante il periodo registrato dallo stato locale maintenance"}
	if err = s.openFinding(ctx, finding); err != nil {
		return err
	}
	if err = s.call(ctx, "POST", "/v1/anomaly/resolve", map[string]string{"stable_key": finding.StableKey}, nil); err != nil {
		return err
	}
	state.WriterFailureOpened, state.WriterFailureMailed = 0, 0
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return atomicPrivateFile(filepath.Join(s.Root, "maintenance", "state.json"), data)
}

func (s *Service) saveTicket(op model.RetentionOperation) error {
	data, err := json.Marshal(op)
	if err != nil {
		return err
	}
	return atomicPrivateFile(filepath.Join(s.Root, "maintenance", "active-ticket.json"), data)
}

func (s *Service) clearTicket() error {
	path := filepath.Join(s.Root, "maintenance", "active-ticket.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectories(filepath.Dir(path))
}

func atomicPrivateFile(path string, data []byte) error {
	temp := path + ".tmp"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		_ = os.Remove(temp)
		file, err = os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			_ = os.Remove(temp)
		}
	}()
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	if err = syncDirectories(filepath.Dir(path)); err != nil {
		return err
	}
	ok = true
	return nil
}
