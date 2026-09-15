// Package store contains local SQLite operations. It is never linked into the public receiver.
package store

import (
	"database/sql"
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
 status TEXT NOT NULL CHECK(status IN ('receiving','complete','failed')),
	started_at INTEGER NOT NULL, received_at TEXT NOT NULL DEFAULT '', failure TEXT NOT NULL DEFAULT '',
	content_format TEXT NOT NULL DEFAULT '' CHECK(content_format IN ('','age-v1')),
	idempotency_key TEXT DEFAULT NULL CHECK(idempotency_key IS NULL OR (length(idempotency_key)>=16 AND length(idempotency_key)<=128))
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
PRAGMA user_version=4;
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
	if version != 1 && version != 2 && version != 3 && version != 4 {
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
	for _, d := range []string{"incoming", "backups"} {
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
	for _, path := range []string{root, filepath.Join(root, "incoming"), filepath.Join(root, "backups")} {
		if err := validatePrivateDirectory(path); err != nil {
			return err
		}
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("directory archivio non valida o simbolica: %s", path)
	}
	if info.Mode().Perm() != 0700 {
		return fmt.Errorf("directory archivio con permessi non sicuri (richiesto chmod 700): %s", path)
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
	return changed(s.db.Exec("UPDATE keys SET revoked=1 WHERE id=?", id))
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
		row := tx.QueryRow(`SELECT id,status,size_bytes,sha256,received_at,key_id,description,original_name,content_format,started_at,failure,idempotency_key FROM backups WHERE key_id=? AND idempotency_key=?`, keyID, idempotencyKey)
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
	err = tx.QueryRow(`SELECT COALESCE(SUM(CASE WHEN status IN ('receiving','complete') THEN size_bytes ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='receiving' THEN 1 ELSE 0 END),0) FROM backups WHERE key_id=?`, keyID).Scan(&used, &active)
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
	return changed(s.db.Exec("UPDATE backups SET status='complete',received_at=?,failure='' WHERE id=? AND status='receiving'", now.UTC().Format(time.RFC3339Nano), id))
}
func (s *Store) Fail(id, reason string) error {
	return changed(s.db.Exec("UPDATE backups SET status='failed',failure=? WHERE id=? AND status='receiving'", reason, id))
}

type scanner interface{ Scan(...any) error }

func scanBackup(r scanner) (model.Backup, error) {
	var b model.Backup
	err := r.Scan(&b.ID, &b.Status, &b.Size, &b.SHA256, &b.ReceivedAt, &b.KeyID, &b.Description, &b.OriginalName, &b.ContentFormat, &b.StartedAt, &b.Failure, &b.IdempotencyKey)
	return b, err
}
func (s *Store) backupCols() string {
	format := "content_format"
	idempotency := "COALESCE(idempotency_key,'')"
	if s.schemaVersion == 1 {
		format = "''"
	}
	if s.schemaVersion < 3 {
		idempotency = "''"
	}
	return "id,status,size_bytes,sha256,received_at,key_id,description,original_name," + format + ",started_at,failure," + idempotency
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
	err = s.db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN status='complete' THEN size_bytes ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='receiving' THEN size_bytes ELSE 0 END),0),COALESCE(SUM(CASE WHEN status='receiving' THEN 1 ELSE 0 END),0) FROM backups WHERE key_id=?`, keyID).Scan(&q.Used, &q.Reserved, &q.Active)
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
