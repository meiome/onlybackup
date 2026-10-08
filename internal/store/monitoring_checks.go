package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

const monitoringCheckRetention = 90 * 24 * time.Hour

type storedMonitoringModel struct {
	keyID     string
	revision  int64
	learnedAt int64
	timezone  string
	raw       json.RawMessage
}

func (s *Store) StartMonitoringCheck(now time.Time) (model.MonitoringCheck, error) {
	var check model.MonitoringCheck
	tx, err := s.db.Begin()
	if err != nil {
		return check, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE monitoring_checks SET status='failed',finished_at=?,summary='interrotto dal precedente arresto' WHERE status='running'`, now.Unix()); err != nil {
		return check, err
	}
	result, err := tx.Exec(`INSERT INTO monitoring_checks(started_at,period_from,period_to,status) VALUES(?,?,?,'running')`, now.Unix(), now.Unix(), now.Unix())
	if err != nil {
		return check, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return check, err
	}
	if err = tx.Commit(); err != nil {
		return check, err
	}
	return model.MonitoringCheck{ID: id, StartedAt: now.Unix(), PeriodFrom: now.Unix(), PeriodTo: now.Unix(), Status: "running"}, nil
}

func (s *Store) PlanMonitoringCheck(id int64, now time.Time) (model.MonitoringCheck, error) {
	var check model.MonitoringCheck
	tx, err := s.db.Begin()
	if err != nil {
		return check, err
	}
	defer tx.Rollback()
	if err = scanMonitoringCheck(tx.QueryRow(`SELECT id,started_at,COALESCE(finished_at,0),period_from,period_to,status,summary FROM monitoring_checks WHERE id=? AND status='running'`, id), &check); err != nil {
		return check, err
	}
	var currentRevision int64
	if err = tx.QueryRow(`SELECT model_revision FROM automation_state WHERE id=1`).Scan(&currentRevision); err != nil {
		return check, err
	}
	if currentRevision == 0 {
		if err = tx.QueryRow(`SELECT COALESCE(MAX(revision),0) FROM monitoring_models`).Scan(&currentRevision); err != nil {
			return check, err
		}
	}
	rows, err := tx.Query(`SELECT m.key_id,m.revision,m.learned_at,m.timezone,m.model_json
	 FROM monitoring_models m JOIN keys k ON k.id=m.key_id
	 WHERE k.revoked=0 AND m.reliable=1 AND m.review_required=0 AND m.revision<=?
	 ORDER BY m.key_id,m.learned_at,m.revision`, currentRevision)
	if err != nil {
		return check, err
	}
	modelsByKey := make(map[string][]storedMonitoringModel)
	for rows.Next() {
		var item storedMonitoringModel
		var raw string
		if err = rows.Scan(&item.keyID, &item.revision, &item.learnedAt, &item.timezone, &raw); err != nil {
			rows.Close()
			return check, err
		}
		item.raw = json.RawMessage(raw)
		modelsByKey[item.keyID] = append(modelsByKey[item.keyID], item)
	}
	if err = rows.Close(); err != nil {
		return check, err
	}
	latest := make(map[string]int64)
	rows, err = tx.Query(`SELECT cm.key_id,MAX(cm.covered_to) FROM monitoring_check_models cm
	 JOIN monitoring_checks c ON c.id=cm.check_id
	 WHERE c.status IN ('completed_clean','completed_with_anomalies') GROUP BY cm.key_id`)
	if err != nil {
		return check, err
	}
	for rows.Next() {
		var keyID string
		var coveredTo int64
		if err = rows.Scan(&keyID, &coveredTo); err != nil {
			rows.Close()
			return check, err
		}
		latest[keyID] = coveredTo
	}
	if err = rows.Close(); err != nil {
		return check, err
	}
	var completedChecks int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM monitoring_checks WHERE status IN ('completed_clean','completed_with_anomalies')`).Scan(&completedChecks); err != nil {
		return check, err
	}
	// A failed first attempt is not coverage, but it still fixes where bootstrap
	// began. Keep that reference even when the process stopped before planning.
	var firstStartedAt int64
	if err = tx.QueryRow(`SELECT started_at FROM monitoring_checks ORDER BY id LIMIT 1`).Scan(&firstStartedAt); err != nil {
		return check, err
	}
	coverageEnd := now.Add(-policy.ScheduleTolerance).Unix()
	keys := make([]string, 0, len(modelsByKey))
	for keyID := range modelsByKey {
		keys = append(keys, keyID)
	}
	sort.Strings(keys)
	periodFrom, periodTo := now.Unix(), now.Unix()
	firstScope := true
	for _, keyID := range keys {
		models := modelsByKey[keyID]
		start, hasCoverage := latest[keyID]
		firstModel := 0
		if !hasCoverage {
			var initialRevision int64
			initialErr := tx.QueryRow(`SELECT cm.covered_from,cm.revision FROM monitoring_check_models cm
			 JOIN monitoring_checks c ON c.id=cm.check_id WHERE cm.key_id=? AND c.id<?
			 ORDER BY c.id,cm.covered_from,cm.revision LIMIT 1`, keyID, id).Scan(&start, &initialRevision)
			if initialErr == nil {
				for firstModel < len(models) && models[firstModel].revision != initialRevision {
					firstModel++
				}
				if firstModel == len(models) {
					return check, errors.New("modello del primo controllo interrotto non disponibile")
				}
			} else if !errors.Is(initialErr, sql.ErrNoRows) {
				return check, initialErr
			} else if completedChecks == 0 {
				// Use the model applicable at the first attempt, then split later
				// revisions normally; a retry must not apply a new model backwards.
				for firstModel+1 < len(models) && models[firstModel+1].learnedAt <= firstStartedAt {
					firstModel++
				}
				current := models[firstModel]
				location, loadErr := time.LoadLocation(current.timezone)
				if loadErr != nil {
					return check, loadErr
				}
				local := time.Unix(firstStartedAt, 0).In(location)
				start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).AddDate(0, 0, -1).Add(-time.Second).Unix()
				if current.learnedAt > firstStartedAt {
					start = current.learnedAt
				}
			} else {
				start = models[0].learnedAt
			}
		}
		if start > coverageEnd {
			continue
		}
		var segments []storedMonitoringModel
		if !hasCoverage {
			segments = models[firstModel:]
		} else {
			segments = models
		}
		for index, item := range segments {
			segmentFrom := start
			if (hasCoverage || index > 0) && item.learnedAt > segmentFrom {
				segmentFrom = item.learnedAt
			}
			segmentTo := coverageEnd
			if index+1 < len(segments) && segments[index+1].learnedAt < segmentTo {
				segmentTo = segments[index+1].learnedAt
			}
			if segmentFrom > segmentTo || item.revision > currentRevision {
				continue
			}
			scope := model.MonitoringCheckModel{CheckID: id, KeyID: keyID, Revision: item.revision, CoveredFrom: segmentFrom, CoveredTo: segmentTo, Model: item.raw}
			if _, err = tx.Exec(`INSERT INTO monitoring_check_models(check_id,key_id,revision,covered_from,covered_to) VALUES(?,?,?,?,?)`, id, keyID, item.revision, segmentFrom, segmentTo); err != nil {
				return check, err
			}
			check.Models = append(check.Models, scope)
			if firstScope || segmentFrom < periodFrom {
				periodFrom = segmentFrom
			}
			if firstScope || segmentTo > periodTo {
				periodTo = segmentTo
			}
			firstScope = false
		}
	}
	if _, err = tx.Exec(`UPDATE monitoring_checks SET period_from=?,period_to=? WHERE id=? AND status='running'`, periodFrom, periodTo, id); err != nil {
		return check, err
	}
	check.PeriodFrom, check.PeriodTo = periodFrom, periodTo
	if err = tx.Commit(); err != nil {
		return check, err
	}
	return check, nil
}

func (s *Store) CompleteMonitoringCheck(id int64, outcome, summary, state string, revision int64, findings []policy.Finding, now time.Time) error {
	if outcome != "completed_clean" && outcome != "completed_with_anomalies" {
		return errors.New("esito controllo non valido")
	}
	if (len(findings) == 0) != (outcome == "completed_clean") {
		return errors.New("esito controllo incoerente con le anomalie")
	}
	if len(summary) > 8192 {
		return errors.New("riepilogo controllo troppo lungo")
	}
	if state != model.MonitoringLearning && state != model.MonitoringRegular && state != model.MonitoringAnomaly && state != model.MonitoringPaused {
		return errors.New("stato monitoraggio non valido")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentRevision int64
	var running int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM monitoring_checks WHERE id=? AND status='running'`, id).Scan(&running); err != nil {
		return err
	}
	if running != 1 {
		return errors.New("controllo non piu in corso o invalidato dalla configurazione")
	}
	if err = tx.QueryRow(`SELECT model_revision FROM automation_state WHERE id=1`).Scan(&currentRevision); err != nil {
		return err
	}
	// Revision zero is the initial learning cycle, which assigns its first
	// revision at completion. A concurrent relearn increments even zero, so an
	// obsolete cycle cannot replace that request or commit its findings.
	if currentRevision != 0 && currentRevision != revision {
		return errors.New("controllo obsoleto: revisione del modello cambiata; ripetere il controllo")
	}
	for _, finding := range findings {
		eventAt := finding.At
		if eventAt.IsZero() {
			eventAt = now
		}
		if _, err = openAnomalyAtTx(tx, finding.StableKey, finding.Kind, finding.KeyID, finding.BackupID, finding.Detail, eventAt, now); err != nil {
			return err
		}
	}
	blocking, err := blockingAnomalyCountTx(tx, now)
	if err != nil {
		return err
	}
	if blocking > 0 {
		state = model.MonitoringAnomaly
	}
	if state == model.MonitoringLearning {
		_, err = tx.Exec(`UPDATE automation_state SET monitoring_state=?,model_revision=?,last_check_at=?,deletion_blocked=1,block_reason='apprendimento non completato' WHERE id=1`, state, revision, now.Unix())
	} else {
		_, err = tx.Exec(`UPDATE automation_state SET monitoring_state=?,model_revision=?,last_check_at=? WHERE id=1`, state, revision, now.Unix())
	}
	if err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE monitoring_checks SET status=?,finished_at=?,summary=? WHERE id=? AND status='running'`, outcome, now.Unix(), summary, id)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("controllo non piu in corso")
	}
	if err = activateRetentionAfterCheckTx(tx); err != nil {
		return err
	}
	if err = cleanupMonitoringChecksTx(tx, now.Add(-monitoringCheckRetention).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// Run only in the transaction completing a real monitoring attempt, after its
// findings and current revision have been committed. Failed, stale or replayed
// completions cannot consume an activation or a deferred resume.
func activateRetentionAfterCheckTx(tx *sql.Tx) error {
	var ready bool
	err := tx.QueryRow(`SELECT enabled AND mail_tested AND monitoring_state='REGOLARE' AND model_revision<>0
 AND (resume_pending OR (activation_pending AND NOT manual_paused))
 AND EXISTS (SELECT 1 FROM keys WHERE revoked=0)
 AND NOT EXISTS (SELECT 1 FROM keys k WHERE k.revoked=0 AND NOT EXISTS (
  SELECT 1 FROM monitoring_models m WHERE m.key_id=k.id AND m.revision=a.model_revision
  AND m.reliable=1 AND m.review_required=0 AND m.timezone=a.timezone))
 AND NOT EXISTS (SELECT 1 FROM retention_operations WHERE state='authorized')
 FROM automation_state a WHERE id=1`).Scan(&ready)
	if err != nil || !ready {
		return err
	}
	_, err = tx.Exec(`UPDATE automation_state SET deletion_blocked=0,block_reason='',manual_paused=0,manual_pause_reason='',
 activation_pending=0,activation_required=0,resume_pending=0,resume_requested_at=0 WHERE id=1`)
	return err
}

func (s *Store) FailMonitoringCheck(id int64, summary string, now time.Time) error {
	if strings.TrimSpace(summary) == "" {
		summary = "controllo interrotto"
	}
	if len(summary) > 8192 {
		summary = summary[:8192]
	}
	_, err := s.db.Exec(`UPDATE monitoring_checks SET status='failed',finished_at=?,summary=? WHERE id=? AND status='running'`, now.Unix(), summary, id)
	return err
}

func cleanupMonitoringChecksTx(tx *sql.Tx, cutoff int64) error {
	_, err := tx.Exec(`DELETE FROM monitoring_checks WHERE finished_at IS NOT NULL AND finished_at<? AND id NOT IN (
	 SELECT cm.check_id FROM monitoring_check_models cm
	 JOIN monitoring_checks c ON c.id=cm.check_id JOIN keys k ON k.id=cm.key_id
	 WHERE k.revoked=0 AND cm.revision=(SELECT model_revision FROM automation_state WHERE id=1)
	 AND c.status IN ('completed_clean','completed_with_anomalies')
	 AND cm.covered_to=(SELECT MAX(cm2.covered_to) FROM monitoring_check_models cm2
	  JOIN monitoring_checks c2 ON c2.id=cm2.check_id
	  WHERE cm2.key_id=cm.key_id AND cm2.revision=cm.revision
	  AND c2.status IN ('completed_clean','completed_with_anomalies'))
	)`, cutoff)
	return err
}

func (s *Store) MonitoringChecks(limit int) ([]model.MonitoringCheck, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("limite controlli non valido")
	}
	rows, err := s.db.Query(`SELECT id,started_at,COALESCE(finished_at,0),period_from,period_to,status,summary FROM monitoring_checks ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var checks []model.MonitoringCheck
	for rows.Next() {
		var check model.MonitoringCheck
		if err = scanMonitoringCheck(rows, &check); err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for index := range checks {
		scopes, scopeErr := s.monitoringCheckModels(checks[index].ID)
		if scopeErr != nil {
			return nil, scopeErr
		}
		checks[index].Models = scopes
	}
	return checks, nil
}

func (s *Store) monitoringCheckModels(checkID int64) ([]model.MonitoringCheckModel, error) {
	rows, err := s.db.Query(`SELECT check_id,key_id,revision,covered_from,covered_to FROM monitoring_check_models WHERE check_id=? ORDER BY key_id,covered_from,revision`, checkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.MonitoringCheckModel
	for rows.Next() {
		var scope model.MonitoringCheckModel
		if err = rows.Scan(&scope.CheckID, &scope.KeyID, &scope.Revision, &scope.CoveredFrom, &scope.CoveredTo); err != nil {
			return nil, err
		}
		result = append(result, scope)
	}
	return result, rows.Err()
}

func scanMonitoringCheck(row scanner, check *model.MonitoringCheck) error {
	return row.Scan(&check.ID, &check.StartedAt, &check.FinishedAt, &check.PeriodFrom, &check.PeriodTo, &check.Status, &check.Summary)
}
