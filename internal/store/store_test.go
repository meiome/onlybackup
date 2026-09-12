package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

func setup(t *testing.T, p model.Profile) (*Store, string, string) {
	t.Helper()
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.PutProfile(p); err != nil {
		t.Fatal(err)
	}
	k, token, err := s.CreateKey("cliente condiviso", p.Name)
	if err != nil {
		t.Fatal(err)
	}
	return s, k.ID, token
}
func reserve(s *Store, token string, size int64, now time.Time) (model.Backup, error) {
	return s.Reserve(token, model.Metadata{Description: "database", OriginalName: "dump.sql"}, size, strings.Repeat("a", 64), now)
}
func TestQuotaReservationsAndNoAutomaticDeletion(t *testing.T) {
	s, key, token := setup(t, model.Profile{Name: "pippo", TotalBytes: 10, MaxBackupBytes: 8, UploadsPerDay: 20, Concurrent: 2})
	now := time.Now()
	b, err := reserve(s, token, 6, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reserve(s, token, 5, now); !errors.Is(err, model.ErrQuota) {
		t.Fatal(err)
	}
	if err = s.Complete(b.ID, now); err != nil {
		t.Fatal(err)
	}
	q, err := s.Quota(key, now)
	if err != nil || q.Used != 6 || q.Reserved != 0 {
		t.Fatal(q, err)
	}
	if _, err = reserve(s, token, 9, now); !errors.Is(err, model.ErrSize) {
		t.Fatal(err)
	}
	rows, _ := s.Backups(key, 100)
	if len(rows) != 1 || rows[0].Status != "complete" {
		t.Fatal(rows)
	}
}
func TestFailureFreesSpaceButCountsAttempt(t *testing.T) {
	s, key, token := setup(t, model.Profile{Name: "mario", TotalBytes: 10, MaxBackupBytes: 10, UploadsPerDay: 1, Concurrent: 1})
	now := time.Now()
	b, err := reserve(s, token, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reserve(s, token, 1, now); err == nil {
		t.Fatal("parallel upload accepted")
	}
	if err = s.Fail(b.ID, "interrotto"); err != nil {
		t.Fatal(err)
	}
	q, _ := s.Quota(key, now)
	if q.Used != 0 || q.Reserved != 0 || q.Attempts24h != 1 {
		t.Fatal(q)
	}
	if _, err = reserve(s, token, 1, now); !errors.Is(err, model.ErrRate) {
		t.Fatal(err)
	}
	if _, err = reserve(s, token, 1, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
}
func TestIdempotentReservation(t *testing.T) {
	s, key, token := setup(t, model.Profile{Name: "idem", TotalBytes: 100, MaxBackupBytes: 100, UploadsPerDay: 1, Concurrent: 1})
	now := time.Now()
	metadata := model.Metadata{Description: "database", OriginalName: "dump.sql"}
	digest := strings.Repeat("a", 64)
	idempotencyKey, err := model.NewIdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.ReserveIdempotent(token, metadata, 10, digest, idempotencyKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveIdempotent(token, metadata, 10, digest, idempotencyKey, now); !errors.Is(err, model.ErrIdempotencyInProgress) {
		t.Fatalf("parallel duplicate: %v", err)
	}
	if err = s.Fail(b.ID, "rete interrotta"); err != nil {
		t.Fatal(err)
	}
	retry, err := s.ReserveIdempotent(token, metadata, 10, digest, idempotencyKey, now.Add(time.Minute))
	if err != nil || retry.ID != b.ID || retry.Status != "receiving" {
		t.Fatalf("failed retry: %+v %v", retry, err)
	}
	if err = s.Complete(retry.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	replay, err := s.ReserveIdempotent(token, metadata, 10, digest, idempotencyKey, now.Add(3*time.Minute))
	if err != nil || replay.ID != b.ID || replay.Status != "complete" {
		t.Fatalf("complete replay: %+v %v", replay, err)
	}
	if _, err = s.ReserveIdempotent(token, metadata, 10, strings.Repeat("b", 64), idempotencyKey, now); !errors.Is(err, model.ErrIdempotencyConflict) {
		t.Fatalf("changed replay: %v", err)
	}
	q, err := s.Quota(key, now.Add(3*time.Minute))
	if err != nil || q.Used != 10 || q.Attempts24h != 1 {
		t.Fatal(q, err)
	}
}
func TestConcurrentReservationAcrossConnections(t *testing.T) {
	s, _, token := setup(t, model.Profile{Name: "M", TotalBytes: 100, MaxBackupBytes: 100, UploadsPerDay: 100, Concurrent: 100})
	var dbPath string
	rows, err := s.db.Query("PRAGMA database_list")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int
		var name string
		if err = rows.Scan(&seq, &name, &dbPath); err != nil {
			t.Fatal(err)
		}
	}
	rows.Close()
	other, err := Open(dbPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := s
			if i%2 == 0 {
				db = other
			}
			_, err := reserve(db, token, 30, time.Now())
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, model.ErrQuota) {
			t.Fatal(err)
		}
	}
	if accepted != 3 {
		t.Fatalf("accepted %d reservations, expected 3", accepted)
	}
}
func TestRevocationAndProfileChange(t *testing.T) {
	s, key, token := setup(t, model.Profile{Name: "S", TotalBytes: 100, MaxBackupBytes: 100, UploadsPerDay: 10, Concurrent: 2})
	if err := s.PutProfile(model.Profile{Name: "small", TotalBytes: 1, MaxBackupBytes: 1, UploadsPerDay: 1, Concurrent: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign(key, "small"); err != nil {
		t.Fatal(err)
	}
	if _, err := reserve(s, token, 2, time.Now()); !errors.Is(err, model.ErrSize) {
		t.Fatal(err)
	}
	if err := s.Revoke(key); err != nil {
		t.Fatal(err)
	}
	if _, err := reserve(s, token, 1, time.Now()); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatal(err)
	}
	var hashed string
	if err := s.db.QueryRow("SELECT token_hash FROM keys WHERE id=?", key).Scan(&hashed); err != nil || hashed == token || hashed != model.TokenHash(token) {
		t.Fatal("credential not hashed", err)
	}
}
func TestReadOnlyAndInitialization(t *testing.T) {
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	if err := Init(root); err == nil {
		t.Fatal("reinitialized archive")
	}
	s, err := Open(filepath.Join(root, "metadata.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Profiles(); err != nil {
		t.Fatal(err)
	}
	if err = s.PutProfile(model.Profile{Name: "bad", TotalBytes: 1, MaxBackupBytes: 1, UploadsPerDay: 1, Concurrent: 1}); err == nil {
		t.Fatal("readonly mutation accepted")
	}
}

func TestContentFormatPersistsAndLegacySchemaMigrates(t *testing.T) {
	t.Run("new-schema", func(t *testing.T) {
		s, _, token := setup(t, model.Profile{Name: "age", TotalBytes: 1000, MaxBackupBytes: 1000, UploadsPerDay: 2, Concurrent: 1})
		metadata := model.Metadata{Description: "encrypted", OriginalName: "dump.sql", ContentFormat: model.ContentFormatAgeV1}
		b, err := s.Reserve(token, metadata, 100, strings.Repeat("b", 64), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Backup(b.ID)
		if err != nil || got.ContentFormat != model.ContentFormatAgeV1 {
			t.Fatalf("content format not persisted: %+v %v", got, err)
		}
	})

	t.Run("version-one-read-and-migrate", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy.db")
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`
CREATE TABLE backups (
 id TEXT PRIMARY KEY, key_id TEXT NOT NULL, description TEXT NOT NULL,
 original_name TEXT NOT NULL, size_bytes INTEGER NOT NULL, sha256 TEXT NOT NULL,
 status TEXT NOT NULL, started_at INTEGER NOT NULL, received_at TEXT NOT NULL DEFAULT '',
 failure TEXT NOT NULL DEFAULT ''
);
INSERT INTO backups(id,key_id,description,original_name,size_bytes,sha256,status,started_at,received_at)
VALUES('00112233445566778899aabbccddeeff','legacy-key','old','old.sql',3,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','complete',1,'2026-01-01T00:00:00Z');
PRAGMA user_version=1;
`)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}

		legacy, err := Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		backup, err := legacy.Backup("00112233445566778899aabbccddeeff")
		_ = legacy.Close()
		if err != nil || backup.ContentFormat != "" {
			t.Fatalf("legacy read: %+v %v", backup, err)
		}

		migrated, err := Open(path, false)
		if err != nil {
			t.Fatal(err)
		}
		defer migrated.Close()
		var version int
		if err = migrated.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
			t.Fatalf("schema version after migration: %d %v", version, err)
		}
		backup, err = migrated.Backup("00112233445566778899aabbccddeeff")
		if err != nil || backup.ContentFormat != "" {
			t.Fatalf("migrated legacy record: %+v %v", backup, err)
		}
	})
}
