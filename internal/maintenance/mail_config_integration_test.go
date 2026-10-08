package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

type configProofSender func(model.MailSettings) error

func (send configProofSender) Send(_ context.Context, settings model.MailSettings, _ model.MailMessage) error {
	return send(settings)
}

func TestMailDeliveryConfirmsTheConfigurationActuallySent(t *testing.T) {
	for _, phase := range []string{"before-send", "during-send"} {
		t.Run(phase, func(t *testing.T) {
			now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
			db, service, _, _ := auditService(t, &now)
			oldSettings, err := db.MailSettings()
			if err != nil {
				t.Fatal(err)
			}
			if err := db.QueueReport("mail-test:old-settings", "test", "test", now); err != nil {
				t.Fatal(err)
			}
			newSettings := oldSettings
			newSettings.Host, newSettings.Recipients = "new.smtp.test", "new-admin@test"
			reset := func() error {
				return db.SetupAutomationMode(newSettings, "UTC", "auto", now)
			}
			if phase == "before-send" {
				if err := reset(); err != nil {
					t.Fatal(err)
				}
			}
			sent := 0
			service.Mail = configProofSender(func(settings model.MailSettings) error {
				sent++
				if settings != oldSettings {
					t.Fatalf("unexpected settings used for delivery: %+v", settings)
				}
				if phase == "during-send" {
					return reset()
				}
				return nil
			})
			if err := service.deliverMail(context.Background(), oldSettings, now); err != nil {
				t.Fatal(err)
			}
			status, err := db.AutomationStatus()
			if err != nil || status.MailTested || !status.DeletionBlocked || sent != 1 {
				t.Fatalf("old delivery certified the replacement settings: %+v sent=%d %v", status, sent, err)
			}
			if due, err := db.DueMail(now, 10); err != nil || len(due) != 0 {
				t.Fatalf("successful old delivery was not recorded: %+v %v", due, err)
			}
			current, err := db.MailSettings()
			if err != nil || current.ConfigVersion != oldSettings.ConfigVersion+1 {
				t.Fatalf("replacement version missing: %+v %v", current, err)
			}
			if err := db.QueueReport("mail-test:new-settings", "test", "test", now); err != nil {
				t.Fatal(err)
			}
			service.Mail = configProofSender(func(settings model.MailSettings) error {
				if settings != current {
					t.Fatalf("new test used incorrect settings: %+v", settings)
				}
				return nil
			})
			if err := service.deliverMail(context.Background(), current, now); err != nil {
				t.Fatal(err)
			}
			if status, err := db.AutomationStatus(); err != nil || !status.MailTested {
				t.Fatalf("current configuration could not be certified: %+v %v", status, err)
			}
		})
	}
}
