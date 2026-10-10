package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

// ResetAuthorized writes this marker only after the API verifies that the
// source is intact and the destination absent. A failed attempt replaces it,
// requiring another physical reconciliation before the next retry.
const reconciledSourceIntact = "autorizzazione ricreata dopo riavvio prima dell operazione fisica"

// A provisional retry may ignore only its own open maintenance error. It never
// clears the global hold or skips final validation, minima, revocation or lease
// checks. Other operations must continue to see the hold until verified success.
func canRetryReconciledOperationTx(tx *sql.Tx, op model.RetentionOperation, now time.Time) (bool, error) {
	if op.ExecutionCommitted || op.Error != reconciledSourceIntact ||
		(op.State != "requested" && op.State != "authorized") ||
		(op.Kind != "quarantine" && op.Kind != "purge") {
		return false, nil
	}
	var ready bool
	var reason, state string
	var lastCheck int64
	err := tx.QueryRow(`SELECT enabled AND mail_tested AND deletion_blocked
 AND NOT manual_paused AND NOT activation_required AND NOT resume_pending
 AND model_revision=?
 AND NOT EXISTS (SELECT 1 FROM keys k WHERE k.revoked=0 AND NOT EXISTS (
  SELECT 1 FROM monitoring_models m WHERE m.key_id=k.id AND m.revision=a.model_revision
  AND m.reliable=1 AND m.review_required=0 AND m.timezone=a.timezone)),
 block_reason,monitoring_state,last_check_at FROM automation_state a WHERE id=1`,
		op.Revision).Scan(&ready, &reason, &state, &lastCheck)
	if err != nil || !ready {
		return false, err
	}
	if state != model.MonitoringRegular && state != model.MonitoringAnomaly {
		return false, nil
	}
	// At final validation, keep the freshness decision made when this transport
	// ticket was issued: a complete source hash may itself take over ten minutes.
	// All current holds, models and independent anomalies are still checked below.
	decisionAt := now.Unix()
	if op.State == "authorized" {
		decisionAt = op.AuthorizedAt
	}
	if lastCheck == 0 || lastCheck > now.Unix()+1 ||
		decisionAt-lastCheck > int64(10*time.Minute/time.Second) {
		return false, nil
	}
	stableKey := fmt.Sprintf("maintenance-operation:%d", op.ID)
	var detail string
	var lastSeen int64
	err = tx.QueryRow(`SELECT detail,last_seen_at FROM anomalies
 WHERE stable_key=? AND kind='maintenance' AND backup_id=? AND resolved_at=0`,
		stableKey, op.BackupID).Scan(&detail, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if lastCheck < lastSeen {
		return false, nil
	}
	if reason != "anomalia: "+detail {
		// Reconciliation may have just clarified uncertainty for the same operation;
		// that reason may remain in automation_state while its physical error is open.
		var matching int
		err = tx.QueryRow(`SELECT COUNT(*) FROM anomalies WHERE stable_key=?
   AND kind='maintenance' AND resolved_at<>0 AND resolved_by='automatic'
   AND 'anomalia: ' || detail=?`, fmt.Sprintf("operation-uncertain:%d", op.ID), reason).Scan(&matching)
		if err != nil || matching == 0 {
			return false, err
		}
	}
	blocking, err := blockingAnomalyCountTx(tx, now)
	if err != nil || blocking != 1 {
		return false, err
	}
	var otherAuthorized int
	err = tx.QueryRow("SELECT COUNT(*) FROM retention_operations WHERE state='authorized' AND id<>?", op.ID).Scan(&otherAuthorized)
	return otherAuthorized == 0, err
}
