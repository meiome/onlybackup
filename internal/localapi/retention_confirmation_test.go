package localapi

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/meiome/onlybackup/internal/model"
)

func TestRetentionMutationsRequireActionSpecificConfirmation(t *testing.T) {
	api, s := apiFixture(t)
	for _, endpoint := range []string{"/v1/automation/enable", "/v1/retention/pause", "/v1/retention/resume", "/v1/retention/resume-when-ready", "/v1/automation/setup"} {
		for _, confirmation := range []string{"", "WRONG ACTION"} {
			body := map[string]any{"confirmation": confirmation}
			if endpoint == "/v1/automation/setup" {
				body["host"], body["port"], body["from"], body["recipients"], body["timezone"] = "smtp.test", 25, "backup@test", "admin@test", "UTC"
			}
			before, err := s.AutomationStatus()
			if err != nil {
				t.Fatal(err)
			}
			response := request(api.AdminHandler(), "POST", endpoint, body, true)
			if response.Code != http.StatusConflict {
				t.Fatalf("%s accepted missing/wrong consent: %d %s", endpoint, response.Code, response.Body.String())
			}
			after, err := s.AutomationStatus()
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("unconfirmed %s changed state: before=%+v after=%+v %v", endpoint, before, after, err)
			}
		}
	}
	// A malformed pause request must not silently become a default pause.
	if response := request(api.AdminHandler(), "POST", "/v1/retention/pause", map[string]any{"confirmation": model.ConfirmRetentionPause, "reason": 42}, true); response.Code != http.StatusBadRequest {
		t.Fatalf("malformed pause accepted: %d", response.Code)
	}
	if status, err := s.AutomationStatus(); err != nil || status.ManualPaused {
		t.Fatalf("malformed request paused retention: %+v %v", status, err)
	}
}

func TestConfirmedSetupPreservesModeUnlessExplicitlyChanged(t *testing.T) {
	api, s := apiFixture(t)
	body := map[string]any{"host": "smtp.test", "port": 25, "from": "backup@test", "recipients": "admin@test", "timezone": "UTC", "retention_mode": "manual", "confirmation": "CONFIGURA MANUAL"}
	post := func(endpoint string, body any) {
		t.Helper()
		if response := request(api.AdminHandler(), "POST", endpoint, body, true); response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", endpoint, response.Code, response.Body.String())
		}
	}
	post("/v1/automation/setup", body)
	delete(body, "retention_mode")
	body["confirmation"] = "CONFIGURA"
	body["host"] = "replacement.smtp.test"
	post("/v1/automation/setup", body)
	if status, err := s.AutomationStatus(); err != nil || status.Enabled || status.ActivationPending {
		t.Fatalf("SMTP setup overrode manual choice: %+v %v", status, err)
	}
	body["retention_mode"] = "auto"
	if response := request(api.AdminHandler(), "POST", "/v1/automation/setup", body, true); response.Code != http.StatusConflict {
		t.Fatalf("mode changed with generic setup consent: %d", response.Code)
	}
	body["confirmation"] = "CONFIGURA AUTO"
	post("/v1/automation/setup", body)
	if err := s.MarkMailTested(api.Now()); err != nil {
		t.Fatal(err)
	}
	post("/v1/automation/enable", map[string]string{"confirmation": "ABILITA"})
	if err := s.SetCheckState(model.MonitoringRegular, 7, api.Now()); err != nil {
		t.Fatal(err)
	}
	post("/v1/retention/resume", map[string]string{"confirmation": "RIPRENDI"})
	post("/v1/retention/pause", map[string]string{"confirmation": "SOSPENDI", "reason": "confirmed hold"})
	if status, err := s.AutomationStatus(); err != nil || !status.ManualPaused || !status.DeletionBlocked {
		t.Fatalf("confirmed pause failed: %+v %v", status, err)
	}
	post("/v1/retention/resume-when-ready", map[string]string{"confirmation": "PRENOTA"})
	if status, err := s.AutomationStatus(); err != nil || !status.ResumePending || !status.DeletionBlocked || !status.ManualPaused {
		t.Fatalf("confirmed reservation released the pause immediately: %+v %v", status, err)
	}
}
