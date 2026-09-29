package store

import "database/sql"

// Existing tickets remain provisional: upgrading must never manufacture an
// irrevocable permission for an operation that may have been revoked.
func migrateV8ToV9(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('retention_operations') WHERE name='execution_committed'`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err = tx.Exec(`ALTER TABLE retention_operations ADD COLUMN execution_committed INTEGER NOT NULL DEFAULT 0 CHECK(execution_committed IN (0,1))`); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`PRAGMA user_version=9`); err != nil {
		return err
	}
	return tx.Commit()
}
