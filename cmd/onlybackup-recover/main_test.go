package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meiome/onlybackup/internal/model"
)

func TestTargetFromReceipt(t *testing.T) {
	dir := t.TempDir()
	receiptPath := filepath.Join(dir, "receipt.json")
	archivePath := filepath.Join(dir, "archive.backup")
	receipt := model.Receipt{
		ID:         "00112233445566778899aabbccddeeff",
		Status:     "complete",
		Size:       123,
		SHA256:     strings.Repeat("a", 64),
		ReceivedAt: "2026-09-16T00:00:00Z",
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(receiptPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	target, err := targetFromReceipt(receiptPath, archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != receipt.ID || target.Source != archivePath || target.Size != receipt.Size || target.SHA256 != receipt.SHA256 {
		t.Fatalf("target inatteso: %+v", target)
	}
}

func TestTargetFromReceiptRejectsInvalidAndTrailingData(t *testing.T) {
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"failed":       `{"id":"00112233445566778899aabbccddeeff","status":"failed","size_bytes":1,"sha256":"` + strings.Repeat("a", 64) + `"}`,
		"invalid-date": `{"id":"00112233445566778899aabbccddeeff","status":"complete","size_bytes":1,"sha256":"` + strings.Repeat("a", 64) + `","received_at":"not-a-date"}`,
		"trailing":     `{"id":"00112233445566778899aabbccddeeff","status":"complete","size_bytes":1,"sha256":"` + strings.Repeat("a", 64) + `"}{}`,
		"unknown":      `{"id":"00112233445566778899aabbccddeeff","status":"complete","size_bytes":1,"sha256":"` + strings.Repeat("a", 64) + `","secret":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".json")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := targetFromReceipt(path, "archive"); err == nil {
				t.Fatal("ricevuta non valida accettata")
			}
		})
	}
	oversized := filepath.Join(dir, "oversized.json")
	if err := os.WriteFile(oversized, []byte(strings.Repeat(" ", (64<<10)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := targetFromReceipt(oversized, "archive"); err == nil {
		t.Fatal("ricevuta troppo grande accettata")
	}
}
