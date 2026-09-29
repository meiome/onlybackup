package store

import "database/sql"

func migrateV6ToV7(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE monitoring_checks (
 id INTEGER PRIMARY KEY AUTOINCREMENT, started_at INTEGER NOT NULL, finished_at INTEGER,
 period_from INTEGER NOT NULL, period_to INTEGER NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('running','completed_clean','completed_with_anomalies','failed')),
 summary TEXT NOT NULL DEFAULT '', CHECK(period_to>=period_from),
 CHECK((status='running' AND finished_at IS NULL) OR (status<>'running' AND finished_at IS NOT NULL)))`,
		`CREATE INDEX monitoring_checks_started ON monitoring_checks(started_at DESC)`,
		`CREATE TABLE monitoring_check_models (
 check_id INTEGER NOT NULL REFERENCES monitoring_checks(id) ON DELETE CASCADE,
 key_id TEXT NOT NULL, revision INTEGER NOT NULL, covered_from INTEGER NOT NULL, covered_to INTEGER NOT NULL,
 PRIMARY KEY(check_id,key_id,revision),
 FOREIGN KEY(key_id,revision) REFERENCES monitoring_models(key_id,revision), CHECK(covered_to>=covered_from))`,
		`CREATE INDEX monitoring_check_models_resume ON monitoring_check_models(key_id,revision,covered_to DESC)`,
		`PRAGMA user_version=7`,
	}
	for _, statement := range statements {
		if _, err = tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
