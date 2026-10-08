package store

import "database/sql"

// Keep existing mail proof and retention choices. Future SMTP confirmations
// must identify the configuration used for delivery; setup advances its version.
func migrateV11ToV12(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`ALTER TABLE automation_state ADD COLUMN mail_config_version INTEGER NOT NULL DEFAULT 1 CHECK(mail_config_version>=1)`); err != nil {
		return err
	}
	if _, err = tx.Exec(`PRAGMA user_version=12`); err != nil {
		return err
	}
	return tx.Commit()
}
