package localcli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestRetentionChangesRequireCLIConfirmation(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "admin.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	type mutation struct {
		path string
		body map[string]any
	}
	requests := make(chan mutation, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OnlyBackup-Protocol", "1")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests <- mutation{path: r.URL.Path, body: body}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	for _, test := range []struct {
		name, input, path, confirmation string
		args                            []string
	}{
		{name: "EOF rejects pause", args: []string{"retention", "pause"}},
		{name: "wrong answer rejects pause", args: []string{"retention", "pause"}, input: "yes\n"},
		{name: "piped pause confirmation", args: []string{"retention", "pause"}, input: "SOSPENDI\n", path: "/v1/retention/pause", confirmation: "SOSPENDI"},
		{name: "explicit enable flag", args: []string{"retention", "enable", "--confirm", "ABILITA"}, path: "/v1/automation/enable", confirmation: "ABILITA"},
		{name: "enable rejects pause flag", args: []string{"retention", "enable", "--confirm", "SOSPENDI"}},
		{name: "piped immediate resume", args: []string{"retention", "resume"}, input: "RIPRENDI\n", path: "/v1/retention/resume", confirmation: "RIPRENDI"},
		{name: "reservation rejects immediate consent", args: []string{"retention", "resume", "--when-ready", "--confirm", "RIPRENDI"}},
		{name: "explicit reservation", args: []string{"retention", "resume", "--when-ready", "--confirm", "PRENOTA"}, path: "/v1/retention/resume-when-ready", confirmation: "PRENOTA"},
		{name: "setup omission preserves mode", args: []string{"automation", "setup", "--confirm", "CONFIGURA"}, path: "/v1/automation/setup", confirmation: "CONFIGURA"},
		{name: "setup mode requires matching consent", args: []string{"automation", "setup", "--retention", "auto", "--confirm", "CONFIGURA"}},
		{name: "explicit setup mode", args: []string{"automation", "setup", "--retention", "manual", "--confirm", "CONFIGURA MANUAL"}, path: "/v1/automation/setup", confirmation: "CONFIGURA MANUAL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			if _, err = write.WriteString(test.input); err != nil {
				t.Fatal(err)
			}
			write.Close()
			original := os.Stdin
			os.Stdin = read
			defer func() { os.Stdin = original }()
			var out, errs bytes.Buffer
			err = Admin(append([]string{"--admin-socket", sock}, test.args...), &out, &errs)
			if (err == nil) != (test.path != "") {
				t.Fatalf("confirmation result: %v output=%s", err, out.String())
			}
			select {
			case got := <-requests:
				if test.path == "" || got.path != test.path || got.body["confirmation"] != test.confirmation {
					t.Fatalf("unexpected mutation: %+v", got)
				}
				if test.name == "setup omission preserves mode" && got.body["retention_mode"] != "" {
					t.Fatalf("CLI silently selected a mode: %+v", got.body)
				}
			default:
				if test.path != "" {
					t.Fatal("confirmed mutation was not sent")
				}
			}
		})
	}
}
