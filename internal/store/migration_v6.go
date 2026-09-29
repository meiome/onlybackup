package store

import "database/sql"

func migrateV5ToV6(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(anomalies)`)
	if err != nil {
		return err
	}
	hasResolvedBy, hasResolutionNote, hasEventAt := false, false, false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		hasResolvedBy = hasResolvedBy || name == "resolved_by"
		hasResolutionNote = hasResolutionNote || name == "resolution_note"
		hasEventAt = hasEventAt || name == "event_at"
	}
	if err = rows.Close(); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{}
	if !hasResolvedBy {
		statements = append(statements, `ALTER TABLE anomalies ADD COLUMN resolved_by TEXT NOT NULL DEFAULT ''`)
	}
	if !hasResolutionNote {
		statements = append(statements, `ALTER TABLE anomalies ADD COLUMN resolution_note TEXT NOT NULL DEFAULT ''`)
	}
	if !hasEventAt {
		statements = append(statements, `ALTER TABLE anomalies ADD COLUMN event_at INTEGER NOT NULL DEFAULT 0`, `UPDATE anomalies SET event_at=opened_at WHERE event_at=0`)
	}
	statements = append(statements,
		`DROP INDEX IF EXISTS retention_operations_active`,
		`CREATE UNIQUE INDEX retention_operations_active ON retention_operations(backup_id) WHERE state IN ('requested','authorized')`,
		`CREATE TABLE IF NOT EXISTS anomaly_exclusions (
 id INTEGER PRIMARY KEY AUTOINCREMENT, source_anomaly_id INTEGER NOT NULL UNIQUE REFERENCES anomalies(id),
 key_id TEXT NOT NULL REFERENCES keys(id), starts_at INTEGER NOT NULL, ends_at INTEGER NOT NULL,
 actor TEXT NOT NULL, reason TEXT NOT NULL, created_at INTEGER NOT NULL,
 CHECK(ends_at>=starts_at AND ends_at-starts_at<=604800))`,
		`CREATE TABLE IF NOT EXISTS anomaly_exclusion_kinds (
 exclusion_id INTEGER NOT NULL REFERENCES anomaly_exclusions(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('upload','time','schedule_missing','extra')),
 PRIMARY KEY(exclusion_id,kind))`,
		`PRAGMA user_version=6`)
	for _, statement := range statements {
		if _, err = tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
