package store

import "database/sql"

// Existing archives keep their enablement and all holds. Only a new archive
// defaults to automatic first activation; upgrades need an explicit resume.
func migrateV10ToV11(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{
		`ALTER TABLE automation_state ADD COLUMN activation_pending INTEGER NOT NULL DEFAULT 0 CHECK(activation_pending IN (0,1))`,
		`ALTER TABLE automation_state ADD COLUMN activation_required INTEGER NOT NULL DEFAULT 0 CHECK(activation_required IN (0,1))`,
		`ALTER TABLE automation_state ADD COLUMN resume_pending INTEGER NOT NULL DEFAULT 0 CHECK(resume_pending IN (0,1))`,
		`ALTER TABLE automation_state ADD COLUMN resume_requested_at INTEGER NOT NULL DEFAULT 0`,
		`PRAGMA user_version=11`,
	} {
		if _, err = tx.Exec(query); err != nil {
			return err
		}
	}
	return tx.Commit()
}
