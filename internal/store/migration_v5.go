package store

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// migrateV4ToV5 keeps an fsync'ed cold copy beside the catalogue before the
// transactional table rebuild. The writer lock guarantees that no uploader is
// changing the database while this function runs.
func migrateV4ToV5(db *sql.DB, databasePath string) error {
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint pre-migrazione: %w", err)
	}
	backupPath := databasePath + ".v4.cold-copy"
	if err := coldCopy(databasePath, backupPath); err != nil {
		return err
	}
	// A catalogue may have been deliberately version-downgraded for recovery
	// testing while retaining the v5 layout. Do not rebuild it a second time.
	rows, err := db.Query(`PRAGMA table_info(backups)`)
	if err != nil {
		return err
	}
	hasRetentionColumns := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "quarantined_at" {
			hasRetentionColumns = true
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if hasRetentionColumns {
		_, err = db.Exec(`PRAGMA user_version=5`)
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`ALTER TABLE upload_attempts RENAME TO upload_attempts_v4`,
		`ALTER TABLE backups RENAME TO backups_v4`,
		`CREATE TABLE backups (
 id TEXT PRIMARY KEY, key_id TEXT NOT NULL REFERENCES keys(id), description TEXT NOT NULL,
 original_name TEXT NOT NULL, size_bytes INTEGER NOT NULL CHECK(size_bytes>0), sha256 TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('receiving','complete','failed','deleting','quarantined','purging','deleted')),
 started_at INTEGER NOT NULL, received_at TEXT NOT NULL DEFAULT '', failure TEXT NOT NULL DEFAULT '',
 content_format TEXT NOT NULL DEFAULT '' CHECK(content_format IN ('','age-v1')),
 idempotency_key TEXT DEFAULT NULL CHECK(idempotency_key IS NULL OR (length(idempotency_key)>=16 AND length(idempotency_key)<=128)),
 quarantined_at INTEGER NOT NULL DEFAULT 0, purge_not_before INTEGER NOT NULL DEFAULT 0, purged_at INTEGER NOT NULL DEFAULT 0
)`,
		`INSERT INTO backups(id,key_id,description,original_name,size_bytes,sha256,status,started_at,received_at,failure,content_format,idempotency_key)
 SELECT id,key_id,description,original_name,size_bytes,sha256,status,started_at,received_at,failure,content_format,idempotency_key FROM backups_v4`,
		`CREATE TABLE upload_attempts (
 id INTEGER PRIMARY KEY AUTOINCREMENT, key_id TEXT NOT NULL REFERENCES keys(id),
 backup_id TEXT NOT NULL REFERENCES backups(id), attempted_at INTEGER NOT NULL)`,
		`INSERT INTO upload_attempts(id,key_id,backup_id,attempted_at) SELECT id,key_id,backup_id,attempted_at FROM upload_attempts_v4`,
		`DROP TABLE upload_attempts_v4`,
		`DROP TABLE backups_v4`,
		`CREATE INDEX backups_key_state ON backups(key_id,status)`,
		`CREATE INDEX backups_key_time ON backups(key_id,started_at)`,
		`CREATE UNIQUE INDEX backups_key_idempotency ON backups(key_id,idempotency_key) WHERE idempotency_key IS NOT NULL`,
		`CREATE INDEX upload_attempts_key_time ON upload_attempts(key_id,attempted_at)`,
		`CREATE TABLE IF NOT EXISTS automation_state (
 id INTEGER PRIMARY KEY CHECK(id=1), enabled INTEGER NOT NULL DEFAULT 0,
 mail_tested INTEGER NOT NULL DEFAULT 0, monitoring_state TEXT NOT NULL DEFAULT 'APPRENDIMENTO',
 deletion_blocked INTEGER NOT NULL DEFAULT 1, block_reason TEXT NOT NULL DEFAULT 'automazione non configurata',
 timezone TEXT NOT NULL DEFAULT '', retention_days INTEGER NOT NULL DEFAULT 7 CHECK(retention_days>=7),
	threshold_basis_points INTEGER NOT NULL DEFAULT 8000 CHECK(threshold_basis_points BETWEEN 1 AND 10000),
	reserve_free INTEGER NOT NULL DEFAULT 0 CHECK(reserve_free>=0),
 model_revision INTEGER NOT NULL DEFAULT 0, last_check_at INTEGER NOT NULL DEFAULT 0,
 next_report_at INTEGER NOT NULL DEFAULT 0, smtp_host TEXT NOT NULL DEFAULT '', smtp_port INTEGER NOT NULL DEFAULT 0,
 smtp_tls INTEGER NOT NULL DEFAULT 1, mail_from TEXT NOT NULL DEFAULT '', mail_to TEXT NOT NULL DEFAULT '')`,
		`INSERT OR IGNORE INTO automation_state(id) VALUES(1)`,
		`CREATE TABLE IF NOT EXISTS monitoring_models (
 key_id TEXT NOT NULL REFERENCES keys(id), revision INTEGER NOT NULL, learned_at INTEGER NOT NULL,
 timezone TEXT NOT NULL, retention_days INTEGER NOT NULL CHECK(retention_days>=7), model_json TEXT NOT NULL,
 reliable INTEGER NOT NULL DEFAULT 0, review_required INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(key_id,revision))`,
		`CREATE TABLE IF NOT EXISTS anomalies (
 id INTEGER PRIMARY KEY AUTOINCREMENT, stable_key TEXT NOT NULL, kind TEXT NOT NULL,
 key_id TEXT NOT NULL DEFAULT '', backup_id TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL,
 opened_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL, resolved_at INTEGER NOT NULL DEFAULT 0,
 acknowledged_at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS anomalies_active_key ON anomalies(stable_key) WHERE resolved_at=0`,
		`CREATE INDEX IF NOT EXISTS anomalies_active ON anomalies(resolved_at,last_seen_at)`,
		`CREATE TABLE IF NOT EXISTS retention_operations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, backup_id TEXT NOT NULL REFERENCES backups(id),
 kind TEXT NOT NULL CHECK(kind IN ('quarantine','purge','recover','cancel')),
 state TEXT NOT NULL CHECK(state IN ('requested','authorized','confirmed','failed','cancelled')),
 origin TEXT NOT NULL CHECK(origin IN ('manual','automatic','reconcile')),
 actor TEXT NOT NULL, reason TEXT NOT NULL, ticket_hash TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL,
 requested_at INTEGER NOT NULL, authorized_at INTEGER NOT NULL DEFAULT 0, completed_at INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS retention_operations_backup ON retention_operations(backup_id,id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS retention_operations_active ON retention_operations(backup_id,kind) WHERE state IN ('requested','authorized')`,
		`CREATE TABLE IF NOT EXISTS mail_queue (
 id INTEGER PRIMARY KEY AUTOINCREMENT, stable_id TEXT NOT NULL UNIQUE, kind TEXT NOT NULL,
 subject TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt_at INTEGER NOT NULL, claimed_at INTEGER NOT NULL DEFAULT 0, sent_at INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS maintenance_leases (id INTEGER PRIMARY KEY CHECK(id=1), owner TEXT NOT NULL DEFAULT '', expires_at INTEGER NOT NULL DEFAULT 0)`,
		`INSERT OR IGNORE INTO maintenance_leases(id) VALUES(1)`,
		`PRAGMA user_version=5`,
	}
	for _, statement := range statements {
		if _, err = tx.Exec(statement); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	var integrity string
	if err = db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("integrity_check dopo migrazione: %s: %w", integrity, err)
	}
	var foreignKeys int
	if err = db.QueryRow("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&foreignKeys); err != nil || foreignKeys != 0 {
		return fmt.Errorf("foreign_key_check dopo migrazione: %d violazioni: %w", foreignKeys, err)
	}
	return nil
}

func coldCopy(source, destination string) error {
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("copia a freddo preesistente non sicura: %s", destination)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	temp := destination + ".tmp"
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("creazione copia a freddo: %w", err)
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			_ = os.Remove(temp)
		}
	}()
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp, destination); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}
