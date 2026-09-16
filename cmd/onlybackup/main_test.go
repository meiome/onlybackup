package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/meiome/onlybackup/internal/filesecurity"
	"github.com/meiome/onlybackup/internal/model"
)

func TestWriteReceiptCreatesPrivateFileWithoutOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.json")
	receipt := model.Receipt{ID: "00112233445566778899aabbccddeeff", Status: "complete", Size: 10}
	if err := writeReceipt(path, receipt); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = filesecurity.CheckPrivate(path, info); err != nil {
		t.Fatal(err)
	}
	if err = writeReceipt(path, receipt); err == nil {
		t.Fatal("ricevuta esistente sovrascritta")
	}
}
