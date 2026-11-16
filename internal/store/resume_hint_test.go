package store

import (
	"strings"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

// A resolved blocking incident must leave an explicit resume instruction in the
// resolution mail, otherwise the suspension is silent for the administrator.
func TestResolvedBlockingIncidentMailCarriesResumeHint(t *testing.T) {
	s, keyID, _ := setup(t, model.Profile{Name: "hint", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	enableRetentionForTest(t, s, now)
	const key = "capacity:retention-window"
	if created, err := s.OpenAnomaly(key, "capacity", keyID, "", "finestra da verificare", now); err != nil || !created {
		t.Fatalf("incident not recorded: %v %v", created, err)
	}
	if resolved, err := s.ResolveAnomaly(key, now.Add(time.Minute)); err != nil || !resolved {
		t.Fatalf("incident not resolved: %v %v", resolved, err)
	}
	status, err := s.AutomationStatus()
	if err != nil || !status.DeletionBlocked || status.MonitoringState != model.MonitoringPaused {
		t.Fatalf("resolution must leave deletion suspended: %+v %v", status, err)
	}
	mail, err := s.DueMail(now.Add(time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range mail {
		if message.Kind == "resolution" && strings.Contains(message.Subject, "anomalia risolta") {
			found = true
			if !strings.Contains(message.Body, model.ResumeHint) {
				t.Fatalf("resolution mail lacks resume hint: %q", message.Body)
			}
			if !strings.Contains(message.Body, key) {
				t.Fatalf("resolution mail should keep the incident key for audit: %q", message.Body)
			}
		}
	}
	if !found {
		t.Fatal("resolution mail missing")
	}
}

// The quarantine margin warning is non-blocking: its resolution must not tell
// the administrator to resume anything.
func TestQuarantineMarginResolutionMailHasNoResumeHint(t *testing.T) {
	s, keyID, _ := setup(t, model.Profile{Name: "margin-hint", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	enableRetentionForTest(t, s, now)
	if _, err := s.OpenAnomaly(policy.QuarantineMarginAnomalyKey, "capacity", keyID, "", "margine", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAnomaly(policy.QuarantineMarginAnomalyKey, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	mail, err := s.DueMail(now.Add(time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range mail {
		if message.Kind == "resolution" && strings.Contains(message.Body, model.ResumeHint) {
			t.Fatalf("non-blocking warning asked for a resume: %q", message.Body)
		}
	}
}

// If another blocking incident is still open, resume would be refused, so the
// resolution mail must not ask for it.
func TestResolutionMailWithAnotherBlockingIncidentHasNoResumeHint(t *testing.T) {
	s, keyID, _ := setup(t, model.Profile{Name: "two-holds", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	enableRetentionForTest(t, s, now)
	const key = "capacity:retention-window"
	if _, err := s.OpenAnomaly(key, "capacity", keyID, "", "finestra", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAnomaly("file-missing:still-open", "file_missing", "", "", "file mancante", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAnomaly(key, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	mail, err := s.DueMail(now.Add(time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range mail {
		if message.Kind == "resolution" && strings.Contains(message.Body, model.ResumeHint) {
			t.Fatalf("resume requested while another blocking incident is open: %q", message.Body)
		}
	}
}
