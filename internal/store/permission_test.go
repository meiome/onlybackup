package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/meiome/onlybackup/internal/model"
)

func TestFinalPermissionIsAtomicAndSurvivesPolicyChanges(t *testing.T) {
	for _, event := range []string{"revoke", "relearn", "block", "rollback"} {
		t.Run(event, func(t *testing.T) {
			s, key, token := setup(t, model.Profile{Name: "final-permission", TotalBytes: 2000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
			now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
			old := completedBackup(t, s, token, "final-old-1234567890", now.AddDate(0, 0, -10))
			completedBackup(t, s, token, "final-new-1234567890", now.AddDate(0, 0, -1))
			enableRetentionForTest(t, s, now)
			op, err := s.RequestQuarantine(old.ID, "manual", "admin", "final permission", now)
			if err != nil {
				t.Fatal(err)
			}
			op, err = authorizeOperationForTest(t, s, op.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			if op.ExecutionCommitted {
				t.Fatal("provisional ticket already irrevocable")
			}
			if event == "rollback" {
				if _, err = s.db.Exec(`CREATE TRIGGER fail_final_permission BEFORE UPDATE OF execution_committed ON retention_operations BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
					t.Fatal(err)
				}
				if err = validateOperationForTest(t, s, op.ID, op.Ticket, now); err == nil {
					t.Fatal("failed transaction accepted")
				}
				stored, err := s.Operation(op.ID)
				if err != nil || stored.ExecutionCommitted {
					t.Fatalf("permission escaped rollback: %+v %v", stored, err)
				}
				return
			}
			if err = validateOperationForTest(t, s, op.ID, op.Ticket, now); err != nil {
				t.Fatal(err)
			}
			switch event {
			case "revoke":
				err = s.Revoke(key)
			case "relearn":
				if err = s.BeginRelearn(); err == nil {
					t.Fatal("relearn accepted with pending work")
				}
				// Even a revision changed by another internal check must not
				// erase an already committed permission.
				err = s.SetCheckState(model.MonitoringRegular, 8, now)
			case "block":
				_, err = s.OpenAnomaly("independent-file", "file_missing", key, "", "independent integrity failure", now)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = validateOperationForTest(t, s, op.ID, op.Ticket, now); err != nil {
				t.Fatalf("permission cancelled by %s: %v", event, err)
			}
			if _, err = authorizeOperationForTest(t, s, op.ID, now); err == nil {
				t.Fatal("ticket replaced before reconciliation")
			}
			if err = s.CancelQuarantineRequest(old.ID, "admin", now); err == nil {
				t.Fatal("committed request cancelled")
			}
			if err = s.ResetAuthorized(op.ID, now); err != nil {
				t.Fatal(err)
			}
			retried, err := authorizeOperationForTest(t, s, op.ID, now)
			if err != nil || !retried.ExecutionCommitted || retried.Ticket == op.Ticket {
				t.Fatalf("retry lost permission or reused ticket: %+v %v", retried, err)
			}
			if err = validateOperationForTest(t, s, op.ID, op.Ticket, now); err == nil {
				t.Fatal("old ticket still valid")
			}
			if err = confirmOperationForTest(t, s, op.ID, retried.Ticket, true, "", now); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestV8MigrationDoesNotMakeLegacyPermissionIrrevocable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := Init(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "metadata.db")
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	key, token, err := s.CreateKey("legacy v8", "XS")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	old := completedBackup(t, s, token, "v8-old-1234567890123", now.AddDate(0, 0, -10))
	completedBackup(t, s, token, "v8-new-1234567890123", now.AddDate(0, 0, -1))
	enableRetentionForTest(t, s, now)
	op, err := s.RequestQuarantine(old.ID, "manual", "admin", "v8", now)
	if err != nil {
		t.Fatal(err)
	}
	op, err = authorizeOperationForTest(t, s, op.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TABLE key_retention; ALTER TABLE automation_state DROP COLUMN manual_paused; ALTER TABLE automation_state DROP COLUMN manual_pause_reason; ALTER TABLE retention_operations DROP COLUMN execution_committed; PRAGMA user_version=8`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	readOnly, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if readOnly.schemaVersion != 8 {
		t.Fatal("read-only open migrated v8")
	}
	readOnly.Close()
	s, err = Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	preserved, err := s.Operation(op.ID)
	if err != nil || preserved.ExecutionCommitted || preserved.State != "authorized" || s.schemaVersion != 10 {
		t.Fatalf("legacy permission changed: %+v %v", preserved, err)
	}
	if err = s.Revoke(key.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ResetAuthorized(op.ID, now); err != nil {
		t.Fatal(err)
	}
	preserved, err = s.Operation(op.ID)
	if err != nil || preserved.State != "cancelled" {
		t.Fatalf("legacy revocation lost: %+v %v", preserved, err)
	}
	if err = s.DatabaseIntegrity(); err != nil {
		t.Fatal(err)
	}
	if err = migrateV8ToV9(s.db); err != nil {
		t.Fatal("migration not idempotent:", err)
	}
}

func TestV9MigrationRollback(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A malformed predecessor must not advance the version.
	if _, err = db.Exec(`PRAGMA user_version=8`); err != nil {
		t.Fatal(err)
	}
	if err = migrateV8ToV9(db); err == nil {
		t.Fatal("migration of missing table succeeded")
	}
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 8 {
		t.Fatalf("failed migration advanced version: %d %v", version, err)
	}
}

func TestV9MigrationRollsBackAddedColumnOnLateFailure(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE retention_operations(id INTEGER PRIMARY KEY); PRAGMA user_version=8`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = conn.Raw(func(driver any) error {
		driver.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, a, b, c string) int {
			if op == sqlite3.SQLITE_PRAGMA && a == "user_version" && b == "9" {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if err = migrateV8ToV9(db); err == nil {
		t.Fatal("injected late failure accepted")
	}
	var version, count int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 8 {
		t.Fatalf("version escaped rollback: %d %v", version, err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('retention_operations') WHERE name='execution_committed'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("column escaped rollback: %d %v", count, err)
	}
}

func TestRevocationRacesFinalPermissionAtomically(t *testing.T) {
	for i := 0; i < 10; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			s, key, token := setup(t, model.Profile{Name: "race-permission", TotalBytes: 2000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
			now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
			old := completedBackup(t, s, token, "race-old-1234567890", now.AddDate(0, 0, -10))
			completedBackup(t, s, token, "race-new-1234567890", now.AddDate(0, 0, -1))
			enableRetentionForTest(t, s, now)
			op, err := s.RequestQuarantine(old.ID, "manual", "admin", "race", now)
			if err != nil {
				t.Fatal(err)
			}
			lease := leaseForTest(t, s, 0, now)
			op, err = s.AuthorizeOperation(op.ID, lease, now)
			if err != nil {
				t.Fatal(err)
			}
			var seq int
			var name, path string
			if err = s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
				t.Fatal(err)
			}
			other, err := Open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			start := make(chan struct{})
			permission := make(chan error, 1)
			revocation := make(chan error, 1)
			go func() { <-start; permission <- s.ValidateOperation(op.ID, op.Ticket, lease, now) }()
			go func() { <-start; revocation <- other.Revoke(key) }()
			close(start)
			permissionErr, revocationErr := <-permission, <-revocation
			if revocationErr != nil {
				t.Fatal(revocationErr)
			}
			current, err := s.Operation(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.ExecutionCommitted != (permissionErr == nil) {
				t.Fatalf("non-atomic result: %+v %v", current, permissionErr)
			}
			if err = s.ResetAuthorized(op.ID, now); err != nil {
				t.Fatal(err)
			}
			current, err = s.Operation(op.ID)
			want := "cancelled"
			if permissionErr == nil {
				want = "authorized"
			}
			if err != nil || current.State != want {
				t.Fatalf("race reconciliation: %+v want=%s err=%v", current, want, err)
			}
		})
	}
}
