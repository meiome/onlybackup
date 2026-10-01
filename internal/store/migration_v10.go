package store

import "database/sql"

// Older schemas cannot distinguish an administrative pause from an automatic
// block. Preserve every existing hold until an explicit administrative resume.
func migrateV9ToV10(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{
		`ALTER TABLE automation_state ADD COLUMN manual_paused INTEGER NOT NULL DEFAULT 0 CHECK(manual_paused IN (0,1))`,
		`ALTER TABLE automation_state ADD COLUMN manual_pause_reason TEXT NOT NULL DEFAULT ''`,
		`UPDATE automation_state SET manual_paused=deletion_blocked,manual_pause_reason=CASE WHEN deletion_blocked=1 THEN 'blocco precedente alla migrazione: ' || block_reason ELSE '' END`,
		`CREATE TABLE key_retention (key_id TEXT PRIMARY KEY REFERENCES keys(id), days INTEGER NOT NULL CHECK(days>=7))`,
		`INSERT INTO key_retention(key_id,days) SELECT id,(SELECT retention_days FROM automation_state WHERE id=1) FROM keys`,
		`PRAGMA user_version=10`,
	} {
		if _, err = tx.Exec(query); err != nil {
			return err
		}
	}
	return tx.Commit()
}
