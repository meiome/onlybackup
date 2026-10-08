package store

import (
	"strings"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

func TestOnlyQuarantineMarginCapacityWarningIsNonBlocking(t *testing.T) {
	for _, test := range []struct {
		name, stableKey, kind string
		blocks                bool
	}{
		{"quarantine margin", policy.QuarantineMarginAnomalyKey, "capacity", false},
		{"retention minima", "capacity:retention-window", "capacity", true},
		{"quota", "quota:key", "capacity", true},
		{"unknown capacity", "capacity:other", "capacity", true},
		{"similar key", policy.QuarantineMarginAnomalyKey + ":other", "capacity", true},
		{"integrity with same key", policy.QuarantineMarginAnomalyKey, "catalog", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, keyID, _ := setup(t, model.Profile{Name: "margin", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
			now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
			enableRetentionForTest(t, s, now)
			created, err := s.OpenAnomaly(test.stableKey, test.kind, keyID, "", "spazio da verificare", now)
			if err != nil || !created {
				t.Fatalf("incident not recorded: %v %v", created, err)
			}
			status, err := s.AutomationStatus()
			if err != nil || status.DeletionBlocked != test.blocks || status.ActiveAnomalies != 1 {
				t.Fatalf("wrong immediate block: %+v %v", status, err)
			}
			check, err := s.StartMonitoringCheck(now)
			if err != nil {
				t.Fatal(err)
			}
			finding := policy.Finding{StableKey: test.stableKey, Kind: test.kind, KeyID: keyID, Detail: "spazio da verificare"}
			if err = s.CompleteMonitoringCheck(check.ID, "completed_with_anomalies", "1 anomalia", model.MonitoringRegular, 7, []policy.Finding{finding}, now); err != nil {
				t.Fatal(err)
			}
			status, err = s.AutomationStatus()
			if err != nil || status.LastCheckRegular == test.blocks {
				t.Fatalf("wrong completed-check classification: %+v %v", status, err)
			}
			if err = s.ResumeRetention(now); (err != nil) != test.blocks {
				t.Fatalf("resume with blocking=%v: %v", test.blocks, err)
			}
			mail, err := s.DueMail(now, 20)
			if err != nil || len(mail) != 1 {
				t.Fatalf("warning mail missing or duplicated: %+v %v", mail, err)
			}
			if got := strings.Contains(mail[0].Subject, "AVVISO NON BLOCCANTE"); got == test.blocks {
				t.Fatalf("misleading warning subject: %s", mail[0].Subject)
			}
		})
	}
}

func TestExistingQuarantineMarginIncidentRequiresExplicitResume(t *testing.T) {
	s, _, _ := setup(t, model.Profile{Name: "legacy-margin", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	enableRetentionForTest(t, s, now)
	opened := now.Add(-24 * time.Hour)
	if _, err := s.OpenAnomaly(policy.QuarantineMarginAnomalyKey, "capacity", "", "", "margine insufficiente", opened); err != nil {
		t.Fatal(err)
	}
	// Reproduce the persisted block and administrative hold of the old release.
	if _, err := s.db.Exec(`UPDATE automation_state SET deletion_blocked=1,monitoring_state='ANOMALIA',block_reason='anomalia: margine insufficiente'`); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseRetention("pausa precedente all'aggiornamento"); err != nil {
		t.Fatal(err)
	}
	check, err := s.StartMonitoringCheck(now)
	if err != nil {
		t.Fatal(err)
	}
	finding := policy.Finding{StableKey: policy.QuarantineMarginAnomalyKey, Kind: "capacity", Detail: "margine insufficiente"}
	if err = s.CompleteMonitoringCheck(check.ID, "completed_with_anomalies", "1 avviso", model.MonitoringRegular, 7, []policy.Finding{finding}, now); err != nil {
		t.Fatal(err)
	}
	status, err := s.AutomationStatus()
	if err != nil || !status.LastCheckRegular || !status.DeletionBlocked || !status.ManualPaused {
		t.Fatalf("old holds cleared automatically: %+v %v", status, err)
	}
	if err = s.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}
	status, err = s.AutomationStatus()
	if err != nil || status.DeletionBlocked || status.ManualPaused || status.ActiveAnomalies != 1 {
		t.Fatalf("resume changed the incident or kept the old hold: %+v %v", status, err)
	}
	incidents, err := s.Anomalies(true)
	if err != nil || len(incidents) != 1 || incidents[0].OpenedAt != opened.Unix() || incidents[0].EventAt != opened.Unix() || incidents[0].AcknowledgedAt != 0 {
		t.Fatalf("existing incident history rewritten: %+v %v", incidents, err)
	}
	var version int
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 12 {
		t.Fatalf("schema changed: %d %v", version, err)
	}
}

func TestResolvingQuarantineMarginWarningPreservesOtherHolds(t *testing.T) {
	for _, hold := range []string{"none", "manual", "integrity"} {
		t.Run(hold, func(t *testing.T) {
			s, _, _ := setup(t, model.Profile{Name: "resolve-margin", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
			now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
			enableRetentionForTest(t, s, now)
			if _, err := s.OpenAnomaly(policy.QuarantineMarginAnomalyKey, "capacity", "", "", "margine insufficiente", now); err != nil {
				t.Fatal(err)
			}
			switch hold {
			case "manual":
				if err := s.PauseRetention("manutenzione amministrativa"); err != nil {
					t.Fatal(err)
				}
			case "integrity":
				if _, err := s.OpenAnomaly("file-missing:one", "file_missing", "", "", "file mancante", now); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.AutomationStatus()
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := s.ResolveAnomaly(policy.QuarantineMarginAnomalyKey, now.Add(time.Minute))
			if err != nil || !resolved {
				t.Fatalf("warning not resolved: %v %v", resolved, err)
			}
			after, err := s.AutomationStatus()
			if err != nil || after.DeletionBlocked != before.DeletionBlocked || after.ManualPaused != before.ManualPaused || after.BlockReason != before.BlockReason || after.MonitoringState != before.MonitoringState {
				t.Fatalf("warning resolution changed another hold: before=%+v after=%+v %v", before, after, err)
			}
			if hold != "none" && !after.DeletionBlocked {
				t.Fatal("other hold was not preserved")
			}
			mail, err := s.DueMail(now.Add(time.Minute), 20)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, message := range mail {
				found = found || message.Kind == "resolution"
			}
			if !found {
				t.Fatal("resolution mail missing")
			}
		})
	}
}
