package maintenance

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/localclient"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
	"github.com/meiome/onlybackup/internal/store"
)

func TestReportResumeAdviceRequiresReadyCompletedCheck(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	filesystem := policy.Filesystem{Blocks: 1000, Bfree: 990, Bavail: 990, BlockSize: 1}
	for _, test := range []struct {
		name   string
		change func(*localclient.Snapshot, *[]policy.Finding)
		want   bool
	}{
		{name: "regular check with manual pause", want: true},
		{name: "learning", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Status.MonitoringState = model.MonitoringLearning
		}},
		{name: "pause still requires a regular check", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Status.LastCheckRegular = false
		}},
		{name: "completed check too old", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Status.LastCheckAt = now.Add(-10*time.Minute - time.Second).Unix()
		}},
		{name: "active anomaly", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Status.ActiveAnomalies = 1
		}},
		{name: "quarantine margin warning", want: true, change: func(s *localclient.Snapshot, findings *[]policy.Finding) {
			s.Status.ActiveAnomalies = 1
			s.Anomalies = []model.Anomaly{{Kind: "capacity", StableKey: policy.QuarantineMarginAnomalyKey}}
			*findings = []policy.Finding{{Kind: "capacity", StableKey: policy.QuarantineMarginAnomalyKey}}
		}},
		{name: "margin warning with another capacity incident", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Status.ActiveAnomalies = 2
			s.Anomalies = []model.Anomaly{{Kind: "capacity", StableKey: policy.QuarantineMarginAnomalyKey}, {Kind: "capacity", StableKey: "capacity:retention-window"}}
		}},
		{name: "margin warning with new integrity finding", change: func(s *localclient.Snapshot, findings *[]policy.Finding) {
			s.Status.ActiveAnomalies = 1
			s.Anomalies = []model.Anomaly{{Kind: "capacity", StableKey: policy.QuarantineMarginAnomalyKey}}
			*findings = []policy.Finding{{Kind: "catalog", StableKey: "catalog-file:one"}}
		}},
		{name: "margin key with wrong kind", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Status.ActiveAnomalies = 1
			s.Anomalies = []model.Anomaly{{Kind: "catalog", StableKey: policy.QuarantineMarginAnomalyKey}}
		}},
		{name: "new findings", change: func(_ *localclient.Snapshot, findings *[]policy.Finding) {
			*findings = []policy.Finding{{Kind: "capacity"}}
		}},
		{name: "uncertain authorized operation", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Operations = []model.RetentionOperation{{State: "authorized"}}
		}},
		{name: "pending request is not an uncertain authorization", want: true, change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Operations = []model.RetentionOperation{{State: "requested"}}
		}},
		{name: "automation disabled", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Status.Enabled = false
		}},
		{name: "initial activation pending", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Status.ActivationPending = true
		}},
		{name: "deferred resume already requested", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Status.ResumePending = true
		}},
		{name: "already resumed", change: func(s *localclient.Snapshot, _ *[]policy.Finding) {
			s.Status.MonitoringState = model.MonitoringRegular
			s.Status.DeletionBlocked = false
			s.Status.ManualPaused = false
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := localclient.Snapshot{
				Status: model.AutomationStatus{Enabled: true, MailTested: true, MonitoringState: model.MonitoringPaused,
					DeletionBlocked: true, ManualPaused: true, ModelRevision: 7, LastCheckAt: now.Unix(), LastCheckRegular: true, Timezone: "UTC"},
				Keys: []model.Key{{ID: "one", Name: "Windows"}},
				Backups: []model.Backup{{KeyID: "one", Receipt: model.Receipt{Status: model.BackupComplete,
					ReceivedAt: now.Format(time.RFC3339), Size: 100}}},
			}
			var findings []policy.Finding
			if test.change != nil {
				test.change(&snapshot, &findings)
			}
			report := buildEmailReport(snapshot, filesystem, findings, now)
			if got := strings.Contains(report, "onlybackup-admin retention resume per riattivarla"); got != test.want {
				t.Fatalf("resume advice=%v, want %v: %s", got, test.want, report)
			}
			if test.want && (!strings.Contains(report, "controllo regolare. Cancellazione sospesa:") ||
				!strings.Contains(renderMailHTML(model.MailMessage{Subject: "OnlyBackup: report periodico", Body: report}), "ATTENZIONE")) {
				t.Fatalf("advice or report badge missing: %s", report)
			}
		})
	}
}

func TestReportKeepsQuarantineMarginWarningVisibleWhileRetentionRuns(t *testing.T) {
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	snapshot := localclient.Snapshot{
		Status: model.AutomationStatus{Enabled: true, MailTested: true, MonitoringState: model.MonitoringRegular,
			LastCheckRegular: true, ActiveAnomalies: 1, Timezone: "UTC"},
		Keys: []model.Key{{ID: "one", Name: "Windows"}},
		Backups: []model.Backup{{KeyID: "one", Receipt: model.Receipt{Status: model.BackupComplete,
			ReceivedAt: now.Format(time.RFC3339), Size: 100}}},
		Anomalies: []model.Anomaly{{Kind: "capacity", StableKey: policy.QuarantineMarginAnomalyKey}},
	}
	filesystem := policy.Filesystem{Blocks: 1000, Bfree: 100, Bavail: 100, BlockSize: 1}
	report := buildEmailReport(snapshot, filesystem, nil, now)
	for _, want := range []string{"Esito: ATTENZIONE", "Anomalie attive: 1", "Retention: attiva", "la retention continua"} {
		if !strings.Contains(report, want) {
			t.Fatalf("warning or retention status missing %q: %s", want, report)
		}
	}
}

func TestPeriodicReportSuggestsResumeOnlyAfterLongRegularCheck(t *testing.T) {
	for _, staleAnomaly := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale_anomaly=%v", staleAnomaly), func(t *testing.T) {
			now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
			db, service, key, token := auditService(t, &now)
			settings := store.MailSettings{Host: "smtp.invalid", Port: 25, From: "backup@test", Recipients: "admin@test"}
			if err := db.SetupAutomation(settings, "UTC", now.Add(-72*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := db.MarkMailTested(now); err != nil {
				t.Fatal(err)
			}
			if err := db.EnableAutomation(); err != nil {
				t.Fatal(err)
			}
			const pauseReason = "blocco precedente alla migrazione: apprendimento non completato"
			if err := db.PauseRetention(pauseReason); err != nil {
				t.Fatal(err)
			}
			data := make([]byte, 100)
			digest := fmt.Sprintf("%x", sha256.Sum256(data))
			for i := 14; i >= 1; i-- {
				at := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, -i)
				b, err := db.ReserveIdempotent(token, model.Metadata{Description: "report test", OriginalName: "db.bin"},
					int64(len(data)), digest, fmt.Sprintf("report-resume-backup-%02d", i), at)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(service.Root, "backups", b.ID+".backup"), data, 0440); err != nil {
					t.Fatal(err)
				}
				if err = db.Complete(b.ID, at); err != nil {
					t.Fatal(err)
				}
			}
			if staleAnomaly {
				if _, err := db.OpenAnomaly("model:"+key.ID, "model", key.ID, "", "vecchio modello non affidabile", now); err != nil {
					t.Fatal(err)
				}
			}
			// A large production archive may take much longer than the ten-minute
			// freshness limit to scan. The advice uses the check's completion time.
			service.Filesystem = func(string) (policy.Filesystem, error) {
				now = now.Add(45 * time.Minute)
				return policy.Filesystem{Blocks: 10000, Bfree: 8600, Bavail: 8600, BlockSize: 1}, nil
			}
			if err := service.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			status, err := db.AutomationStatus()
			if err != nil {
				t.Fatal(err)
			}
			if status.MonitoringState != model.MonitoringPaused || !status.ManualPaused || !status.DeletionBlocked ||
				status.ManualPauseReason != pauseReason || status.ActiveAnomalies != 0 {
				t.Fatalf("report changed pause or check was not regular: %+v", status)
			}
			operations, err := db.SnapshotOperations()
			if err != nil || len(operations) != 0 {
				t.Fatalf("report created retention operations: %+v, %v", operations, err)
			}
			mail, err := db.DueMail(now, 100)
			if err != nil {
				t.Fatal(err)
			}
			var reports []model.MailMessage
			for _, message := range mail {
				if message.Subject == "OnlyBackup: report periodico" {
					reports = append(reports, message)
				}
			}
			if len(reports) != 1 || !strings.Contains(reports[0].Body, "Generato: 05/10/2026 09:45 UTC") {
				t.Fatalf("periodic report missing or completion time wrong: %+v", reports)
			}
			if got := strings.Contains(reports[0].Body, "onlybackup-admin retention resume per riattivarla"); got == staleAnomaly {
				t.Fatalf("resume advice=%v with stale anomaly=%v: %s", got, staleAnomaly, reports[0].Body)
			}
			// Check the advice against the writer's actual permission, only in this
			// temporary test database. Generating the report itself kept the pause.
			err = db.ResumeRetention(now)
			if staleAnomaly && err == nil {
				t.Fatal("resolved anomaly should still require another regular check")
			}
			if !staleAnomaly && err != nil {
				t.Fatalf("report suggested an unavailable resume: %v", err)
			}
		})
	}
}

func TestReportDoesNotSuggestResumeAfterCancellationFollowingCheck(t *testing.T) {
	f := newPermissionFixture(t)
	backupID := f.backups[0].ID
	if _, err := f.db.RequestQuarantine(backupID, "manual", "admin", "test cancellation", f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.db.PauseRetention("test manual pause"); err != nil {
		t.Fatal(err)
	}
	service := f.service(t, nil)
	filesystem, err := service.Filesystem(service.Root)
	if err != nil {
		t.Fatal(err)
	}
	completeCheck := func() {
		t.Helper()
		check, err := f.db.StartMonitoringCheck(f.now)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.db.CompleteMonitoringCheck(check.ID, "completed_clean", "nessuna anomalia", model.MonitoringRegular, 7, nil, f.now); err != nil {
			t.Fatal(err)
		}
	}
	report := func() string {
		t.Helper()
		var snapshot localclient.Snapshot
		if err := service.call(context.Background(), "GET", "/v1/snapshot", nil, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Status.MonitoringState != model.MonitoringPaused || !snapshot.Status.ManualPaused || snapshot.Status.ActiveAnomalies != 0 {
			t.Fatalf("unexpected paused status: %+v", snapshot.Status)
		}
		return buildEmailReport(snapshot, filesystem, nil, f.now)
	}
	completeCheck()
	if body := report(); !strings.Contains(body, "onlybackup-admin retention resume per riattivarla") {
		t.Fatalf("regular manual pause did not suggest resume: %s", body)
	}
	// Inject cancellation precisely after check completion and before the
	// final snapshot. Revision and last_check_at stay unchanged, but the
	// writer invalidates the regular state until another check completes.
	if err := f.db.CancelQuarantineRequest(backupID, "admin", f.now); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ResumeRetention(f.now); err == nil {
		t.Fatal("cancelled request should require another regular check")
	}
	if body := report(); strings.Contains(body, "onlybackup-admin retention resume per riattivarla") {
		t.Fatalf("report suggests resume rejected by the writer: %s", body)
	}
	stored, err := f.db.Backup(backupID)
	if err != nil || stored.Status != model.BackupComplete {
		t.Fatalf("cancelled backup was not preserved: %+v, %v", stored, err)
	}
	f.now = f.now.Add(time.Minute)
	completeCheck()
	if body := report(); !strings.Contains(body, "onlybackup-admin retention resume per riattivarla") {
		t.Fatalf("new regular check did not restore advice: %s", body)
	}
	if err := f.db.ResumeRetention(f.now); err != nil {
		t.Fatalf("report suggested unavailable resume after new check: %v", err)
	}
}
