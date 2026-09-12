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
