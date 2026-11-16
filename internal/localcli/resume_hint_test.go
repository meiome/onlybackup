package localcli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/meiome/onlybackup/internal/model"
)

func statusCall(status model.AutomationStatus, err error) func(string, string, any, any) error {
	return func(method, path string, request, response any) error {
		if method != "GET" || path != "/v1/status" {
			return errors.New("richiesta inattesa: " + method + " " + path)
		}
		if err != nil {
			return err
		}
		*response.(*model.AutomationStatus) = status
		return nil
	}
}

// Only the state that retention resume actually releases gets the hint.
func TestPrintResumeHintOnlyForReleasableSuspension(t *testing.T) {
	releasable := model.AutomationStatus{
		MonitoringState: model.MonitoringPaused, DeletionBlocked: true,
		Enabled: true, MailTested: true,
	}
	for _, test := range []struct {
		name     string
		status   model.AutomationStatus
		wantHint bool
	}{
		{"releasable suspension", releasable, true},
		{"resume already booked", with(releasable, func(s *model.AutomationStatus) { s.ResumePending = true }), false},
		{"manual pause", with(releasable, func(s *model.AutomationStatus) { s.ManualPaused = true }), false},
		{"other anomaly still active", with(releasable, func(s *model.AutomationStatus) { s.ActiveAnomalies = 1 }), false},
		{"automation disabled", with(releasable, func(s *model.AutomationStatus) { s.Enabled = false }), false},
		{"mail not tested", with(releasable, func(s *model.AutomationStatus) { s.MailTested = false }), false},
		{"regular monitoring", model.AutomationStatus{MonitoringState: model.MonitoringRegular, Enabled: true, MailTested: true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			printResumeHint(statusCall(test.status, nil), &errs)
			if got := strings.Contains(errs.String(), model.ResumeHint); got != test.wantHint {
				t.Fatalf("hint=%v want %v, stderr=%q", got, test.wantHint, errs.String())
			}
			if out.Len() != 0 {
				t.Fatalf("hint leaked to stdout: %q", out.String())
			}
		})
	}
}

// A failed status read must not hide that the action succeeded; it is reported.
func TestPrintResumeHintReportsStatusErrorWithoutFailing(t *testing.T) {
	var errs bytes.Buffer
	printResumeHint(statusCall(model.AutomationStatus{}, errors.New("writer non disponibile")), &errs)
	if !strings.Contains(errs.String(), "writer non disponibile") || strings.Contains(errs.String(), model.ResumeHint) {
		t.Fatalf("unexpected stderr: %q", errs.String())
	}
}

func with(s model.AutomationStatus, change func(*model.AutomationStatus)) model.AutomationStatus {
	change(&s)
	return s
}
