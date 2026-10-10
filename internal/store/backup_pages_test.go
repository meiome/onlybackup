package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/meiome/onlybackup/internal/model"
)

func TestBackupPagesPreserveHistoryAcrossConcurrentDeposits(t *testing.T) {
	s, keyID, _ := setup(t, model.Profile{Name: "pages", TotalBytes: 10000, MaxBackupBytes: 1, UploadsPerDay: 10000, Concurrent: 2})
	empty, err := s.BackupPage("", 0)
	if err != nil || len(empty.Backups) != 0 || empty.Next != "" {
		t.Fatalf("empty page: %+v, %v", empty, err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := tx.Exec
	insert := func(i int, status string) {
		t.Helper()
		_, err := exec(`INSERT INTO backups(id,key_id,description,original_name,size_bytes,sha256,status,started_at) VALUES(?,?,?,?,1,?,?,?)`,
			fmt.Sprintf("%032x", i), keyID, "history", "db.sql", strings.Repeat("a", 64), status, i)
		if err != nil {
			t.Fatal(err)
		}
	}
	// A full second page must terminate without losing its final record.
	for i := 1; i <= model.BackupPageSize*2; i++ {
		insert(i*2, "deleted")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	exec = s.db.Exec
	first, err := s.BackupPage("", 0)
	if err != nil || len(first.Backups) != model.BackupPageSize || first.Next == "" {
		t.Fatalf("first page: rows=%d next=%q err=%v", len(first.Backups), first.Next, err)
	}
	// New random IDs may fall before or after the cursor; neither belongs
	// to this catalogue traversal. State changes must not move existing IDs.
	insert(1, "complete")
	insert(model.BackupPageSize*4+1, "complete")
	lastID := fmt.Sprintf("%032x", model.BackupPageSize*4)
	if _, err := s.db.Exec("UPDATE backups SET status='quarantined' WHERE id=?", lastID); err != nil {
		t.Fatal(err)
	}
	second, err := s.BackupPage(first.Next, first.Through)
	if err != nil || len(second.Backups) != model.BackupPageSize || second.Next != "" || second.Through != first.Through {
		t.Fatalf("second page: rows=%d next=%q err=%v", len(second.Backups), second.Next, err)
	}
	all := append(first.Backups, second.Backups...)
	for i, backup := range all {
		if backup.ID != fmt.Sprintf("%032x", (i+1)*2) {
			t.Fatalf("missing, duplicated or newly inserted record at %d: %s", i, backup.ID)
		}
	}
	if all[len(all)-1].Status != model.BackupQuarantined {
		t.Fatal("status change hid an existing record")
	}
}
