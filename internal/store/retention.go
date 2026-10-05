package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

const quarantineMinimum = 48 * time.Hour

type MailSettings = model.MailSettings

func (s *Store) AutomationStatus() (model.AutomationStatus, error) {
	var v model.AutomationStatus
	err := s.db.QueryRow(`SELECT enabled,mail_tested,monitoring_state,(deletion_blocked OR manual_paused),block_reason,manual_paused,manual_pause_reason,
 timezone,retention_days,threshold_basis_points,reserve_free,model_revision,last_check_at,next_report_at,
 (SELECT COUNT(*) FROM anomalies WHERE resolved_at=0) FROM automation_state WHERE id=1`).Scan(
		&v.Enabled, &v.MailTested, &v.MonitoringState, &v.DeletionBlocked, &v.BlockReason, &v.ManualPaused, &v.ManualPauseReason,
		&v.Timezone, &v.RetentionDays, &v.ThresholdBasis, &v.ReserveFree, &v.ModelRevision, &v.LastCheckAt,
		&v.NextReportAt, &v.ActiveAnomalies)
	if err != nil {
		return v, err
	}
	if v.ManualPaused {
		v.BlockReason = v.ManualPauseReason
	}
	v.KeyRetentionDays = make(map[string]int)
	rows, err := s.db.Query(`SELECT k.id,COALESCE(r.days,a.retention_days) FROM keys k CROSS JOIN automation_state a LEFT JOIN key_retention r ON r.key_id=k.id WHERE a.id=1`)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var days int
		if err = rows.Scan(&id, &days); err != nil {
			return v, err
		}
		v.KeyRetentionDays[id] = days
	}
	if err = rows.Err(); err != nil {
		return v, err
	}
	// Preserve the writer's real check state before presenting a regular
	// but blocked system as suspended to administrators and reports.
	v.LastCheckRegular = v.MonitoringState == model.MonitoringRegular
	if v.DeletionBlocked && v.MonitoringState == model.MonitoringRegular {
		v.MonitoringState = model.MonitoringPaused
	}
	return v, err
}

func (s *Store) SetReserveFree(bytes int64) error {
	if bytes < 0 {
		return errors.New("riserva disco non valida")
	}
	_, err := s.db.Exec(`UPDATE automation_state SET reserve_free=? WHERE id=1`, bytes)
	return err
}

// SetKeyRetentionDays changes only the administrator-owned policy, never the model.
func (s *Store) SetKeyRetentionDays(keyID string, days int) error {
	if !model.ValidID(keyID) || days < 7 || days > 365000 {
		return errors.New("chiave o conservazione non valida: da 7 a 365000 giorni")
	}
	_, err := s.db.Exec(`INSERT INTO key_retention(key_id,days) VALUES(?,?) ON CONFLICT(key_id) DO UPDATE SET days=excluded.days`, keyID, days)
	return err
}

// Check again under the same transaction as each new destructive permission.
func checkRetentionMinimumTx(tx *sql.Tx, backupID string, now time.Time) error {
	var received, timezone string
	var started int64
	var days int
	err := tx.QueryRow(`SELECT b.received_at,b.started_at,a.timezone,COALESCE(r.days,a.retention_days)
 FROM backups b CROSS JOIN automation_state a LEFT JOIN key_retention r ON r.key_id=b.key_id
 WHERE b.id=? AND a.id=1`, backupID).Scan(&received, &started, &timezone, &days)
	if err != nil {
		return err
	}
	protected, err := policy.RetentionProtected(model.Backup{Receipt: model.Receipt{ReceivedAt: received}, StartedAt: started}, now, timezone, days)
	if err != nil {
		return err
	}
	if protected {
		return model.ErrRetentionMinimum
	}
	return nil
}

func (s *Store) MailSettings() (MailSettings, error) {
	var v MailSettings
	err := s.db.QueryRow(`SELECT smtp_host,smtp_port,smtp_tls,mail_from,mail_to FROM automation_state WHERE id=1`).Scan(
		&v.Host, &v.Port, &v.TLS, &v.From, &v.Recipients)
	return v, err
}

func (s *Store) SetupAutomation(settings MailSettings, timezone string, now time.Time) error {
	if strings.TrimSpace(settings.Host) == "" || settings.Port < 1 || settings.Port > 65535 ||
		strings.TrimSpace(settings.From) == "" || strings.TrimSpace(settings.Recipients) == "" {
		return errors.New("configurazione mail incompleta")
	}
	if strings.ContainsAny(settings.Host+settings.From+settings.Recipients, "\r\n") || strings.ContainsAny(settings.Host, " \t") {
		return errors.New("configurazione mail contiene caratteri non validi")
	}
	if _, err := mail.ParseAddress(settings.From); err != nil {
		return errors.New("mittente mail non valido")
	}
	if _, err := mail.ParseAddressList(settings.Recipients); err != nil {
		return errors.New("destinatari mail non validi")
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return fmt.Errorf("fuso non valido: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM retention_operations WHERE state IN ('requested','authorized')`).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return errors.New("configurazione rifiutata: completare, riconciliare o annullare le operazioni pendenti")
	}
	_, err = tx.Exec(`UPDATE automation_state SET smtp_host=?,smtp_port=?,smtp_tls=?,mail_from=?,mail_to=?,
	 timezone=?,enabled=0,mail_tested=0,monitoring_state='APPRENDIMENTO',deletion_blocked=1,
	 block_reason='prova mail e apprendimento richiesti',model_revision=0,next_report_at=? WHERE id=1`, settings.Host,
		settings.Port, settings.TLS, settings.From, settings.Recipients, timezone, now.Add(72*time.Hour).Unix())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkMailTested(now time.Time) error {
	_, err := s.db.Exec(`UPDATE automation_state SET mail_tested=1,next_report_at=CASE WHEN next_report_at=0 THEN ? ELSE next_report_at END WHERE id=1`, now.Add(72*time.Hour).Unix())
	return err
}

func (s *Store) EnableAutomation() error {
	status, err := s.AutomationStatus()
	if err != nil {
		return err
	}
	if !status.MailTested {
		return errors.New("la prova mail deve riuscire prima di abilitare l'automazione")
	}
	_, err = s.db.Exec(`UPDATE automation_state SET enabled=1 WHERE id=1`)
	return err
}

func (s *Store) PauseRetention(reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "sospensione amministrativa"
	}
	_, err := s.db.Exec(`UPDATE automation_state SET manual_paused=1,manual_pause_reason=? WHERE id=1`, reason)
	return err
}

func (s *Store) ResumeRetention(now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	var state string
	var lastCheck, revision int64
	if err = tx.QueryRow(`SELECT monitoring_state,last_check_at,model_revision FROM automation_state WHERE id=1`).Scan(&state, &lastCheck, &revision); err != nil {
		return err
	}
	if active, err = blockingAnomalyCountTx(tx, now); err != nil {
		return err
	}
	var uncertain int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM retention_operations WHERE state='authorized'`).Scan(&uncertain); err != nil {
		return err
	}
	if active != 0 || uncertain != 0 || revision == 0 || lastCheck == 0 || lastCheck > now.Unix()+1 || now.Unix()-lastCheck > int64(10*time.Minute/time.Second) || state != model.MonitoringRegular {
		return errors.New("riattivazione rifiutata: controllo recente regolare, modello affidabile e nessuna operazione incerta richiesti")
	}
	if _, err = tx.Exec(`UPDATE automation_state SET deletion_blocked=0,block_reason='',manual_paused=0,manual_pause_reason='' WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginRelearn() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM retention_operations WHERE state IN ('requested','authorized')`).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return errors.New("relearn rifiutato: completare, riconciliare o annullare le operazioni pendenti")
	}
	_, err = tx.Exec(`UPDATE automation_state SET monitoring_state='APPRENDIMENTO',deletion_blocked=1,block_reason='nuovo apprendimento richiesto',model_revision=model_revision+1 WHERE id=1`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func backupTime(b model.Backup) time.Time {
	if b.ReceivedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, b.ReceivedAt); err == nil {
			return t
		}
	}
	return time.Unix(b.StartedAt, 0).UTC()
}

func (s *Store) RequestQuarantine(backupID, origin, actor, reason string, now time.Time) (model.RetentionOperation, error) {
	var out model.RetentionOperation
	if !model.ValidID(backupID) || (origin != "manual" && origin != "automatic") || strings.TrimSpace(actor) == "" {
		return out, errors.New("richiesta retention non valida")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var blocked bool
	var blockReason, timezone string
	var revision int64
	if err = tx.QueryRow(`SELECT (deletion_blocked OR manual_paused),CASE WHEN manual_paused THEN manual_pause_reason ELSE block_reason END,timezone,model_revision FROM automation_state WHERE id=1`).Scan(&blocked, &blockReason, &timezone, &revision); err != nil {
		return out, err
	}
	if blocked {
		return out, fmt.Errorf("%w: %s", model.ErrRetentionBlocked, blockReason)
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return out, errors.New("fuso dell'automazione non valido")
	}
	var status, received string
	var started int64
	var keyID string
	var keyRevoked bool
	if err = tx.QueryRow(`SELECT b.status,b.received_at,b.started_at,b.key_id,k.revoked FROM backups b JOIN keys k ON k.id=b.key_id WHERE b.id=?`, backupID).Scan(&status, &received, &started, &keyID, &keyRevoked); err != nil {
		return out, err
	}
	if keyRevoked {
		return out, errors.New("i backup di una chiave revocata non sono selezionabili")
	}
	if status != model.BackupComplete {
		return out, fmt.Errorf("backup non selezionabile nello stato %s", status)
	}
	var acknowledgedMissing int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM anomalies WHERE kind='file_missing' AND backup_id=? AND acknowledged_at<>0`, backupID).Scan(&acknowledgedMissing); err != nil {
		return out, err
	}
	if acknowledgedMissing != 0 {
		return out, errors.New("backup riconosciuto come definitivamente mancante")
	}
	b := model.Backup{Receipt: model.Receipt{ReceivedAt: received}, StartedAt: started}
	bt := backupTime(b).In(loc)
	localNow := now.In(loc)
	by, bm, bd := bt.Date()
	ny, nm, nd := localNow.Date()
	if by == ny && bm == nm && bd == nd {
		return out, model.ErrCurrentDay
	}
	var copies int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM backups b WHERE b.key_id=? AND b.status='complete'
	 AND NOT EXISTS (SELECT 1 FROM anomalies a WHERE a.kind='file_missing' AND a.backup_id=b.id AND a.acknowledged_at<>0)`, keyID).Scan(&copies); err != nil {
		return out, err
	}
	if copies <= 1 {
		return out, model.ErrLastCopy
	}
	if err = checkRetentionMinimumTx(tx, backupID, now); err != nil {
		return out, err
	}
	transition, err := tx.Exec(`UPDATE backups SET status='deleting' WHERE id=? AND status='complete'`, backupID)
	if err != nil {
		return out, err
	}
	if n, _ := transition.RowsAffected(); n != 1 {
		return out, errors.New("stato backup cambiato durante la richiesta")
	}
	res, err := tx.Exec(`INSERT INTO retention_operations(backup_id,kind,state,origin,actor,reason,revision,requested_at)
 VALUES(?,'quarantine','requested',?,?,?,?,?)`, backupID, origin, actor, reason, revision, now.Unix())
	if err != nil {
		return out, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return model.RetentionOperation{ID: id, BackupID: backupID, Kind: "quarantine", State: "requested", Origin: origin, Actor: actor, Reason: reason, Revision: revision, RequestedAt: now.Unix()}, nil
}

func (s *Store) RequestPurge(backupID, origin, actor, reason string, now time.Time) (model.RetentionOperation, error) {
	var out model.RetentionOperation
	if !model.ValidID(backupID) || (origin != "manual" && origin != "automatic" && origin != "reconcile") || strings.TrimSpace(actor) == "" {
		return out, errors.New("richiesta purge non valida")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var blocked bool
	var blockReason, status, keyID string
	var revision, notBefore int64
	if err = tx.QueryRow(`SELECT (deletion_blocked OR manual_paused),CASE WHEN manual_paused THEN manual_pause_reason ELSE block_reason END,model_revision FROM automation_state WHERE id=1`).Scan(&blocked, &blockReason, &revision); err != nil {
		return out, err
	}
	if blocked {
		return out, fmt.Errorf("%w: %s", model.ErrRetentionBlocked, blockReason)
	}
	var keyRevoked bool
	if err = tx.QueryRow(`SELECT b.status,b.purge_not_before,b.key_id,k.revoked FROM backups b JOIN keys k ON k.id=b.key_id WHERE b.id=?`, backupID).Scan(&status, &notBefore, &keyID, &keyRevoked); err != nil {
		return out, err
	}
	if keyRevoked {
		return out, errors.New("i backup di una chiave revocata non sono eliminabili")
	}
	if status != model.BackupQuarantined {
		return out, fmt.Errorf("backup non in quarantena: %s", status)
	}
	var acknowledgedMissing int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM anomalies WHERE kind='file_missing' AND backup_id=? AND acknowledged_at<>0`, backupID).Scan(&acknowledgedMissing); err != nil {
		return out, err
	}
	if acknowledgedMissing != 0 {
		return out, errors.New("backup riconosciuto come definitivamente mancante")
	}
	if err = checkRetentionMinimumTx(tx, backupID, now); err != nil {
		return out, err
	}
	if notBefore == 0 || now.Unix() < notBefore {
		return out, model.ErrTooEarly
	}
	var stableCopies int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM backups b WHERE b.key_id=? AND b.status IN ('complete','deleting','quarantined')
	 AND NOT EXISTS (SELECT 1 FROM anomalies a WHERE a.kind='file_missing' AND a.backup_id=b.id AND a.acknowledged_at<>0)`, keyID).Scan(&stableCopies); err != nil {
		return out, err
	}
	if stableCopies <= 1 {
		return out, model.ErrLastCopy
	}
	transition, err := tx.Exec(`UPDATE backups SET status='purging' WHERE id=? AND status='quarantined'`, backupID)
	if err != nil {
		return out, err
	}
	if n, _ := transition.RowsAffected(); n != 1 {
		return out, errors.New("stato backup cambiato durante la richiesta")
	}
	res, err := tx.Exec(`INSERT INTO retention_operations(backup_id,kind,state,origin,actor,reason,revision,requested_at)
 VALUES(?,'purge','requested',?,?,?,?,?)`, backupID, origin, actor, reason, revision, now.Unix())
	if err != nil {
		return out, err
	}
	id, _ := res.LastInsertId()
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return model.RetentionOperation{ID: id, BackupID: backupID, Kind: "purge", State: "requested", Origin: origin, Actor: actor, Reason: reason, Revision: revision, RequestedAt: now.Unix()}, nil
}

func (s *Store) RequestRecovery(backupID, actor, reason string, now time.Time) (model.RetentionOperation, error) {
	var out model.RetentionOperation
	if !model.ValidID(backupID) || strings.TrimSpace(actor) == "" {
		return out, errors.New("richiesta recupero non valida")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var status string
	var revision int64
	if err = tx.QueryRow(`SELECT status FROM backups WHERE id=?`, backupID).Scan(&status); err != nil {
		return out, err
	}
	if status != model.BackupQuarantined {
		return out, errors.New("solo un backup in quarantena puo essere recuperato")
	}
	if err = tx.QueryRow(`SELECT model_revision FROM automation_state WHERE id=1`).Scan(&revision); err != nil {
		return out, err
	}
	res, err := tx.Exec(`INSERT INTO retention_operations(backup_id,kind,state,origin,actor,reason,revision,requested_at)
 VALUES(?,'recover','requested','manual',?,?,?,?)`, backupID, actor, reason, revision, now.Unix())
	if err != nil {
		return out, err
	}
	id, _ := res.LastInsertId()
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return model.RetentionOperation{ID: id, BackupID: backupID, Kind: "recover", State: "requested", Origin: "manual", Actor: actor, Reason: reason, Revision: revision, RequestedAt: now.Unix()}, nil
}

func ticketHash(ticket string) string {
	h := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(h[:])
}

func (s *Store) AuthorizeOperation(operationID int64, lease model.MaintenanceLease, now time.Time) (model.RetentionOperation, error) {
	var out model.RetentionOperation
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = validateMaintenanceLeaseTx(tx, lease, now); err != nil {
		return out, err
	}
	var blocked bool
	var currentRevision int64
	if err = tx.QueryRow(`SELECT (deletion_blocked OR manual_paused),model_revision FROM automation_state WHERE id=1`).Scan(&blocked, &currentRevision); err != nil {
		return out, err
	}
	if err = scanOperation(tx.QueryRow(`SELECT id,backup_id,kind,state,origin,actor,reason,revision,requested_at,authorized_at,completed_at,error,lease_generation,execution_committed FROM retention_operations WHERE id=?`, operationID), &out); err != nil {
		return out, err
	}
	if out.State != "requested" || out.Revision != currentRevision {
		// A committed operation is not a new policy decision. After physical
		// reconciliation, only its lost transport ticket needs replacing.
		if out.State != "authorized" || !out.ExecutionCommitted {
			return out, model.ErrTicket
		}
		ticket, tokenErr := model.NewToken()
		if tokenErr != nil {
			return out, tokenErr
		}
		if err = changed(tx.Exec(`UPDATE retention_operations SET ticket_hash=?,lease_generation=? WHERE id=? AND state='authorized' AND execution_committed=1 AND ticket_hash=''`, ticketHash(ticket), lease.Generation, out.ID)); err != nil {
			return out, err
		}
		if err = tx.Commit(); err != nil {
			return out, err
		}
		out.Ticket, out.LeaseGeneration = ticket, lease.Generation
		return out, nil
	}
	if blocked && out.Kind != "recover" {
		return out, model.ErrRetentionBlocked
	}
	if out.Kind != "recover" {
		if err = checkRetentionMinimumTx(tx, out.BackupID, now); err != nil {
			return out, err
		}
		var revoked bool
		if err = tx.QueryRow(`SELECT k.revoked FROM backups b JOIN keys k ON k.id=b.key_id WHERE b.id=?`, out.BackupID).Scan(&revoked); err != nil {
			return out, err
		}
		if revoked {
			return out, errors.New("operazione distruttiva rifiutata: chiave revocata")
		}
	}
	if out.Kind == "purge" {
		var stableCopies int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM backups b WHERE b.key_id=(SELECT key_id FROM backups WHERE id=?)
		 AND b.status IN ('complete','deleting','quarantined')
		 AND NOT EXISTS (SELECT 1 FROM anomalies a WHERE a.kind='file_missing' AND a.backup_id=b.id AND a.acknowledged_at<>0)`, out.BackupID).Scan(&stableCopies); err != nil {
			return out, err
		}
		if stableCopies < 1 {
			return out, model.ErrLastCopy
		}
	}
	if out.Kind != "recover" {
		var receiving int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM backups WHERE status='receiving'`).Scan(&receiving); err != nil {
			return out, err
		}
		if receiving != 0 {
			return out, errors.New("upload in corso: autorizzazione distruttiva rinviata")
		}
	}
	ticket, err := model.NewToken()
	if err != nil {
		return out, err
	}
	res, err := tx.Exec(`UPDATE retention_operations SET state='authorized',ticket_hash=?,authorized_at=?,lease_generation=? WHERE id=? AND state='requested'`, ticketHash(ticket), now.Unix(), lease.Generation, operationID)
	if err != nil {
		return out, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return out, model.ErrTicket
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	out.State = "authorized"
	out.AuthorizedAt = now.Unix()
	out.Ticket = ticket
	out.LeaseGeneration = lease.Generation
	return out, nil
}

func (s *Store) ValidateOperation(operationID int64, ticket string, lease model.MaintenanceLease, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = validateMaintenanceLeaseTx(tx, lease, now); err != nil {
		return err
	}
	var op model.RetentionOperation
	var hash string
	if err = scanOperationWithHash(tx.QueryRow(`SELECT id,backup_id,kind,state,origin,actor,reason,revision,requested_at,authorized_at,completed_at,error,lease_generation,execution_committed,ticket_hash FROM retention_operations WHERE id=?`, operationID), &op, &hash); err != nil {
		return err
	}
	var blocked bool
	var revision int64
	if err = tx.QueryRow(`SELECT (deletion_blocked OR manual_paused),model_revision FROM automation_state WHERE id=1`).Scan(&blocked, &revision); err != nil {
		return err
	}
	if op.State != "authorized" || hash == "" || ticketHash(ticket) != hash || op.LeaseGeneration != lease.Generation {
		return model.ErrTicket
	}
	if op.ExecutionCommitted {
		return tx.Commit()
	}
	if op.Revision != revision || op.AuthorizedAt > now.Unix()+1 {
		return model.ErrTicket
	}
	if blocked && op.Kind != "recover" {
		return model.ErrRetentionBlocked
	}
	if op.Kind != "recover" {
		if err = checkRetentionMinimumTx(tx, op.BackupID, now); err != nil {
			return err
		}
		var revoked bool
		if err = tx.QueryRow(`SELECT k.revoked FROM backups b JOIN keys k ON k.id=b.key_id WHERE b.id=?`, op.BackupID).Scan(&revoked); err != nil {
			return err
		}
		if revoked {
			return errors.New("operazione distruttiva rifiutata: chiave revocata")
		}
	}
	if op.Kind != "recover" {
		var receiving int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM backups WHERE status='receiving'`).Scan(&receiving); err != nil {
			return err
		}
		if receiving != 0 {
			return errors.New("upload in corso: operazione fisica rinviata")
		}
	}
	// This transaction is the final, durable permission for this one operation.
	// A later revocation/relearn/block applies only to new permissions.
	if err = changed(tx.Exec(`UPDATE retention_operations SET execution_committed=1 WHERE id=? AND state='authorized'`, operationID)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ConfirmOperation(operationID int64, ticket string, physicalDone bool, detail string, lease model.MaintenanceLease, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = validateMaintenanceLeaseTx(tx, lease, now); err != nil {
		return err
	}
	var op model.RetentionOperation
	var hash string
	if err = scanOperationWithHash(tx.QueryRow(`SELECT id,backup_id,kind,state,origin,actor,reason,revision,requested_at,authorized_at,completed_at,error,lease_generation,execution_committed,ticket_hash FROM retention_operations WHERE id=?`, operationID), &op, &hash); err != nil {
		return err
	}
	if op.State == "confirmed" && physicalDone {
		if hash != "" && ticketHash(ticket) == hash && op.LeaseGeneration == lease.Generation {
			return nil
		}
		return model.ErrTicket
	}
	if op.State != "authorized" || hash == "" || ticketHash(ticket) != hash || op.LeaseGeneration != lease.Generation {
		return model.ErrTicket
	}
	if !physicalDone {
		if _, err = tx.Exec(`UPDATE retention_operations SET error=? WHERE id=?`, detail, op.ID); err == nil {
			err = openAnomalyTx(tx, fmt.Sprintf("maintenance-operation:%d", op.ID), "maintenance", "", op.BackupID, "errore operazione di manutenzione: "+detail, now)
		}
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	var transition sql.Result
	switch op.Kind {
	case "quarantine":
		transition, err = tx.Exec(`UPDATE backups SET status='quarantined',quarantined_at=?,purge_not_before=? WHERE id=? AND status='deleting'`, now.Unix(), purgeNotBefore(now), op.BackupID)
	case "purge":
		transition, err = tx.Exec(`UPDATE backups SET status='deleted',purged_at=? WHERE id=? AND status='purging'`, now.Unix(), op.BackupID)
	case "recover":
		transition, err = tx.Exec(`UPDATE backups SET status='complete',quarantined_at=0,purge_not_before=0 WHERE id=? AND status='quarantined'`, op.BackupID)
	default:
		return errors.New("tipo operazione non confermabile")
	}
	if err != nil {
		return err
	}
	if n, _ := transition.RowsAffected(); n != 1 {
		return errors.New("conferma rifiutata: stato del backup non coerente con l'operazione")
	}
	confirmed, err := tx.Exec(`UPDATE retention_operations SET state='confirmed',completed_at=?,error='' WHERE id=? AND state='authorized'`, now.Unix(), op.ID)
	if err != nil {
		return err
	}
	if n, _ := confirmed.RowsAffected(); n != 1 {
		return model.ErrTicket
	}
	if _, err = resolveMaintenanceOperationTx(tx, operationID, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileAuthorized confirms a physical result after the maintenance
// process lost the plaintext ticket. It is exposed only on the authenticated
// maintenance socket and must be called after the writer itself verifies the
// fixed source/destination paths and file identity.
func (s *Store) ReconcileAuthorized(operationID int64, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var op model.RetentionOperation
	if err = scanOperation(tx.QueryRow(`SELECT id,backup_id,kind,state,origin,actor,reason,revision,requested_at,authorized_at,completed_at,error,lease_generation,execution_committed FROM retention_operations WHERE id=?`, operationID), &op); err != nil {
		return err
	}
	if op.State != "authorized" {
		return model.ErrTicket
	}
	var transition sql.Result
	switch op.Kind {
	case "quarantine":
		transition, err = tx.Exec(`UPDATE backups SET status='quarantined',quarantined_at=?,purge_not_before=? WHERE id=? AND status='deleting'`, now.Unix(), purgeNotBefore(now), op.BackupID)
	case "purge":
		transition, err = tx.Exec(`UPDATE backups SET status='deleted',purged_at=? WHERE id=? AND status='purging'`, now.Unix(), op.BackupID)
	case "recover":
		transition, err = tx.Exec(`UPDATE backups SET status='complete',quarantined_at=0,purge_not_before=0 WHERE id=? AND status='quarantined'`, op.BackupID)
	default:
		return errors.New("operazione non riconciliabile")
	}
	if err != nil {
		return err
	}
	if n, _ := transition.RowsAffected(); n != 1 {
		return errors.New("riconciliazione rifiutata: stato del backup non coerente")
	}
	confirmed, err := tx.Exec(`UPDATE retention_operations SET state='confirmed',completed_at=?,error='riconciliata dopo riavvio' WHERE id=? AND state='authorized'`, now.Unix(), operationID)
	if err != nil {
		return err
	}
	if n, _ := confirmed.RowsAffected(); n != 1 {
		return model.ErrTicket
	}
	if _, err = resolveMaintenanceOperationTx(tx, operationID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func purgeNotBefore(now time.Time) int64 {
	value := now.Add(quarantineMinimum).Unix()
	if now.Nanosecond() != 0 {
		value++
	}
	return value
}

func (s *Store) ResetAuthorized(operationID int64, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var op model.RetentionOperation
	var revoked bool
	if err = scanOperation(tx.QueryRow(`SELECT o.id,o.backup_id,o.kind,o.state,o.origin,o.actor,o.reason,o.revision,o.requested_at,o.authorized_at,o.completed_at,o.error,o.lease_generation,o.execution_committed
	 FROM retention_operations o WHERE o.id=?`, operationID), &op); err != nil {
		return err
	}
	if err = tx.QueryRow(`SELECT k.revoked FROM backups b JOIN keys k ON k.id=b.key_id WHERE b.id=?`, op.BackupID).Scan(&revoked); err != nil {
		return err
	}
	if op.State != "authorized" {
		return model.ErrTicket
	}
	if op.ExecutionCommitted {
		// The caller verified that the source is intact. Preserve the final
		// permission even after revocation; invalidate only the lost ticket.
		if err = changed(tx.Exec(`UPDATE retention_operations SET ticket_hash='',lease_generation=0 WHERE id=? AND state='authorized' AND execution_committed=1`, operationID)); err != nil {
			return err
		}
		if _, err = resolveMaintenanceOperationTx(tx, operationID, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	if revoked && (op.Kind == "quarantine" || op.Kind == "purge") {
		expected, restored := model.BackupDeleting, model.BackupComplete
		if op.Kind == "purge" {
			expected, restored = model.BackupPurging, model.BackupQuarantined
		}
		if err = changed(tx.Exec(`UPDATE backups SET status=? WHERE id=? AND status=?`, restored, op.BackupID, expected)); err != nil {
			return err
		}
		if err = changed(tx.Exec(`UPDATE retention_operations SET state='cancelled',ticket_hash='',lease_generation=0,completed_at=?,error='annullata automaticamente: chiave revocata' WHERE id=? AND state='authorized'`, now.Unix(), operationID)); err != nil {
			return err
		}
		if _, err = resolveMaintenanceOperationTx(tx, operationID, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err = changed(tx.Exec(`UPDATE retention_operations SET state='requested',ticket_hash='',lease_generation=0,authorized_at=0,error='autorizzazione ricreata dopo riavvio prima dell operazione fisica' WHERE id=? AND state='authorized'`, operationID)); err != nil {
		return err
	}
	if _, err = resolveMaintenanceOperationTx(tx, operationID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Operation(id int64) (model.RetentionOperation, error) {
	var op model.RetentionOperation
	err := scanOperation(s.db.QueryRow(`SELECT id,backup_id,kind,state,origin,actor,reason,revision,requested_at,authorized_at,completed_at,error,lease_generation,execution_committed FROM retention_operations WHERE id=?`, id), &op)
	return op, err
}

func (s *Store) CancelQuarantineRequest(backupID, actor string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE retention_operations SET state='cancelled',completed_at=?,error='annullata da ' || ? WHERE backup_id=? AND kind='quarantine' AND state='requested'`, now.Unix(), actor, backupID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("richiesta non annullabile: gia autorizzata o inesistente")
	}
	var restored sql.Result
	if restored, err = tx.Exec(`UPDATE backups SET status='complete' WHERE id=? AND status='deleting'`, backupID); err == nil {
		if n, _ := restored.RowsAffected(); n != 1 {
			return errors.New("backup non ripristinato dopo l'annullamento")
		}
		_, err = tx.Exec(`UPDATE automation_state SET deletion_blocked=1,monitoring_state='SOSPESO',block_reason='richiesta annullata; riattivazione amministrativa richiesta' WHERE id=1`)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func scanOperation(row scanner, op *model.RetentionOperation) error {
	return row.Scan(&op.ID, &op.BackupID, &op.Kind, &op.State, &op.Origin, &op.Actor, &op.Reason,
		&op.Revision, &op.RequestedAt, &op.AuthorizedAt, &op.CompletedAt, &op.Error, &op.LeaseGeneration, &op.ExecutionCommitted)
}

func scanOperationWithHash(row scanner, op *model.RetentionOperation, hash *string) error {
	return row.Scan(&op.ID, &op.BackupID, &op.Kind, &op.State, &op.Origin, &op.Actor, &op.Reason,
		&op.Revision, &op.RequestedAt, &op.AuthorizedAt, &op.CompletedAt, &op.Error, &op.LeaseGeneration, &op.ExecutionCommitted, hash)
}

func (s *Store) Operations(states ...string) ([]model.RetentionOperation, error) {
	query := `SELECT id,backup_id,kind,state,origin,actor,reason,revision,requested_at,authorized_at,completed_at,error,lease_generation,execution_committed FROM retention_operations`
	args := make([]any, 0, len(states))
	if len(states) != 0 {
		query += ` WHERE state IN (` + strings.TrimRight(strings.Repeat("?,", len(states)), ",") + `)`
		for _, state := range states {
			args = append(args, state)
		}
	}
	query += ` ORDER BY id`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RetentionOperation
	for rows.Next() {
		var op model.RetentionOperation
		if err = scanOperation(rows, &op); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (s *Store) SnapshotOperations() ([]model.RetentionOperation, error) {
	rows, err := s.db.Query(`SELECT id,backup_id,kind,state,origin,actor,reason,revision,requested_at,authorized_at,completed_at,error,lease_generation,execution_committed
 FROM retention_operations WHERE state IN ('requested','authorized') OR id IN
 (SELECT id FROM retention_operations ORDER BY id DESC LIMIT 100) ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.RetentionOperation
	for rows.Next() {
		var op model.RetentionOperation
		if err = scanOperation(rows, &op); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

var excludableAnomalyKinds = map[string]bool{"upload": true, "time": true, "schedule_missing": true, "extra": true}

func anomalyExcludedTx(tx *sql.Tx, keyID, kind string, eventAt time.Time) (bool, error) {
	if keyID == "" || !excludableAnomalyKinds[kind] {
		return false, nil
	}
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM anomaly_exclusions e
 JOIN anomaly_exclusion_kinds k ON k.exclusion_id=e.id
 WHERE e.key_id=? AND k.kind=? AND ? BETWEEN e.starts_at AND e.ends_at`, keyID, kind, eventAt.Unix()).Scan(&count)
	return count != 0, err
}

func recordExcludedAnomalyTx(tx *sql.Tx, stableKey, kind, keyID, backupID, detail string, eventAt, now time.Time) error {
	var exclusionID int64
	var actor, reason string
	err := tx.QueryRow(`SELECT e.id,e.actor,e.reason FROM anomaly_exclusions e
 JOIN anomaly_exclusion_kinds k ON k.exclusion_id=e.id
 WHERE e.key_id=? AND k.kind=? AND ? BETWEEN e.starts_at AND e.ends_at ORDER BY e.id DESC LIMIT 1`, keyID, kind, eventAt.Unix()).Scan(&exclusionID, &actor, &reason)
	if err != nil {
		return err
	}
	note := fmt.Sprintf("esclusione amministrativa %d: %s", exclusionID, reason)
	res, err := tx.Exec(`UPDATE anomalies SET last_seen_at=?,detail=? WHERE stable_key=? AND resolved_at<>0 AND resolution_note=?`, now.Unix(), detail, stableKey, note)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 0 {
		return nil
	}
	_, err = tx.Exec(`INSERT INTO anomalies(stable_key,kind,key_id,backup_id,detail,event_at,opened_at,last_seen_at,resolved_at,acknowledged_at,resolved_by,resolution_note)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, stableKey, kind, keyID, backupID, detail, eventAt.Unix(), now.Unix(), now.Unix(), now.Unix(), now.Unix(), actor, note)
	return err
}

func anomalyBlocksTx(tx *sql.Tx, kind, keyID string, now time.Time) (bool, error) {
	if kind == "extra" {
		return false, nil
	}
	if !excludableAnomalyKinds[kind] {
		return true, nil
	}
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM anomalies WHERE resolved_at=0 AND kind=? AND key_id=? AND event_at>=?`, kind, keyID, now.AddDate(0, 0, -14).Unix()).Scan(&count)
	return count >= 2, err
}

func blockForAnomalyTx(tx *sql.Tx, detail string) error {
	_, err := tx.Exec(`UPDATE automation_state SET deletion_blocked=1,monitoring_state='ANOMALIA',block_reason=? WHERE id=1`, "anomalia: "+detail)
	return err
}

func blockingAnomalyCountTx(tx *sql.Tx, now time.Time) (int, error) {
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM anomalies a WHERE a.resolved_at=0 AND (
	 a.kind NOT IN ('upload','time','schedule_missing','extra') OR
		(a.kind<>'extra' AND (SELECT COUNT(*) FROM anomalies b WHERE b.resolved_at=0 AND b.kind=a.kind AND b.key_id=a.key_id AND b.event_at>=?)>=2))`, now.AddDate(0, 0, -14).Unix()).Scan(&count)
	return count, err
}

func (s *Store) OpenAnomaly(stableKey, kind, keyID, backupID, detail string, now time.Time) (bool, error) {
	return s.OpenAnomalyAt(stableKey, kind, keyID, backupID, detail, now, now)
}

func (s *Store) OpenAnomalyAt(stableKey, kind, keyID, backupID, detail string, eventAt, now time.Time) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	created, err := openAnomalyAtTx(tx, stableKey, kind, keyID, backupID, detail, eventAt, now)
	if err != nil {
		return false, err
	}
	return created, tx.Commit()
}

func openAnomalyAtTx(tx *sql.Tx, stableKey, kind, keyID, backupID, detail string, eventAt, now time.Time) (bool, error) {
	if strings.TrimSpace(stableKey) == "" || strings.TrimSpace(kind) == "" || strings.TrimSpace(detail) == "" {
		return false, errors.New("anomalia non valida")
	}
	excluded, err := anomalyExcludedTx(tx, keyID, kind, eventAt)
	if err != nil {
		return false, err
	}
	if excluded {
		if err = recordExcludedAnomalyTx(tx, stableKey, kind, keyID, backupID, detail, eventAt, now); err != nil {
			return false, err
		}
		return false, nil
	}
	res, err := tx.Exec(`UPDATE anomalies SET last_seen_at=?,detail=? WHERE stable_key=? AND resolved_at=0`, now.Unix(), detail, stableKey)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 0 {
		return false, nil
	}
	if _, err = tx.Exec(`INSERT INTO anomalies(stable_key,kind,key_id,backup_id,detail,event_at,opened_at,last_seen_at) VALUES(?,?,?,?,?,?,?,?)`, stableKey, kind, keyID, backupID, detail, eventAt.Unix(), now.Unix(), now.Unix()); err != nil {
		return false, err
	}
	blocks, err := anomalyBlocksTx(tx, kind, keyID, now)
	if err != nil {
		return false, err
	}
	if blocks {
		if err = blockForAnomalyTx(tx, detail); err != nil {
			return false, err
		}
	}
	mailID := fmt.Sprintf("anomaly:%s:%d", stableKey, now.Unix())
	subject, body := anomalyMail(kind, detail)
	if kind == "schedule_missing" {
		subject, body, err = missingBackupMailTx(tx, keyID, eventAt)
		if err != nil {
			return false, err
		}
	}
	if _, err = tx.Exec(`INSERT INTO mail_queue(stable_id,kind,subject,body,created_at,next_attempt_at) VALUES(?,'anomaly',?,?,?,?)`, mailID, subject, body, now.Unix(), now.Unix()); err != nil {
		return false, err
	}
	return true, nil
}

func anomalyMail(kind, detail string) (string, string) {
	if kind == "extra" {
		return "OnlyBackup: AVVISO NON BLOCCANTE - copie eccedenti", detail + "\nQuesta anomalia non sospende la retention; eventuali altri blocchi restano validi."
	}
	return "OnlyBackup: nuova anomalia", detail
}

func (s *Store) ResolveAnomaly(stableKey string, now time.Time) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE anomalies SET resolved_at=? WHERE stable_key=? AND resolved_at=0`, now.Unix(), stableKey)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, tx.Commit()
	}
	if _, err = tx.Exec(`UPDATE automation_state SET deletion_blocked=1,monitoring_state='SOSPESO',block_reason='anomalia risolta; retention resume richiesto' WHERE id=1`); err != nil {
		return false, err
	}
	mailID := fmt.Sprintf("resolved:%s:%d", stableKey, now.Unix())
	if _, err = tx.Exec(`INSERT INTO mail_queue(stable_id,kind,subject,body,created_at,next_attempt_at) VALUES(?,'resolution','OnlyBackup: anomalia risolta',?,?,?)`, mailID, stableKey, now.Unix(), now.Unix()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func resolveMaintenanceOperationTx(tx *sql.Tx, operationID int64, now time.Time) (bool, error) {
	stableKey := fmt.Sprintf("maintenance-operation:%d", operationID)
	uncertainKey := fmt.Sprintf("operation-uncertain:%d", operationID)
	res, err := tx.Exec(`UPDATE anomalies SET resolved_at=?,resolved_by='automatic',resolution_note='stato fisico verificato dalla riconciliazione'
 WHERE stable_key IN (?,?) AND kind='maintenance' AND resolved_at=0`, now.Unix(), stableKey, uncertainKey)
	if err != nil {
		return false, err
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		return false, nil
	}
	mailID := fmt.Sprintf("maintenance-resolved:%d:%d", operationID, now.Unix())
	if _, err = tx.Exec(`INSERT INTO mail_queue(stable_id,kind,subject,body,created_at,next_attempt_at)
 VALUES(?,'resolution','OnlyBackup: errore di manutenzione risolto',?,?,?)`, mailID, stableKey, now.Unix(), now.Unix()); err != nil {
		return false, err
	}
	return true, nil
}

// TryClearResolvedMaintenanceBlock removes only the block still attributable
// to a reconciled operation. All ambiguous or independent holds remain set.
func (s *Store) TryClearResolvedMaintenanceBlock(operationID int64, now time.Time) (bool, error) {
	if operationID <= 0 {
		return false, errors.New("operazione di manutenzione non valida")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stableKey := fmt.Sprintf("maintenance-operation:%d", operationID)
	uncertainKey := fmt.Sprintf("operation-uncertain:%d", operationID)
	var detail string
	var openedAt int64
	if err = tx.QueryRow(`SELECT detail,opened_at FROM anomalies
 WHERE stable_key IN (?,?) AND kind='maintenance' AND resolved_at<>0 AND resolved_by='automatic'
 AND 'anomalia: ' || detail=(SELECT block_reason FROM automation_state WHERE id=1)
 ORDER BY id DESC LIMIT 1`, stableKey, uncertainKey).Scan(&detail, &openedAt); errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	} else if err != nil {
		return false, err
	}
	var blocked bool
	var blockReason, state string
	var revision, lastCheck int64
	if err = tx.QueryRow(`SELECT (deletion_blocked OR manual_paused),block_reason,monitoring_state,model_revision,last_check_at
 FROM automation_state WHERE id=1`).Scan(&blocked, &blockReason, &state, &revision, &lastCheck); err != nil {
		return false, err
	}
	if !blocked || blockReason != "anomalia: "+detail || state != model.MonitoringRegular || revision == 0 ||
		lastCheck == 0 || lastCheck > now.Unix()+1 || now.Unix()-lastCheck > int64(10*time.Minute/time.Second) {
		return false, tx.Commit()
	}
	blocking, err := blockingAnomalyCountTx(tx, now)
	if err != nil {
		return false, err
	}
	var uncertain, laterCancellation int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM retention_operations WHERE state='authorized'`).Scan(&uncertain); err != nil {
		return false, err
	}
	if err = tx.QueryRow(`SELECT COUNT(*) FROM retention_operations WHERE state='cancelled' AND completed_at>=?`, openedAt).Scan(&laterCancellation); err != nil {
		return false, err
	}
	if blocking != 0 || uncertain != 0 || laterCancellation != 0 {
		return false, tx.Commit()
	}
	res, err := tx.Exec(`UPDATE automation_state SET deletion_blocked=0,block_reason=''
 WHERE id=1 AND manual_paused=0 AND deletion_blocked=1 AND block_reason=?`, "anomalia: "+detail)
	if err != nil {
		return false, err
	}
	changed, _ := res.RowsAffected()
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return changed == 1, nil
}

func (s *Store) AcknowledgeMissingAnomaly(id int64, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind, stableKey string
	var lastCheck int64
	if err = tx.QueryRow(`SELECT kind,stable_key FROM anomalies WHERE id=? AND resolved_at=0`, id).Scan(&kind, &stableKey); err != nil {
		return err
	}
	if kind != "file_missing" {
		return errors.New("solo una copia definitivamente mancante puo essere riconosciuta")
	}
	if err = tx.QueryRow(`SELECT last_check_at FROM automation_state WHERE id=1`).Scan(&lastCheck); err != nil {
		return err
	}
	if lastCheck == 0 || lastCheck > now.Unix()+1 || now.Unix()-lastCheck > int64(10*time.Minute/time.Second) {
		return errors.New("serve un controllo recente")
	}
	if _, err = tx.Exec(`UPDATE anomalies SET acknowledged_at=?,resolved_at=? WHERE id=? AND resolved_at=0`, now.Unix(), now.Unix(), id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE automation_state SET deletion_blocked=1,monitoring_state='SOSPESO',block_reason='copia mancante riconosciuta; retention resume richiesto' WHERE id=1`); err != nil {
		return err
	}
	mailID := fmt.Sprintf("acknowledged:%s:%d", stableKey, now.Unix())
	if _, err = tx.Exec(`INSERT INTO mail_queue(stable_id,kind,subject,body,created_at,next_attempt_at) VALUES(?,'resolution','OnlyBackup: copia mancante riconosciuta',?,?,?)`, mailID, stableKey, now.Unix(), now.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Anomalies(activeOnly bool) ([]model.Anomaly, error) {
	query := `SELECT id,stable_key,kind,key_id,backup_id,detail,event_at,opened_at,last_seen_at,resolved_at,acknowledged_at,resolved_by,resolution_note FROM anomalies`
	if activeOnly {
		query += ` WHERE resolved_at=0`
	}
	query += ` ORDER BY opened_at,id`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Anomaly
	for rows.Next() {
		var a model.Anomaly
		if err = rows.Scan(&a.ID, &a.StableKey, &a.Kind, &a.KeyID, &a.BackupID, &a.Detail, &a.EventAt, &a.OpenedAt, &a.LastSeenAt, &a.ResolvedAt, &a.AcknowledgedAt, &a.ResolvedBy, &a.ResolutionNote); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AcknowledgedMissingBackupIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT backup_id FROM anomalies
	 WHERE kind='file_missing' AND acknowledged_at<>0 AND backup_id<>'' ORDER BY backup_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var backupID string
		if err = rows.Scan(&backupID); err != nil {
			return nil, err
		}
		out = append(out, backupID)
	}
	return out, rows.Err()
}

func (s *Store) CreateAnomalyExclusion(sourceID int64, startsAt, endsAt time.Time, actor, reason string, kinds []string, now time.Time) (model.AnomalyExclusion, error) {
	var out model.AnomalyExclusion
	actor, reason = strings.TrimSpace(actor), strings.TrimSpace(reason)
	if sourceID <= 0 || actor == "" || reason == "" || endsAt.Before(startsAt) || endsAt.Sub(startsAt) > 7*24*time.Hour || len(kinds) == 0 {
		return out, errors.New("esclusione anomalia non valida o superiore a sette giorni")
	}
	seen := make(map[string]bool)
	for _, kind := range kinds {
		if !excludableAnomalyKinds[kind] || seen[kind] {
			return out, fmt.Errorf("tipo anomalia non escludibile o duplicato: %s", kind)
		}
		seen[kind] = true
	}
	tx, err := s.db.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var keyID string
	var eventAt int64
	if err = tx.QueryRow(`SELECT key_id,event_at FROM anomalies WHERE id=?`, sourceID).Scan(&keyID, &eventAt); err != nil {
		return out, err
	}
	if keyID == "" || eventAt < startsAt.Unix() || eventAt > endsAt.Unix() {
		return out, errors.New("l'intervallo deve contenere l'anomalia originaria associata a una chiave")
	}
	res, err := tx.Exec(`INSERT INTO anomaly_exclusions(source_anomaly_id,key_id,starts_at,ends_at,actor,reason,created_at) VALUES(?,?,?,?,?,?,?)`, sourceID, keyID, startsAt.Unix(), endsAt.Unix(), actor, reason, now.Unix())
	if err != nil {
		return out, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return out, err
	}
	for _, kind := range kinds {
		if _, err = tx.Exec(`INSERT INTO anomaly_exclusion_kinds(exclusion_id,kind) VALUES(?,?)`, id, kind); err != nil {
			return out, err
		}
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(kinds)), ",")
	args := []any{now.Unix(), now.Unix(), actor, reason, keyID, startsAt.Unix(), endsAt.Unix()}
	for _, kind := range kinds {
		args = append(args, kind)
	}
	if _, err = tx.Exec(`UPDATE anomalies SET resolved_at=?,acknowledged_at=?,resolved_by=?,resolution_note=?
 WHERE resolved_at=0 AND key_id=? AND event_at BETWEEN ? AND ? AND kind IN (`+placeholders+`)`, args...); err != nil {
		return out, err
	}
	blocking, err := blockingAnomalyCountTx(tx, now)
	if err != nil {
		return out, err
	}
	var uncertain int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM retention_operations WHERE state='authorized'`).Scan(&uncertain); err != nil {
		return out, err
	}
	if blocking == 0 && uncertain == 0 {
		if _, err = tx.Exec(`UPDATE automation_state SET deletion_blocked=0,monitoring_state='REGOLARE',block_reason='' WHERE id=1 AND block_reason LIKE 'anomalia:%'`); err != nil {
			return out, err
		}
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	out = model.AnomalyExclusion{ID: id, SourceAnomalyID: sourceID, KeyID: keyID, StartsAt: startsAt.Unix(), EndsAt: endsAt.Unix(), Actor: actor, Reason: reason, CreatedAt: now.Unix(), Kinds: append([]string(nil), kinds...)}
	return out, nil
}

func (s *Store) AnomalyExclusions() ([]model.AnomalyExclusion, error) {
	rows, err := s.db.Query(`SELECT e.id,e.source_anomaly_id,e.key_id,e.starts_at,e.ends_at,e.actor,e.reason,e.created_at,k.kind
 FROM anomaly_exclusions e JOIN anomaly_exclusion_kinds k ON k.exclusion_id=e.id ORDER BY e.id,k.kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AnomalyExclusion
	for rows.Next() {
		var id int64
		var kind string
		var source model.AnomalyExclusion
		if err = rows.Scan(&id, &source.SourceAnomalyID, &source.KeyID, &source.StartsAt, &source.EndsAt, &source.Actor, &source.Reason, &source.CreatedAt, &kind); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != id {
			source.ID = id
			out = append(out, source)
		}
		out[len(out)-1].Kinds = append(out[len(out)-1].Kinds, kind)
	}
	return out, rows.Err()
}

func (s *Store) QueueReport(stableID, subject, body string, now time.Time) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO mail_queue(stable_id,kind,subject,body,created_at,next_attempt_at) VALUES(?,'report',?,?,?,?)`, stableID, subject, body, now.Unix(), now.Unix())
	return err
}

func (s *Store) QueuePeriodicReport(stableID, subject, body string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO mail_queue(stable_id,kind,subject,body,created_at,next_attempt_at) VALUES(?,'report',?,?,?,?)`, stableID, subject, body, now.Unix(), now.Unix()); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE automation_state SET next_report_at=? WHERE id=1`, now.Add(72*time.Hour).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DueMail(now time.Time, limit int) ([]model.MailMessage, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("limite coda mail non valido")
	}
	rows, err := s.db.Query(`SELECT id,stable_id,kind,subject,body,attempts,next_attempt_at,sent_at FROM mail_queue WHERE sent_at=0 AND next_attempt_at<=? ORDER BY id LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.MailMessage
	for rows.Next() {
		var m model.MailMessage
		if err = rows.Scan(&m.ID, &m.StableID, &m.Kind, &m.Subject, &m.Body, &m.Attempts, &m.NextAttempt, &m.SentAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ConfirmMail(id int64, sendErr string, now time.Time) error {
	var stableID string
	if err := s.db.QueryRow(`SELECT stable_id FROM mail_queue WHERE id=?`, id).Scan(&stableID); err != nil {
		return err
	}
	if sendErr == "" {
		_, err := s.db.Exec(`UPDATE mail_queue SET sent_at=?,last_error='' WHERE id=? AND sent_at=0`, now.Unix(), id)
		if err == nil && strings.HasPrefix(stableID, "mail-test:") {
			err = s.MarkMailTested(now)
		}
		return err
	}
	var attempts int
	if err := s.db.QueryRow(`SELECT attempts FROM mail_queue WHERE id=? AND sent_at=0`, id).Scan(&attempts); err != nil {
		return err
	}
	delay := time.Minute << min(attempts, 8)
	_, err := s.db.Exec(`UPDATE mail_queue SET attempts=attempts+1,next_attempt_at=?,last_error=? WHERE id=? AND sent_at=0`, now.Add(delay).Unix(), sendErr, id)
	return err
}

func (s *Store) SaveModel(keyID string, revision int64, timezone string, retentionDays int, value any, reliable, review bool, now time.Time) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO monitoring_models(key_id,revision,learned_at,timezone,retention_days,model_json,reliable,review_required)
 VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(key_id,revision) DO UPDATE SET learned_at=excluded.learned_at,
 timezone=excluded.timezone,retention_days=excluded.retention_days,model_json=excluded.model_json,reliable=excluded.reliable,review_required=excluded.review_required`,
		keyID, revision, now.Unix(), timezone, retentionDays, string(b), reliable, review)
	return err
}

func (s *Store) LoadModels() (map[string]json.RawMessage, error) {
	rows, err := s.db.Query(`SELECT key_id,model_json FROM monitoring_models WHERE reliable=1 AND review_required=0 AND revision=(SELECT model_revision FROM automation_state WHERE id=1)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]json.RawMessage)
	for rows.Next() {
		var keyID, value string
		if err = rows.Scan(&keyID, &value); err != nil {
			return nil, err
		}
		out[keyID] = json.RawMessage(value)
	}
	return out, rows.Err()
}

func (s *Store) SetCheckState(state string, revision int64, now time.Time) error {
	if state != model.MonitoringLearning && state != model.MonitoringRegular && state != model.MonitoringAnomaly && state != model.MonitoringPaused {
		return errors.New("stato monitoraggio non valido")
	}
	if state == model.MonitoringLearning {
		_, err := s.db.Exec(`UPDATE automation_state SET monitoring_state=?,model_revision=?,last_check_at=?,deletion_blocked=1,block_reason='apprendimento non completato' WHERE id=1`, state, revision, now.Unix())
		return err
	}
	_, err := s.db.Exec(`UPDATE automation_state SET monitoring_state=?,model_revision=?,last_check_at=? WHERE id=1`, state, revision, now.Unix())
	return err
}

func (s *Store) AcquireMaintenanceLease(owner string, generation int64, now time.Time, duration time.Duration) (model.MaintenanceLease, error) {
	var lease model.MaintenanceLease
	if strings.TrimSpace(owner) == "" || duration <= 0 {
		return lease, errors.New("lease non valida")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return lease, err
	}
	defer tx.Rollback()
	var result sql.Result
	if generation == 0 {
		result, err = tx.Exec(`UPDATE maintenance_leases SET owner=?,expires_at=?,generation=generation+1
		 WHERE id=1 AND expires_at<=? AND generation<9223372036854775807`, owner, now.Add(duration).Unix(), now.Unix())
	} else if generation > 0 {
		result, err = tx.Exec(`UPDATE maintenance_leases SET expires_at=?
		 WHERE id=1 AND owner=? AND generation=? AND expires_at>?`, now.Add(duration).Unix(), owner, generation, now.Unix())
	} else {
		return lease, errors.New("generazione lease non valida")
	}
	if err != nil {
		return lease, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return lease, model.ErrLease
	}
	if err = tx.QueryRow(`SELECT owner,generation,expires_at FROM maintenance_leases WHERE id=1`).Scan(&lease.Owner, &lease.Generation, &lease.ExpiresAt); err != nil {
		return lease, err
	}
	if err = tx.Commit(); err != nil {
		return model.MaintenanceLease{}, err
	}
	return lease, nil
}

func (s *Store) ValidateMaintenanceLease(lease model.MaintenanceLease, now time.Time) error {
	var valid int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM maintenance_leases WHERE id=1 AND owner=? AND generation=? AND expires_at>?`, lease.Owner, lease.Generation, now.Unix()).Scan(&valid); err != nil {
		return err
	}
	if valid != 1 {
		return model.ErrLease
	}
	return nil
}

func validateMaintenanceLeaseTx(tx *sql.Tx, lease model.MaintenanceLease, now time.Time) error {
	if strings.TrimSpace(lease.Owner) == "" || lease.Generation <= 0 {
		return model.ErrLease
	}
	var valid int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM maintenance_leases WHERE id=1 AND owner=? AND generation=? AND expires_at>?`, lease.Owner, lease.Generation, now.Unix()).Scan(&valid); err != nil {
		return err
	}
	if valid != 1 {
		return model.ErrLease
	}
	return nil
}

func (s *Store) ReleaseMaintenanceLease(lease model.MaintenanceLease) error {
	_, err := s.db.Exec(`UPDATE maintenance_leases SET owner='',expires_at=0 WHERE id=1 AND owner=? AND generation=?`, lease.Owner, lease.Generation)
	return err
}

// DatabaseIntegrity exposes only fixed SQLite checks to the writer's
// maintenance interface; peers cannot submit arbitrary SQL.
func (s *Store) DatabaseIntegrity() error {
	var result string
	if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil || result != "ok" {
		return fmt.Errorf("integrity_check: %s: %w", result, err)
	}
	rows, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign_key_check ha rilevato violazioni")
	}
	return rows.Err()
}
