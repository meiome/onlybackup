package store

import "database/sql"

func migrateV7ToV8(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	columns := []struct {
		table, name, statement string
	}{
		{"maintenance_leases", "generation", `ALTER TABLE maintenance_leases ADD COLUMN generation INTEGER NOT NULL DEFAULT 0 CHECK(generation>=0)`},
		{"retention_operations", "lease_generation", `ALTER TABLE retention_operations ADD COLUMN lease_generation INTEGER NOT NULL DEFAULT 0 CHECK(lease_generation>=0)`},
	}
	for _, column := range columns {
		var count int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, column.table, column.name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err = tx.Exec(column.statement); err != nil {
				return err
			}
		}
	}
	statements := []string{
		`UPDATE maintenance_leases SET owner='',expires_at=0,generation=0`,
		`PRAGMA user_version=8`,
	}
	for _, statement := range statements {
		if _, err = tx.Exec(statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}
