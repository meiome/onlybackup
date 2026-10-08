package localapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMailConfirmationHeaderCannotBypassConfigurationVersion(t *testing.T) {
	for _, test := range []struct {
		name, stableID string
		version        int64
		wantProof      bool
	}{
		{name: "missing version", stableID: "mail-test:missing"},
		{name: "obsolete version", stableID: "mail-test:obsolete", version: 2},
		{name: "ordinary report", stableID: "report:ordinary", version: 1},
		{name: "current proof", stableID: "mail-test:current", version: 1, wantProof: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, s := apiFixture(t)
			if err := s.QueueReport(test.stableID, "test", "test", api.Now()); err != nil {
				t.Fatal(err)
			}
			messages, err := s.DueMail(api.Now(), 10)
			if err != nil || len(messages) != 1 {
				t.Fatalf("mail queue: %+v %v", messages, err)
			}
			body := map[string]any{"id": messages[0].ID, "error": ""}
			if test.version != 0 {
				body["config_version"] = test.version
			}
			var data bytes.Buffer
			if err := json.NewEncoder(&data).Encode(body); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/mail/confirm", &data)
			r.Header.Set(protocolHeader, ProtocolVersion)
			r.Header.Set("X-OnlyBackup-Mail-Kind", "test")
			w := httptest.NewRecorder()
			api.MaintenanceHandler().ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("confirmation failed: %d %s", w.Code, w.Body.String())
			}
			if status, err := s.AutomationStatus(); err != nil || status.MailTested != test.wantProof {
				t.Fatalf("unexpected SMTP proof: %+v %v", status, err)
			}
		})
	}
}
