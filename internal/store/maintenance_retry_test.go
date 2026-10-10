package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

type provisionalRetryFixture struct {
	store      *Store
	key, token string
	op, other  model.RetentionOperation
	lease      model.MaintenanceLease
	now        time.Time
}

func newProvisionalRetryFixture(t *testing.T, kind string) *provisionalRetryFixture {
	t.Helper()
	s, key, token := setup(t, model.Profile{Name: "provisional-retry", TotalBytes: 2000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	old := completedBackup(t, s, token, "retry-old-1234567890", now.AddDate(0, 0, -14))
	otherBackup := completedBackup(t, s, token, "retry-other-1234567890", now.AddDate(0, 0, -13))
	completedBackup(t, s, token, "retry-new-1234567890", now.AddDate(0, 0, -1))
	enableRetentionForTest(t, s, now)
	lease, err := s.AcquireMaintenanceLease("retry-worker", 0, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.RequestQuarantine(old.ID, "manual", "admin", "retry fixture", now)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "purge" {
		op, err = s.AuthorizeOperation(op.ID, lease, now)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.ValidateOperation(op.ID, op.Ticket, lease, now); err != nil {
			t.Fatal(err)
		}
		if err = s.ConfirmOperation(op.ID, op.Ticket, true, "", lease, now); err != nil {
			t.Fatal(err)
		}
		if err = s.ReleaseMaintenanceLease(lease); err != nil {
			t.Fatal(err)
		}
		now = now.Add(72 * time.Hour)
		lease, err = s.AcquireMaintenanceLease("retry-worker", 0, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
			t.Fatal(err)
		}
		op, err = s.RequestPurge(old.ID, "automatic", "worker", "retry fixture", now)
		if err != nil {
			t.Fatal(err)
		}
	}
	other, err := s.RequestQuarantine(otherBackup.ID, "manual", "admin", "independent work", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveModel(key, 7, "UTC", 7, map[string]bool{"reliable": true}, true, false, now); err != nil {
		t.Fatal(err)
	}
	op, err = s.AuthorizeOperation(op.ID, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmOperation(op.ID, op.Ticket, false, "temporary source verification failure", lease, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ResetAuthorized(op.ID, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err = s.SetCheckState(model.MonitoringAnomaly, 7, now); err != nil {
		t.Fatal(err)
	}
	return &provisionalRetryFixture{s, key, token, op, other, lease, now}
}

func TestProvisionalRetryKeepsOwnErrorAndOtherWorkBlocked(t *testing.T) {
	for _, kind := range []string{"quarantine", "purge"} {
		t.Run(kind, func(t *testing.T) {
			f := newProvisionalRetryFixture(t, kind)
			s := f.store
			op, err := s.AuthorizeOperation(f.op.ID, f.lease, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if op.ExecutionCommitted {
				t.Fatal("ticket issuance committed final permission")
			}
			if _, err = s.AuthorizeOperation(f.other.ID, f.lease, f.now); !errors.Is(err, model.ErrRetentionBlocked) {
				t.Fatalf("unrelated operation escaped hold: %v", err)
			}
			// Slow full-file verification may outlast the ten-minute freshness window.
			f.now = f.now.Add(11 * time.Minute)
			if err = s.ValidateOperation(op.ID, op.Ticket, f.lease, f.now); err != nil {
				t.Fatalf("long hash invalidated freshness established at ticket issuance: %v", err)
			}
			status, err := s.AutomationStatus()
			if err != nil || !status.DeletionBlocked {
				t.Fatal("retry released global hold", err)
			}
			active, err := s.Anomalies(true)
			if err != nil || len(active) != 1 {
				t.Fatalf("own error resolved prematurely: %+v %v", active, err)
			}
			messages, err := s.DueMail(f.now, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range messages {
				if m.Kind == "resolution" {
					t.Fatal("retry sent false resolution")
				}
			}
			if err = s.ConfirmOperation(op.ID, op.Ticket, true, "", f.lease, f.now); err != nil {
				t.Fatal(err)
			}
			active, err = s.Anomalies(true)
			if err != nil || len(active) != 0 {
				t.Fatal("completion did not close own error", err)
			}
		})
	}
}

func TestProvisionalRetryRechecksIndependentHoldsAtBothBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *provisionalRetryFixture)
	}{
		{"manual-pause", func(t *testing.T, f *provisionalRetryFixture) {
			if err := f.store.PauseRetention("admin pause"); err != nil {
				t.Fatal(err)
			}
		}},
		{"independent-anomaly", func(t *testing.T, f *provisionalRetryFixture) {
			if _, err := f.store.OpenAnomaly("other-file", "file_missing", f.key, "", "missing copy", f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{"resolved-independent-hold", func(t *testing.T, f *provisionalRetryFixture) {
			if _, err := f.store.OpenAnomaly("other-file", "file_missing", f.key, "", "missing copy", f.now); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.ResolveAnomaly("other-file", f.now); err != nil {
				t.Fatal(err)
			}
			if err := f.store.SetCheckState(model.MonitoringAnomaly, 7, f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{"deferred-resume", func(t *testing.T, f *provisionalRetryFixture) {
			if err := f.store.RequestRetentionResume(f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{"mail-proof-lost", func(t *testing.T, f *provisionalRetryFixture) {
			if _, err := f.store.db.Exec("UPDATE automation_state SET mail_tested=0 WHERE id=1"); err != nil {
				t.Fatal(err)
			}
		}},
		{"activation-required", func(t *testing.T, f *provisionalRetryFixture) {
			if _, err := f.store.db.Exec("UPDATE automation_state SET activation_required=1 WHERE id=1"); err != nil {
				t.Fatal(err)
			}
		}},
		{"disabled-automation", func(t *testing.T, f *provisionalRetryFixture) {
			if _, err := f.store.db.Exec("UPDATE automation_state SET enabled=0 WHERE id=1"); err != nil {
				t.Fatal(err)
			}
		}},
		{"other-uncertain-operation", func(t *testing.T, f *provisionalRetryFixture) {
			if _, err := f.store.db.Exec("UPDATE retention_operations SET state='authorized' WHERE id=?", f.other.ID); err != nil {
				t.Fatal(err)
			}
		}},

		{"learning-key", func(t *testing.T, f *provisionalRetryFixture) {
			if _, _, err := f.store.CreateKey("new-unlearned-key", "provisional-retry"); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreliable-model", func(t *testing.T, f *provisionalRetryFixture) {
			if err := f.store.SaveModel(f.key, 7, "UTC", 7, nil, false, false, f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{"new-revision", func(t *testing.T, f *provisionalRetryFixture) {
			if err := f.store.SetCheckState(model.MonitoringAnomaly, 8, f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{"raised-minimum", func(t *testing.T, f *provisionalRetryFixture) {
			if err := f.store.SetKeyRetentionDays(f.key, 30); err != nil {
				t.Fatal(err)
			}
		}},
		{"revoked-key", func(t *testing.T, f *provisionalRetryFixture) {
			if err := f.store.Revoke(f.key); err != nil {
				t.Fatal(err)
			}
		}},
		{"upload-active", func(t *testing.T, f *provisionalRetryFixture) {
			if _, err := reserve(f.store, f.token, 100, f.now); err != nil {
				t.Fatal(err)
			}
		}},
		{"expired-lease", func(t *testing.T, f *provisionalRetryFixture) { f.now = f.now.Add(2 * time.Hour) }},
	}
	for _, boundary := range []string{"authorize", "validate"} {
		for _, tc := range cases {
			t.Run(boundary+"/"+tc.name, func(t *testing.T) {
				f := newProvisionalRetryFixture(t, "quarantine")
				op := f.op
				var err error
				if boundary == "validate" {
					op, err = f.store.AuthorizeOperation(op.ID, f.lease, f.now)
					if err != nil {
						t.Fatal(err)
					}
				}
				tc.change(t, f)
				if boundary == "authorize" {
					_, err = f.store.AuthorizeOperation(op.ID, f.lease, f.now)
				} else {
					err = f.store.ValidateOperation(op.ID, op.Ticket, f.lease, f.now)
				}
				if err == nil {
					t.Fatal("independent safety check bypassed")
				}
				current, queryErr := f.store.Operation(op.ID)
				if queryErr != nil || current.ExecutionCommitted {
					t.Fatalf("denied retry committed permission: %+v %v", current, queryErr)
				}
			})
		}
	}
}

func TestTransportErrorCannotForgeReconciliationMarker(t *testing.T) {
	f := newProvisionalRetryFixture(t, "quarantine")
	op, err := f.store.AuthorizeOperation(f.op.ID, f.lease, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.ConfirmOperation(op.ID, op.Ticket, false, reconciledSourceIntact, f.lease, f.now); err != nil {
		t.Fatal(err)
	}
	if err = f.store.ValidateOperation(op.ID, op.Ticket, f.lease, f.now); !errors.Is(err, model.ErrRetentionBlocked) {
		t.Fatalf("free-form error forged reconciliation: %v", err)
	}
}

func TestRepeatedProvisionalFailuresKeepOneErrorUntilCompletion(t *testing.T) {
	f := newProvisionalRetryFixture(t, "quarantine")
	for i := 0; i < 4; i++ {
		op, err := f.store.AuthorizeOperation(f.op.ID, f.lease, f.now)
		if err != nil {
			t.Fatal(err)
		}
		if op.ExecutionCommitted {
			t.Fatal("retry silently committed before final validation")
		}
		if err = f.store.ConfirmOperation(op.ID, op.Ticket, false, fmt.Sprintf("transient source failure %d", i), f.lease, f.now); err != nil {
			t.Fatal(err)
		}
		if err = f.store.ResetAuthorized(op.ID, f.now); err != nil {
			t.Fatal(err)
		}
		messages, err := f.store.DueMail(f.now, 100)
		if err != nil {
			t.Fatal(err)
		}
		anomaly := 0
		for _, m := range messages {
			if m.Kind == "resolution" {
				t.Fatal("failed retry sent resolution")
			}
			if m.Kind == "anomaly" {
				anomaly++
			}
		}
		if anomaly != 1 {
			t.Fatalf("failure mail duplicated: %d", anomaly)
		}
		f.now = f.now.Add(time.Minute)
		if err = f.store.SetCheckState(model.MonitoringAnomaly, 7, f.now); err != nil {
			t.Fatal(err)
		}
	}
	op, err := f.store.AuthorizeOperation(f.op.ID, f.lease, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.ValidateOperation(op.ID, op.Ticket, f.lease, f.now); err != nil {
		t.Fatal(err)
	}
	if err = f.store.ConfirmOperation(op.ID, op.Ticket, true, "", f.lease, f.now); err != nil {
		t.Fatal(err)
	}
	messages, err := f.store.DueMail(f.now, 100)
	if err != nil {
		t.Fatal(err)
	}
	resolutions := 0
	for _, m := range messages {
		if m.Kind == "resolution" {
			resolutions++
		}
	}
	if resolutions != 1 {
		t.Fatalf("verified success resolution count: %d", resolutions)
	}
}

func TestProvisionalRetryRequiresFreshCheckBeforeTicket(t *testing.T) {
	f := newProvisionalRetryFixture(t, "quarantine")
	f.now = f.now.Add(11 * time.Minute)
	if _, err := f.store.AuthorizeOperation(f.op.ID, f.lease, f.now); !errors.Is(err, model.ErrRetentionBlocked) {
		t.Fatalf("stale check admitted retry: %v", err)
	}
}

func TestRepeatedSourceErrorDoesNotReplaceAdministrativeReason(t *testing.T) {
	f := newProvisionalRetryFixture(t, "quarantine")
	op, err := f.store.AuthorizeOperation(f.op.ID, f.lease, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.PauseRetention("admin maintenance"); err != nil {
		t.Fatal(err)
	}
	if err = f.store.ConfirmOperation(op.ID, op.Ticket, false, "different source failure", f.lease, f.now); err != nil {
		t.Fatal(err)
	}
	status, err := f.store.AutomationStatus()
	if err != nil || !status.ManualPaused || status.BlockReason != "admin maintenance" {
		t.Fatalf("retry replaced independent reason: %+v %v", status, err)
	}
}
