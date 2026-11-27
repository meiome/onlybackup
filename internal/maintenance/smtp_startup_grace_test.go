package maintenance

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

// smtpAnomalies counts smtp:delivery anomalies, only the active ones or all
// of them, resolved ones included.
func smtpAnomalies(t *testing.T, db *store.Store, activeOnly bool) int {
	t.Helper()
	anomalies, err := db.Anomalies(activeOnly)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, anomaly := range anomalies {
		if anomaly.StableKey == "smtp:delivery" {
			count++
		}
	}
	return count
}

// resolverDown is the error net.Dialer returns while the local resolver is
// not listening yet.
func resolverDown() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
		Err: "connection refused", Name: "smtp.test", Server: "[::1]:53",
	}}
}

func smtpGraceService(t *testing.T, now *time.Time, stableID string) (*store.Store, *Service, model.MailSettings) {
	t.Helper()
	db, service, _, _ := auditService(t, now)
	settings, err := db.MailSettings()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueueReport(stableID, "report", "report", *now); err != nil {
		t.Fatal(err)
	}
	return db, service, settings
}

func TestSMTPNetworkFailureDuringStartupGraceStaysQueued(t *testing.T) {
	now := time.Date(2026, 11, 23, 7, 47, 0, 0, time.UTC)
	db, service, settings := smtpGraceService(t, &now, "report:boot")
	dnsReady := false
	sent := 0
	service.Mail = configProofSender(func(model.MailSettings) error {
		if !dnsReady {
			return resolverDown()
		}
		sent++
		return nil
	})

	if err := service.deliverMail(context.Background(), settings, now); err != nil {
		t.Fatal(err)
	}
	if n := smtpAnomalies(t, db, true); n != 0 {
		t.Fatalf("startup failure opened %d smtp anomalies", n)
	}
	now = now.Add(5 * time.Minute)
	if due, err := db.DueMail(now, 10); err != nil || len(due) != 1 {
		t.Fatalf("failed startup mail is not queued for retry: %+v %v", due, err)
	}

	dnsReady = true
	if err := service.deliverMail(context.Background(), settings, now); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("retry after startup sent %d messages", sent)
	}
	if n := smtpAnomalies(t, db, false); n != 0 {
		t.Fatalf("transient startup failure recorded %d smtp anomalies", n)
	}
}

func TestSMTPNetworkFailureAfterStartupGraceOpensAnomaly(t *testing.T) {
	now := time.Date(2026, 11, 23, 7, 47, 0, 0, time.UTC)
	db, service, settings := smtpGraceService(t, &now, "report:boot")
	service.MailStartupGrace = 10 * time.Minute
	service.Mail = configProofSender(func(model.MailSettings) error { return resolverDown() })
	for step := 0; step < 2; step++ {
		if err := service.deliverMail(context.Background(), settings, now); err != nil {
			t.Fatal(err)
		}
		if n := smtpAnomalies(t, db, true); n != 0 {
			t.Fatalf("failure at +%v opened an anomaly inside the grace", now.Sub(service.mailStartedAt))
		}
		now = now.Add(5 * time.Minute)
	}
	if err := service.deliverMail(context.Background(), settings, now); err != nil {
		t.Fatal(err)
	}
	if n := smtpAnomalies(t, db, true); n != 1 {
		t.Fatalf("persistent failure after the grace opened %d smtp anomalies", n)
	}
}

func TestSMTPFailureOpensAnomalyInsideStartupGrace(t *testing.T) {
	cases := []struct {
		name     string
		stableID string
		grace    time.Duration
		err      error
	}{
		{"authentication", "report:boot", 0, errors.New("535 authentication failed")},
		{"reset after connect", "report:boot", 0, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}},
		{"tls alert", "report:boot", 0, &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}},
		{"test mail", "mail-test:1", 0, resolverDown()},
		{"grace disabled", "report:boot", -1, resolverDown()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 11, 23, 7, 47, 0, 0, time.UTC)
			db, service, settings := smtpGraceService(t, &now, tc.stableID)
			service.MailStartupGrace = tc.grace
			service.Mail = configProofSender(func(model.MailSettings) error { return tc.err })
			if err := service.deliverMail(context.Background(), settings, now); err != nil {
				t.Fatal(err)
			}
			if n := smtpAnomalies(t, db, true); n != 1 {
				t.Fatalf("failure opened %d smtp anomalies, want 1", n)
			}
		})
	}
}

func TestSMTPFailureAfterDeliveryOpensAnomalyImmediately(t *testing.T) {
	now := time.Date(2026, 11, 23, 7, 47, 0, 0, time.UTC)
	db, service, settings := smtpGraceService(t, &now, "report:first")
	fail := false
	service.Mail = configProofSender(func(model.MailSettings) error {
		if fail {
			return resolverDown()
		}
		return nil
	})
	if err := service.deliverMail(context.Background(), settings, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := db.QueueReport("report:second", "report", "report", now); err != nil {
		t.Fatal(err)
	}
	fail = true
	if err := service.deliverMail(context.Background(), settings, now); err != nil {
		t.Fatal(err)
	}
	if n := smtpAnomalies(t, db, true); n != 1 {
		t.Fatalf("failure after a successful delivery opened %d smtp anomalies", n)
	}
}

func TestSMTPGraceIgnoresSettingsWithoutHost(t *testing.T) {
	now := time.Date(2026, 11, 23, 7, 47, 0, 0, time.UTC)
	_, service, settings := smtpGraceService(t, &now, "report:boot")
	settings.Host = ""
	service.Mail = configProofSender(func(model.MailSettings) error { return nil })
	if err := service.deliverMail(context.Background(), settings, now); err != nil {
		t.Fatal(err)
	}
	if !service.mailStartedAt.IsZero() {
		t.Fatalf("grace started without an SMTP host at %v", service.mailStartedAt)
	}
}
