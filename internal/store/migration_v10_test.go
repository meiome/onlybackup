package store

import (
	"database/sql"
	"github.com/meiome/onlybackup/internal/model"
	"path/filepath"
	"testing"
	"time"
)

func TestV10PreservesExistingMinimumAndBlocks(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "regular", true: "blocked"}[blocked], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "archive")
			if err := Init(root); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "metadata.db")
			s, err := Open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			key, _, err := s.CreateKey("legacy", "XS")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
			enableRetentionForTest(t, s, now)
			if _, err = s.db.Exec(`UPDATE automation_state SET retention_days=45,deletion_blocked=?,block_reason='legacy reason'; ALTER TABLE automation_state DROP COLUMN mail_config_version; ALTER TABLE automation_state DROP COLUMN activation_required; ALTER TABLE automation_state DROP COLUMN activation_pending; ALTER TABLE automation_state DROP COLUMN resume_pending; ALTER TABLE automation_state DROP COLUMN resume_requested_at; DROP TABLE key_retention; ALTER TABLE automation_state DROP COLUMN manual_paused; ALTER TABLE automation_state DROP COLUMN manual_pause_reason; PRAGMA user_version=9`, blocked); err != nil {
				t.Fatal(err)
			}
			s.Close()
			ro, err := Open(path, true)
			if err != nil {
				t.Fatal(err)
			}
			if ro.schemaVersion != 9 {
				t.Fatal("read-only migration")
			}
			ro.Close()
			s, err = Open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			status, err := s.AutomationStatus()
			if err != nil {
				t.Fatal(err)
			}
			if s.schemaVersion != 12 || !status.Enabled || status.RetentionDaysForKey(key.ID) != 45 || status.ManualPaused != blocked || status.DeletionBlocked != blocked {
				t.Fatalf("migration changed policy: %+v", status)
			}
			if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
				t.Fatal(err)
			}
			if err = s.ResumeRetention(now); err != nil {
				t.Fatal(err)
			}
			if err = s.SetKeyRetentionDays(key.ID, 30); err != nil {
				t.Fatal(err)
			}
			if err = s.BeginRelearn(); err != nil {
				t.Fatal(err)
			}
			status, err = s.AutomationStatus()
			if err != nil || status.RetentionDaysForKey(key.ID) != 30 {
				t.Fatalf("relearn changed minimum: %+v %v", status, err)
			}
			newKey, _, err := s.CreateKey("new", "XS")
			if err != nil {
				t.Fatal(err)
			}
			status, err = s.AutomationStatus()
			if err != nil || status.RetentionDaysForKey(newKey.ID) != 45 {
				t.Fatalf("new key lost preserved default: %+v %v", status, err)
			}
		})
	}
}

func TestV10MigrationRollsBackOnLateFailure(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "broken-v9.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Missing keys table fails after ALTERs and the hold update, not before them.
	if _, err = db.Exec(`CREATE TABLE automation_state(id INTEGER PRIMARY KEY,deletion_blocked INTEGER,block_reason TEXT,retention_days INTEGER); INSERT INTO automation_state VALUES(1,1,'manual pause',30); PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	if err = migrateV9ToV10(db); err == nil {
		t.Fatal("malformed predecessor accepted")
	}
	var version, columns, tables int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 9 {
		t.Fatalf("migration advanced on failure: %d %v", version, err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('automation_state') WHERE name IN ('manual_paused','manual_pause_reason')`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("columns escaped rollback: %d %v", columns, err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='key_retention'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("table escaped rollback: %d %v", tables, err)
	}
}
