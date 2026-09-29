package localcli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meiome/onlybackup/internal/model"
)

func TestMonitoringChecksUsesAdminAPIAndLimit(t *testing.T) {
	call := func(method, path string, request, response any) error {
		if method != "GET" || path != "/v1/monitoring-checks?limit=37" || request != nil {
			t.Fatalf("richiesta inattesa: %s %s", method, path)
		}
		*response.(*[]model.MonitoringCheck) = []model.MonitoringCheck{{ID: 8, StartedAt: 10, FinishedAt: 20, PeriodFrom: 1, PeriodTo: 9, Status: "completed_clean", Summary: "nessuna anomalia"}}
		return nil
	}
	var out, errs bytes.Buffer
	err := printMonitoringChecks(call, 37, &out)
	if err != nil || !strings.Contains(out.String(), `"status": "completed_clean"`) || !strings.Contains(out.String(), `"summary": "nessuna anomalia"`) {
		t.Fatalf("monitoring checks: err=%v out=%s stderr=%s", err, out.String(), errs.String())
	}
}

func TestLocalAdministration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	var out, errs bytes.Buffer
	run := func(args ...string) error {
		out.Reset()
		errs.Reset()
		return Admin(append([]string{"--state", root}, args...), &out, &errs)
	}
	if err := run("init"); err != nil {
		t.Fatal(err)
	}
	if err := run("profiles", "set", "--name", "mario", "--total", "2GiB", "--max-backup", "1GiB", "--daily", "7", "--concurrent", "1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key")
	if err := run("keys", "create", "--name", "azienda", "--profile", "mario", "--out", path); err != nil {
		t.Fatal(err)
	}
	var k model.Key
	if err := json.Unmarshal(out.Bytes(), &k); err != nil {
		t.Fatal(err)
	}
	secret, _ := os.ReadFile(path)
	if len(secret) == 0 || strings.Contains(out.String(), strings.TrimSpace(string(secret))) {
		t.Fatal("secret exposed or missing")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if err := run("keys", "create", "--name", "other", "--profile", "mario", "--out", path); err == nil {
		t.Fatal("key file overwritten")
	}
	if err := run("keys", "assign", "--id", k.ID, "--profile", "XS"); err != nil {
		t.Fatal(err)
	}
	if err := run("quota", "--key", k.ID); err != nil {
		t.Fatal(err)
	}
	if err := run("keys", "revoke", "--id", k.ID); err != nil {
		t.Fatal(err)
	}
	if err := run("keys", "list"); err != nil || !strings.Contains(out.String(), `"revoked": true`) {
		t.Fatal(out.String(), err)
	}
}

func TestGuidedDeletionAcceptsPipedRemoteInput(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "192.0.2.1 12345 192.0.2.2 22")
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = read
	t.Cleanup(func() {
		os.Stdin = originalStdin
		_ = read.Close()
	})
	if _, err = write.WriteString("1\n1\nCANCELLA\n"); err != nil {
		t.Fatal(err)
	}
	if err = write.Close(); err != nil {
		t.Fatal(err)
	}
	backup := model.Backup{Receipt: model.Receipt{ID: "00000000000000000000000000000001", Status: model.BackupComplete, Size: 100, ReceivedAt: "2026-01-01T00:00:00Z"}, KeyID: "key"}
	called := false
	call := func(method, path string, request, response any) error {
		switch path {
		case "/v1/inventory":
			inventory := response.(*adminInventory)
			inventory.Status.Timezone = "UTC"
			inventory.Keys = []model.Key{{ID: "key", Name: "remote"}}
			inventory.Backups = []model.Backup{backup, {Receipt: model.Receipt{ID: "00000000000000000000000000000002", Status: model.BackupComplete, Size: 100, ReceivedAt: "2026-01-02T00:00:00Z"}, KeyID: "key"}}
		case "/v1/retention/request":
			called = method == "POST" && request.(map[string]string)["backup_id"] == backup.ID
			*response.(*model.RetentionOperation) = model.RetentionOperation{ID: 1, BackupID: backup.ID, Kind: "quarantine"}
		default:
			t.Fatalf("chiamata inattesa: %s %s", method, path)
		}
		return nil
	}
	var out bytes.Buffer
	if err = guidedDeletion(call, &out); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("la richiesta da pipe in sessione SSH non e stata inoltrata")
	}
}
