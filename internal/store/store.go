// Package store contains local SQLite operations. It is never linked into the public receiver.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

type Store struct {
	db            *sql.DB
	schemaVersion int
}

const schema = `
CREATE TABLE IF NOT EXISTS profiles (
 name TEXT PRIMARY KEY, total_bytes INTEGER NOT NULL CHECK(total_bytes>0),
 max_backup_bytes INTEGER NOT NULL CHECK(max_backup_bytes>0 AND max_backup_bytes<=total_bytes),
 uploads_per_day INTEGER NOT NULL CHECK(uploads_per_day>0), concurrent_uploads INTEGER NOT NULL CHECK(concurrent_uploads>0)
);
CREATE TABLE IF NOT EXISTS keys (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE,
 profile TEXT NOT NULL REFERENCES profiles(name), revoked INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS backups (
 id TEXT PRIMARY KEY, key_id TEXT NOT NULL REFERENCES keys(id), description TEXT NOT NULL,
 original_name TEXT NOT NULL, size_bytes INTEGER NOT NULL CHECK(size_bytes>0), sha256 TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('receiving','complete','failed','deleting','quarantined','purging','deleted')),
	started_at INTEGER NOT NULL, received_at TEXT NOT NULL DEFAULT '', failure TEXT NOT NULL DEFAULT '',
	content_format TEXT NOT NULL DEFAULT '' CHECK(content_format IN ('','age-v1')),
	idempotency_key TEXT DEFAULT NULL CHECK(idempotency_key IS NULL OR (length(idempotency_key)>=16 AND length(idempotency_key)<=128)),
	quarantined_at INTEGER NOT NULL DEFAULT 0, purge_not_before INTEGER NOT NULL DEFAULT 0, purged_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS backups_key_state ON backups(key_id,status);
CREATE INDEX IF NOT EXISTS backups_key_time ON backups(key_id,started_at);
CREATE UNIQUE INDEX IF NOT EXISTS backups_key_idempotency ON backups(key_id,idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE TABLE IF NOT EXISTS upload_attempts (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 key_id TEXT NOT NULL REFERENCES keys(id),
 backup_id TEXT NOT NULL REFERENCES backups(id),
 attempted_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS upload_attempts_key_time ON upload_attempts(key_id,attempted_at);
CREATE TABLE IF NOT EXISTS automation_state (
 id INTEGER PRIMARY KEY CHECK(id=1), enabled INTEGER NOT NULL DEFAULT 1,
 mail_tested INTEGER NOT NULL DEFAULT 0, monitoring_state TEXT NOT NULL DEFAULT 'APPRENDIMENTO',
 deletion_blocked INTEGER NOT NULL DEFAULT 1, block_reason TEXT NOT NULL DEFAULT 'automazione non configurata',
 manual_paused INTEGER NOT NULL DEFAULT 0 CHECK(manual_paused IN (0,1)),
 manual_pause_reason TEXT NOT NULL DEFAULT '',
 activation_pending INTEGER NOT NULL DEFAULT 1 CHECK(activation_pending IN (0,1)),
 activation_required INTEGER NOT NULL DEFAULT 1 CHECK(activation_required IN (0,1)),
 resume_pending INTEGER NOT NULL DEFAULT 0 CHECK(resume_pending IN (0,1)),
 resume_requested_at INTEGER NOT NULL DEFAULT 0,
 mail_config_version INTEGER NOT NULL DEFAULT 1 CHECK(mail_config_version>=1),
 timezone TEXT NOT NULL DEFAULT '', retention_days INTEGER NOT NULL DEFAULT 7 CHECK(retention_days>=7),
	threshold_basis_points INTEGER NOT NULL DEFAULT 8000 CHECK(threshold_basis_points BETWEEN 1 AND 10000),
	reserve_free INTEGER NOT NULL DEFAULT 0 CHECK(reserve_free>=0),
 model_revision INTEGER NOT NULL DEFAULT 0, last_check_at INTEGER NOT NULL DEFAULT 0,
 next_report_at INTEGER NOT NULL DEFAULT 0, smtp_host TEXT NOT NULL DEFAULT '', smtp_port INTEGER NOT NULL DEFAULT 0,
 smtp_tls INTEGER NOT NULL DEFAULT 1, mail_from TEXT NOT NULL DEFAULT '', mail_to TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO automation_state(id) VALUES(1);
CREATE TABLE IF NOT EXISTS key_retention (
 key_id TEXT PRIMARY KEY REFERENCES keys(id), days INTEGER NOT NULL CHECK(days>=7)
);
CREATE TABLE IF NOT EXISTS monitoring_models (
 key_id TEXT NOT NULL REFERENCES keys(id), revision INTEGER NOT NULL, learned_at INTEGER NOT NULL,
 timezone TEXT NOT NULL, retention_days INTEGER NOT NULL CHECK(retention_days>=7), model_json TEXT NOT NULL,
 reliable INTEGER NOT NULL DEFAULT 0, review_required INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(key_id,revision)
);
CREATE TABLE IF NOT EXISTS anomalies (
 id INTEGER PRIMARY KEY AUTOINCREMENT, stable_key TEXT NOT NULL, kind TEXT NOT NULL,
	key_id TEXT NOT NULL DEFAULT '', backup_id TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL,
	event_at INTEGER NOT NULL, opened_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL, resolved_at INTEGER NOT NULL DEFAULT 0,
 acknowledged_at INTEGER NOT NULL DEFAULT 0, resolved_by TEXT NOT NULL DEFAULT '',
 resolution_note TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS anomalies_active_key ON anomalies(stable_key) WHERE resolved_at=0;
CREATE INDEX IF NOT EXISTS anomalies_active ON anomalies(resolved_at,last_seen_at);
CREATE TABLE IF NOT EXISTS retention_operations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, backup_id TEXT NOT NULL REFERENCES backups(id),
 kind TEXT NOT NULL CHECK(kind IN ('quarantine','purge','recover','cancel')),
 state TEXT NOT NULL CHECK(state IN ('requested','authorized','confirmed','failed','cancelled')),
 origin TEXT NOT NULL CHECK(origin IN ('manual','automatic','reconcile')),
 actor TEXT NOT NULL, reason TEXT NOT NULL, ticket_hash TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL,
 requested_at INTEGER NOT NULL, authorized_at INTEGER NOT NULL DEFAULT 0, completed_at INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '', lease_generation INTEGER NOT NULL DEFAULT 0 CHECK(lease_generation>=0),
 execution_committed INTEGER NOT NULL DEFAULT 0 CHECK(execution_committed IN (0,1))
);
CREATE INDEX IF NOT EXISTS retention_operations_backup ON retention_operations(backup_id,id);
CREATE UNIQUE INDEX IF NOT EXISTS retention_operations_active ON retention_operations(backup_id) WHERE state IN ('requested','authorized');
CREATE TABLE IF NOT EXISTS anomaly_exclusions (
 id INTEGER PRIMARY KEY AUTOINCREMENT, source_anomaly_id INTEGER NOT NULL UNIQUE REFERENCES anomalies(id),
 key_id TEXT NOT NULL REFERENCES keys(id), starts_at INTEGER NOT NULL, ends_at INTEGER NOT NULL,
 actor TEXT NOT NULL, reason TEXT NOT NULL, created_at INTEGER NOT NULL,
 CHECK(ends_at>=starts_at AND ends_at-starts_at<=604800)
);
CREATE TABLE IF NOT EXISTS anomaly_exclusion_kinds (
 exclusion_id INTEGER NOT NULL REFERENCES anomaly_exclusions(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('upload','time','schedule_missing','extra')),
 PRIMARY KEY(exclusion_id,kind)
);
CREATE TABLE IF NOT EXISTS mail_queue (
 id INTEGER PRIMARY KEY AUTOINCREMENT, stable_id TEXT NOT NULL UNIQUE, kind TEXT NOT NULL,
 subject TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt_at INTEGER NOT NULL, claimed_at INTEGER NOT NULL DEFAULT 0, sent_at INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS maintenance_leases (
 id INTEGER PRIMARY KEY CHECK(id=1), owner TEXT NOT NULL DEFAULT '', expires_at INTEGER NOT NULL DEFAULT 0,
 generation INTEGER NOT NULL DEFAULT 0 CHECK(generation>=0)
);
INSERT OR IGNORE INTO maintenance_leases(id) VALUES(1);
CREATE TABLE IF NOT EXISTS monitoring_checks (
 id INTEGER PRIMARY KEY AUTOINCREMENT, started_at INTEGER NOT NULL, finished_at INTEGER,
 period_from INTEGER NOT NULL, period_to INTEGER NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('running','completed_clean','completed_with_anomalies','failed')),
 summary TEXT NOT NULL DEFAULT '', CHECK(period_to>=period_from),
 CHECK((status='running' AND finished_at IS NULL) OR (status<>'running' AND finished_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS monitoring_checks_started ON monitoring_checks(started_at DESC);
CREATE TABLE IF NOT EXISTS monitoring_check_models (
 check_id INTEGER NOT NULL REFERENCES monitoring_checks(id) ON DELETE CASCADE,
 key_id TEXT NOT NULL, revision INTEGER NOT NULL, covered_from INTEGER NOT NULL, covered_to INTEGER NOT NULL,
 PRIMARY KEY(check_id,key_id,revision),
 FOREIGN KEY(key_id,revision) REFERENCES monitoring_models(key_id,revision), CHECK(covered_to>=covered_from)
);
CREATE INDEX IF NOT EXISTS monitoring_check_models_resume ON monitoring_check_models(key_id,revision,covered_to DESC);
PRAGMA user_version=12;
`

func Open(path string, readonly bool) (*Store, error) {
	a, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(a); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, errors.New("il database deve essere un file regolare")
	}
	q := url.Values{"_busy_timeout": {"5000"}, "_foreign_keys": {"on"}, "_txlock": {"immediate"}}
	if readonly {
		q.Set("mode", "ro")
	} else {
		q.Set("mode", "rw")
		q.Set("_synchronous", "FULL")
	}
	u := url.URL{Scheme: "file", Path: a, RawQuery: q.Encode()}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, errors.New("impossibile leggere la versione dello schema database")
	}
	if version == 1 && !readonly {
		tx, txErr := db.Begin()
		if txErr == nil {
			_, txErr = tx.Exec("ALTER TABLE backups ADD COLUMN content_format TEXT NOT NULL DEFAULT '' CHECK(content_format IN ('','age-v1'))")
		}
		if txErr == nil {
			_, txErr = tx.Exec("PRAGMA user_version=2")
		}
		if txErr == nil {
			txErr = tx.Commit()
		} else if tx != nil {
			_ = tx.Rollback()
		}
		if txErr != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", txErr)
		}
		version = 2
	}
	if version == 2 && !readonly {
		tx, txErr := db.Begin()
		if txErr == nil {
			_, txErr = tx.Exec("ALTER TABLE backups ADD COLUMN idempotency_key TEXT DEFAULT NULL CHECK(idempotency_key IS NULL OR (length(idempotency_key)>=16 AND length(idempotency_key)<=128))")
		}
		if txErr == nil {
			_, txErr = tx.Exec("CREATE UNIQUE INDEX backups_key_idempotency ON backups(key_id,idempotency_key) WHERE idempotency_key IS NOT NULL")
		}
		if txErr == nil {
			_, txErr = tx.Exec("PRAGMA user_version=3")
		}
		if txErr == nil {
			txErr = tx.Commit()
		} else if tx != nil {
			_ = tx.Rollback()
		}
		if txErr != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", txErr)
		}
		version = 3
	}
	if version == 3 && !readonly {
		tx, txErr := db.Begin()
		if txErr == nil {
			_, txErr = tx.Exec(`CREATE TABLE upload_attempts (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 key_id TEXT NOT NULL REFERENCES keys(id),
 backup_id TEXT NOT NULL REFERENCES backups(id),
 attempted_at INTEGER NOT NULL
)`)
		}
		if txErr == nil {
			_, txErr = tx.Exec("CREATE INDEX upload_attempts_key_time ON upload_attempts(key_id,attempted_at)")
		}
		if txErr == nil {
			// Schema v3 retained only the most recent started_at for a failed
			// idempotent retry. Import the one historical attempt that can be
			// reconstructed for every backup without inventing lost history.
			_, txErr = tx.Exec("INSERT INTO upload_attempts(key_id,backup_id,attempted_at) SELECT key_id,id,started_at FROM backups")
		}
		if txErr == nil {
			_, txErr = tx.Exec("PRAGMA user_version=4")
		}
		if txErr == nil {
			txErr = tx.Commit()
		} else if tx != nil {
			_ = tx.Rollback()
		}
		if txErr != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", txErr)
		}
		version = 4
	}
	if version == 4 && !readonly {
		if err = migrateV4ToV5(db, a); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", err)
		}
		version = 5
	}
	if version == 5 && !readonly {
		if err = migrateV5ToV6(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", err)
		}
		version = 6
	}
	if version == 6 && !readonly {
		if err = migrateV6ToV7(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", err)
		}
		version = 7
	}
	if version == 7 && !readonly {
		if err = migrateV7ToV8(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", err)
		}
		version = 8
	}
	if version == 8 && !readonly {
		if err = migrateV8ToV9(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", err)
		}
		version = 9
	}
	if version == 9 && !readonly {
		if err = migrateV9ToV10(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", err)
		}
		version = 10
	}
	if version == 10 && !readonly {
		if err = migrateV10ToV11(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", err)
		}
		version = 11
	}
	if version == 11 && !readonly {
		if err = migrateV11ToV12(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrazione schema database: %w", err)
		}
		version = 12
	}
	if version < 1 || version > 12 {
		db.Close()
		return nil, errors.New("schema database non supportato; eseguire init per un nuovo archivio")
	}
	return &Store{db: db, schemaVersion: version}, nil
}
func Init(root string) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err := validatePrivateDirectory(root); err != nil {
		return err
	}
	for _, d := range []string{"incoming", "backups", "quarantine", "maintenance"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0700); err != nil {
			return err
		}
	}
	if err := ValidateState(root); err != nil {
		return err
	}
	path := filepath.Join(root, "metadata.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("inizializzazione: %w", err)
	}
	f.Close()
	a, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: a, RawQuery: "_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL"}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(schema); err != nil {
		return err
	}
	for _, p := range model.DefaultProfiles() {
		if _, err = tx.Exec("INSERT INTO profiles VALUES(?,?,?,?,?)", p.Name, p.TotalBytes, p.MaxBackupBytes, p.UploadsPerDay, p.Concurrent); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ValidateState rejects archive directories which could be traversed or
// modified by another local user. It deliberately reports unsafe existing
// permissions instead of changing them behind the operator's back.
func ValidateState(root string) error {
	for _, item := range []struct {
		path  string
		modes []os.FileMode
	}{
		{root, []os.FileMode{0700, 0710}},
		{filepath.Join(root, "incoming"), []os.FileMode{0700}},
		{filepath.Join(root, "backups"), []os.FileMode{0700, 0770}},
		{filepath.Join(root, "quarantine"), []os.FileMode{0700, 0770}},
		{filepath.Join(root, "maintenance"), []os.FileMode{0700, 0770}},
	} {
		if err := validateDirectoryModes(item.path, item.modes...); err != nil {
			return err
		}
	}
	return nil
}

// PrepareMaintenanceState adds only the fixed directories introduced by v5.
// It is safe for upgrades and never alters existing permissions or contents.
func PrepareMaintenanceState(root string) error {
	for _, name := range []string{"quarantine", "maintenance"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	return validateDirectoryModes(path, 0700)
}

func validateDirectoryModes(path string, allowed ...os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("directory archivio non valida o simbolica: %s", path)
	}
	validMode := false
	for _, mode := range allowed {
		if info.Mode().Perm() == mode {
			validMode = true
			break
		}
	}
	if !validMode {
		return fmt.Errorf("directory archivio con permessi non sicuri: %s", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int64(stat.Uid) != int64(os.Geteuid()) {
		return fmt.Errorf("directory archivio non appartenente all'utente corrente: %s", path)
	}
	return nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) PutProfile(p model.Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO profiles VALUES(?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET total_bytes=excluded.total_bytes,max_backup_bytes=excluded.max_backup_bytes,uploads_per_day=excluded.uploads_per_day,concurrent_uploads=excluded.concurrent_uploads`, p.Name, p.TotalBytes, p.MaxBackupBytes, p.UploadsPerDay, p.Concurrent)
	return err
}
func (s *Store) Profiles() ([]model.Profile, error) {
	rows, err := s.db.Query("SELECT name,total_bytes,max_backup_bytes,uploads_per_day,concurrent_uploads FROM profiles ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Profile{}
	for rows.Next() {
		var p model.Profile
		if err = rows.Scan(&p.Name, &p.TotalBytes, &p.MaxBackupBytes, &p.UploadsPerDay, &p.Concurrent); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) CreateKey(name, profile string) (model.Key, string, error) {
	var k model.Key
	if strings.TrimSpace(name) == "" || len(name) > 255 {
		return k, "", errors.New("nome chiave obbligatorio, massimo 255 byte")
	}
	id, err := model.NewID()
	if err != nil {
		return k, "", err
	}
	token, err := model.NewToken()
	if err != nil {
		return k, "", err
	}
	_, err = s.db.Exec("INSERT INTO keys(id,name,token_hash,profile) VALUES(?,?,?,?)", id, name, model.TokenHash(token), profile)
	if err != nil {
		return k, "", err
	}
	return model.Key{ID: id, Name: name, Profile: profile}, token, nil
}
func (s *Store) Keys() ([]model.Key, error) {
	rows, err := s.db.Query("SELECT id,name,profile,revoked FROM keys ORDER BY name,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Key{}
	for rows.Next() {
		var k model.Key
		if err = rows.Scan(&k.ID, &k.Name, &k.Profile, &k.Revoked); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
func changed(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}
func (s *Store) Revoke(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = changed(tx.Exec("UPDATE keys SET revoked=1 WHERE id=?", id)); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE backups SET status='complete' WHERE key_id=? AND status='deleting' AND id IN
	 (SELECT backup_id FROM retention_operations WHERE state='requested' AND kind='quarantine')`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE backups SET status='quarantined' WHERE key_id=? AND status='purging' AND id IN
	 (SELECT backup_id FROM retention_operations WHERE state='requested' AND kind='purge')`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE retention_operations SET state='cancelled',ticket_hash='',lease_generation=0,completed_at=unixepoch(),
	 error='annullata automaticamente: chiave revocata' WHERE state='requested' AND kind IN ('quarantine','purge')
	 AND backup_id IN (SELECT id FROM backups WHERE key_id=?)`, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Assign(id, profile string) error {
	return changed(s.db.Exec("UPDATE keys SET profile=? WHERE id=?", profile, id))
}

// Reserve atomically applies limits across connections and administrator processes.
// Accepted attempts count for a rolling 24h even when the upload later fails.
func (s *Store) Reserve(token string, m model.Metadata, size int64, digest string, now time.Time) (model.Backup, error) {
	return s.ReserveIdempotent(token, m, size, digest, "", now)
}

// ReserveIdempotent restituisce la ricevuta esistente per una richiesta già
// completata e impedisce che la stessa operazione crei più backup.
func (s *Store) ReserveIdempotent(token string, m model.Metadata, size int64, digest, idempotencyKey string, now time.Time) (model.Backup, error) {
	return s.ReserveIdempotentChecked(token, m, size, digest, idempotencyKey, now, nil)
}

// ReserveIdempotentChecked runs check inside the same immediate transaction
// which creates the reservation. reservedBytes includes every active upload
// and the candidate request, so their state cannot change between the check
// and the catalogue commit.
func (s *Store) ReserveIdempotentChecked(token string, m model.Metadata, size int64, digest, idempotencyKey string, now time.Time, check func(reservedBytes int64) error) (model.Backup, error) {
	var b model.Backup
	if !model.ValidToken(token) {
		return b, model.ErrUnauthorized
	}
	if err := m.Validate(); err != nil {
		return b, err
	}
	if size <= 0 || !model.ValidDigest(digest) {
		return b, model.ErrSize
	}
	if idempotencyKey != "" && !model.ValidIdempotencyKey(idempotencyKey) {
		return b, model.ErrIdempotencyConflict
	}
	tx, err := s.db.Begin()
	if err != nil {
		return b, err
	}
	defer tx.Rollback()
	var p model.Profile
	var keyID string
	err = tx.QueryRow(`SELECT k.id,p.name,p.total_bytes,p.max_backup_bytes,p.uploads_per_day,p.concurrent_uploads FROM keys k JOIN profiles p ON p.name=k.profile WHERE k.token_hash=? AND k.revoked=0`, model.TokenHash(token)).Scan(&keyID, &p.Name, &p.TotalBytes, &p.MaxBackupBytes, &p.UploadsPerDay, &p.Concurrent)
	if errors.Is(err, sql.ErrNoRows) {
		return b, model.ErrUnauthorized
	}
	if err != nil {
		return b, err
	}
	existingID := ""
	if idempotencyKey != "" {
		row := tx.QueryRow(`SELECT id,status,size_bytes,sha256,received_at,key_id,description,original_name,content_format,started_at,failure,idempotency_key,quarantined_at,purge_not_before,purged_at FROM backups WHERE key_id=? AND idempotency_key=?`, keyID, idempotencyKey)
		existing, lookupErr := scanBackup(row)
		if lookupErr == nil {
			if existing.Size != size || existing.SHA256 != digest || existing.Metadata != m {
				return b, model.ErrIdempotencyConflict
			}
			switch existing.Status {
			case "complete":
				return existing, nil
			case "receiving":
				return b, model.ErrIdempotencyInProgress
			case "failed":
				existingID = existing.ID
			case "deleting", "quarantined", "purging", "deleted":
				return b, model.ErrIdempotencyUnavailable
			default:
				return b, errors.New("stato deposito non valido")
			}
		} else if !errors.Is(lookupErr, sql.ErrNoRows) {
			return b, lookupErr
		}
	}
	if size > p.MaxBackupBytes {
		return b, model.ErrSize
	}
	var used, active, attempts int64
	err = tx.QueryRow(`SELECT COALESCE(SUM(CASE WHEN status IN ('receiving','complete','deleting','quarantined','purging') THEN size_bytes ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='receiving' THEN 1 ELSE 0 END),0) FROM backups WHERE key_id=?`, keyID).Scan(&used, &active)
	if err == nil {
		err = tx.QueryRow("SELECT COUNT(*) FROM upload_attempts WHERE key_id=? AND attempted_at>=?", keyID, now.Add(-24*time.Hour).Unix()).Scan(&attempts)
	}
	if err != nil {
		return b, err
	}
	if used > p.TotalBytes || size > p.TotalBytes-used {
		return b, model.ErrQuota
	}
	if active >= p.Concurrent {
		return b, model.ErrConcurrent
	}
	if attempts >= p.UploadsPerDay {
		return b, model.ErrRate
	}
	if check != nil {
		var reserved int64
		if err = tx.QueryRow("SELECT COALESCE(SUM(size_bytes),0) FROM backups WHERE status='receiving'").Scan(&reserved); err != nil {
			return b, err
		}
		if reserved < 0 || size > int64(^uint64(0)>>1)-reserved {
			return b, errors.New("prenotazioni disco non valide")
		}
		if err = check(reserved + size); err != nil {
			return b, err
		}
	}
	id := existingID
	if id == "" {
		id, err = model.NewID()
		if err != nil {
			return b, err
		}
		var storedKey any
		if idempotencyKey != "" {
			storedKey = idempotencyKey
		}
		_, err = tx.Exec(`INSERT INTO backups(id,key_id,description,original_name,size_bytes,sha256,status,started_at,content_format,idempotency_key) VALUES(?,?,?,?,?,?,'receiving',?,?,?)`, id, keyID, m.Description, m.OriginalName, size, digest, now.Unix(), m.ContentFormat, storedKey)
	} else {
		_, err = tx.Exec(`UPDATE backups SET status='receiving',started_at=?,received_at='',failure='' WHERE id=? AND status='failed'`, now.Unix(), id)
	}
	if err != nil {
		return b, err
	}
	// Only the rolling 24-hour window is observable and used for admission.
	// Reclaim older rows for active keys before adding the new attempt.
	if _, err = tx.Exec("DELETE FROM upload_attempts WHERE key_id=? AND attempted_at<?", keyID, now.Add(-24*time.Hour).Unix()); err != nil {
		return b, err
	}
	if _, err = tx.Exec("INSERT INTO upload_attempts(key_id,backup_id,attempted_at) VALUES(?,?,?)", keyID, id, now.Unix()); err != nil {
		return b, err
	}
	if err = tx.Commit(); err != nil {
		return b, err
	}
	return model.Backup{Receipt: model.Receipt{ID: id, Status: "receiving", Size: size, SHA256: digest}, KeyID: keyID, IdempotencyKey: idempotencyKey, Metadata: m, StartedAt: now.Unix()}, nil
}
func (s *Store) Complete(id string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE backups SET status='complete',received_at=?,failure='' WHERE id=? AND status='receiving'", now.UTC().Format(time.RFC3339Nano), id)
	if err = changed(res, err); err != nil {
		return err
	}
	if err = evaluateCompletedInTransaction(tx, id, now.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Fail(id, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE backups SET status='failed',failure=? WHERE id=? AND status='receiving'", reason, id)
	if err = changed(res, err); err != nil {
		return err
	}
	var keyID string
	if err = tx.QueryRow(`SELECT key_id FROM backups WHERE id=?`, id).Scan(&keyID); err != nil {
		return err
	}
	if err = openAnomalyTx(tx, "upload-failed:"+id, "upload", keyID, id, reason, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func evaluateCompletedInTransaction(tx *sql.Tx, id string, now time.Time) error {
	var keyID string
	var size int64
	if err := tx.QueryRow(`SELECT key_id,size_bytes FROM backups WHERE id=?`, id).Scan(&keyID, &size); err != nil {
		return err
	}
	var raw string
	var reliable, review bool
	err := tx.QueryRow(`SELECT model_json,reliable,review_required FROM monitoring_models WHERE key_id=? AND revision=(SELECT model_revision FROM automation_state WHERE id=1)`, keyID).Scan(&raw, &reliable, &review)
	if errors.Is(err, sql.ErrNoRows) || !reliable || review {
		return nil
	}
	if err != nil {
		return err
	}
	var learned policy.KeyModel
	if err = json.Unmarshal([]byte(raw), &learned); err != nil {
		return err
	}
	loc, err := time.LoadLocation(learned.Timezone)
	if err != nil {
		return err
	}
	local := now.In(loc)
	type scheduledAppointment struct {
		appointment policy.Appointment
		at          time.Time
	}
	var appointments []scheduledAppointment
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	for offset := -1; offset <= 1; offset++ {
		day := dayStart.AddDate(0, 0, offset)
		for _, appointment := range learned.Schedule[day.Weekday()] {
			y, m, d := day.Date()
			appointments = append(appointments, scheduledAppointment{appointment: appointment,
				at: time.Date(y, m, d, appointment.MinuteOfDay/60, appointment.MinuteOfDay%60, 0, 0, loc)})
		}
	}
	best := -1
	bestDistance := time.Duration(1<<63 - 1)
	for i, appointment := range appointments {
		distance := now.Sub(appointment.at)
		if distance < 0 {
			distance = -distance
		}
		if distance <= policy.ScheduleTolerance && distance < bestDistance {
			best, bestDistance = i, distance
		}
	}
	if best < 0 {
		return openAnomalyTx(tx, "arrival-time:"+id, "time", keyID, id, "completamento fuori dalla tolleranza oraria del modello", now)
	}
	appointment := appointments[best].appointment
	days := now.Sub(learned.LearnedAt).Hours() / 24
	expected := float64(appointment.MedianSize) + appointment.GrowthPerDay*days
	if expected <= 0 || float64(size) < expected*0.8 || float64(size) > expected*1.2 {
		if err = openAnomalyTx(tx, "arrival-size:"+id, "size", keyID, id, "dimensione fuori dalla tolleranza del 20%", now); err != nil {
			return err
		}
	}
	rows, err := tx.Query(`SELECT id,received_at,size_bytes FROM backups WHERE key_id=? AND status IN ('complete','deleting','quarantined','purging','deleted') AND received_at<>''`, keyID)
	if err != nil {
		return err
	}
	var observations []policy.Observation
	for rows.Next() {
		var backupID, value string
		var backupSize int64
		if err = rows.Scan(&backupID, &value, &backupSize); err != nil {
			rows.Close()
			return err
		}
		at, parseErr := time.Parse(time.RFC3339Nano, value)
		if parseErr == nil {
			observations = append(observations, policy.Observation{BackupID: backupID, KeyID: keyID, At: at, Size: backupSize})
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, finding := range policy.Check(learned, observations, now) {
		if finding.Kind == "extra" {
			// Include earlier copies too (including equal-time ID tie breaks).
			// Exclusions apply at receipt time, not at this detection time.
			if _, err = openAnomalyAtTx(tx, finding.StableKey, finding.Kind, finding.KeyID, finding.BackupID, finding.Detail, finding.At, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func openAnomalyTx(tx *sql.Tx, stableKey, kind, keyID, backupID, detail string, now time.Time) error {
	_, err := openAnomalyAtTx(tx, stableKey, kind, keyID, backupID, detail, now, now)
	return err
}

type scanner interface{ Scan(...any) error }

func scanBackup(r scanner) (model.Backup, error) {
	var b model.Backup
	err := r.Scan(&b.ID, &b.Status, &b.Size, &b.SHA256, &b.ReceivedAt, &b.KeyID, &b.Description, &b.OriginalName, &b.ContentFormat, &b.StartedAt, &b.Failure, &b.IdempotencyKey, &b.QuarantinedAt, &b.PurgeNotBefore, &b.PurgedAt)
	return b, err
}
func (s *Store) backupCols() string {
	format := "content_format"
	idempotency := "COALESCE(idempotency_key,'')"
	retention := "quarantined_at,purge_not_before,purged_at"
	if s.schemaVersion == 1 {
		format = "''"
	}
	if s.schemaVersion < 3 {
		idempotency = "''"
	}
	if s.schemaVersion < 5 {
		retention = "0,0,0"
	}
	return "id,status,size_bytes,sha256,received_at,key_id,description,original_name," + format + ",started_at,failure," + idempotency + "," + retention
}
func (s *Store) Backup(id string) (model.Backup, error) {
	return scanBackup(s.db.QueryRow("SELECT "+s.backupCols()+" FROM backups WHERE id=?", id))
}
func (s *Store) Backups(key string, limit int) ([]model.Backup, error) {
	if limit < 1 || limit > 10000 {
		return nil, errors.New("limite elenco tra 1 e 10000")
	}
	return s.queryBackups("SELECT "+s.backupCols()+" FROM backups WHERE (?='' OR key_id=?) ORDER BY started_at DESC,id DESC LIMIT ?", key, key, limit)
}

// SnapshotBackups returns the complete catalogue needed to correlate disk and
// catalogue state. It deliberately has no display-oriented row limit.
func (s *Store) SnapshotBackups() ([]model.Backup, error) {
	return s.queryBackups("SELECT " + s.backupCols() + " FROM backups ORDER BY started_at DESC,id DESC")
}
func (s *Store) Pending() ([]model.Backup, error) {
	return s.queryBackups("SELECT " + s.backupCols() + " FROM backups WHERE status='receiving'")
}
func (s *Store) queryBackups(q string, args ...any) ([]model.Backup, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (s *Store) ReservedBytes() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT COALESCE(SUM(size_bytes),0) FROM backups WHERE status='receiving'").Scan(&n)
	return n, err
}
func (s *Store) Quota(keyID string, now time.Time) (model.Quota, error) {
	q := model.Quota{KeyID: keyID}
	err := s.db.QueryRow(`SELECT p.name,p.total_bytes,p.max_backup_bytes,p.uploads_per_day,p.concurrent_uploads FROM keys k JOIN profiles p ON p.name=k.profile WHERE k.id=?`, keyID).Scan(&q.Profile.Name, &q.Profile.TotalBytes, &q.Profile.MaxBackupBytes, &q.Profile.UploadsPerDay, &q.Profile.Concurrent)
	if err != nil {
		return q, err
	}
	err = s.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN status IN ('complete','deleting','quarantined','purging') THEN size_bytes ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='receiving' THEN size_bytes ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='receiving' THEN 1 ELSE 0 END),0) FROM backups WHERE key_id=?`, keyID).Scan(&q.Used, &q.Reserved, &q.Active)
	if err != nil {
		return q, err
	}
	if s.schemaVersion < 4 {
		// Read-only v1-v3 catalogues cannot be migrated. Their best available
		// history is the latest started_at stored on each backup.
		err = s.db.QueryRow("SELECT COUNT(*) FROM backups WHERE key_id=? AND started_at>=?", keyID, now.Add(-24*time.Hour).Unix()).Scan(&q.Attempts24h)
	} else {
		err = s.db.QueryRow("SELECT COUNT(*) FROM upload_attempts WHERE key_id=? AND attempted_at>=?", keyID, now.Add(-24*time.Hour).Unix()).Scan(&q.Attempts24h)
	}
	return q, err
}
