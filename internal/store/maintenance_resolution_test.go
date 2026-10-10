package store

import (
	"github.com/meiome/onlybackup/internal/model"
	"strings"
	"testing"
	"time"
)

func TestRepeatedPhysicalFailureDoesNotSendFalseResolutions(t *testing.T) {
	s, _, token := setup(t, model.Profile{Name: "retry-mail", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	old := completedBackup(t, s, token, "mail-old-1234567890", now.AddDate(0, 0, -10))
	completedBackup(t, s, token, "mail-new-1234567890", now.AddDate(0, 0, -1))
	enableRetentionForTest(t, s, now)
	op, err := s.RequestQuarantine(old.ID, "automatic", "worker", "retry", now)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireMaintenanceLease("worker", 0, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.AuthorizeOperation(op.ID, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateOperation(op.ID, op.Ticket, lease, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err = s.ConfirmOperation(op.ID, op.Ticket, false, "invalid cross-device link", lease, now); err != nil {
			t.Fatal(err)
		}
		if err = s.ResetAuthorized(op.ID, now); err != nil {
			t.Fatal(err)
		}
		active, err := s.Anomalies(true)
		if err != nil || len(active) != 1 {
			t.Fatalf("failure closed or duplicated: %+v %v", active, err)
		}
		messages, err := s.DueMail(now, 100)
		if err != nil {
			t.Fatal(err)
		}
		anomaly, resolution := 0, 0
		for _, m := range messages {
			if m.Kind == "anomaly" {
				anomaly++
			}
			if m.Kind == "resolution" {
				resolution++
			}
		}
		if anomaly != 1 || resolution != 0 {
			t.Fatalf("retry generated duplicate/false mail: %d %d", anomaly, resolution)
		}
		now = now.Add(time.Minute)
		op, err = s.AuthorizeOperation(op.ID, lease, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = s.ConfirmOperation(op.ID, op.Ticket, true, "", lease, now); err != nil {
		t.Fatal(err)
	}
	messages, err := s.DueMail(now, 100)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, m := range messages {
		if strings.HasPrefix(m.StableID, "maintenance-resolved:") {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("actual completion resolution count: %d", resolved)
	}
}
