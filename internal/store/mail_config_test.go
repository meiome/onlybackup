package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

func queueMailProof(t *testing.T, s *Store, stableID string, now time.Time) model.MailMessage {
	t.Helper()
	if err := s.QueueReport(stableID, "mail proof", "test", now); err != nil {
		t.Fatal(err)
	}
	messages, err := s.DueMail(now, 10)
	if err != nil || len(messages) != 1 {
		t.Fatalf("mail queue: %+v %v", messages, err)
	}
	return messages[0]
}

func TestMailProofRequiresConfigurationActuallyUsedForDelivery(t *testing.T) {
	for _, scenario := range []string{"current", "changed", "same settings reset", "missing version", "failed", "ordinary report", "replayed"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, _, now := activationFixture(t)
			settings, err := s.MailSettings()
			if err != nil || settings.ConfigVersion <= 0 {
				t.Fatalf("missing configuration version: %+v %v", settings, err)
			}
			stableID := "mail-test:configuration-proof"
			if scenario == "ordinary report" {
				stableID = "ordinary-report"
			}
			message := queueMailProof(t, s, stableID, now)
			version, sendErr := settings.ConfigVersion, ""
			if scenario == "replayed" {
				if err := s.ConfirmMail(message.ID, "", version, now); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "changed" || scenario == "same settings reset" || scenario == "replayed" {
				if scenario == "changed" {
					settings.Host, settings.Recipients = "new.smtp.test", "new-admin@test"
				}
				if err := s.SetupAutomationMode(settings, "UTC", "auto", now.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
				current, err := s.MailSettings()
				if err != nil || current.ConfigVersion != version+1 {
					t.Fatalf("setup did not advance version: %+v %v", current, err)
				}
				if scenario == "replayed" {
					// Even a duplicate claiming the new version cannot reuse a sent test.
					version = current.ConfigVersion
				}
			}
			if scenario == "missing version" {
				version = 0
			}
			if scenario == "failed" {
				sendErr = "SMTP unavailable"
			}
			at := now.Add(2 * time.Minute)
			if err := s.ConfirmMail(message.ID, sendErr, version, at); err != nil {
				t.Fatal(err)
			}
			completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, at)
			status := activationStatus(t, s)
			wantProof := scenario == "current"
			if status.MailTested != wantProof || status.DeletionBlocked == wantProof {
				t.Fatalf("unexpected mail proof or retention activation: %+v", status)
			}
			var sentAt int64
			if err := s.db.QueryRow(`SELECT sent_at FROM mail_queue WHERE id=?`, message.ID).Scan(&sentAt); err != nil || (sentAt != 0) != (sendErr == "") {
				t.Fatalf("delivery was not recorded correctly: %d %v", sentAt, err)
			}
			if scenario == "changed" {
				current, err := s.MailSettings()
				if err != nil {
					t.Fatal(err)
				}
				message = queueMailProof(t, s, "mail-test:new-config", at)
				if err := s.ConfirmMail(message.ID, "", current.ConfigVersion, at); err != nil {
					t.Fatal(err)
				}
				completeActivationCheck(t, s, model.MonitoringRegular, 7, nil, at)
				if activationStatus(t, s).DeletionBlocked {
					t.Fatal("new configuration could not activate after its own successful test")
				}
			}
		})
	}
}

func TestV12MigrationPreservesMailProofAndVersionSurvivesRestart(t *testing.T) {
	s, _, _, now := activationFixture(t)
	if err := s.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	before := activationStatus(t, s)
	var path string
	if err := s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`ALTER TABLE automation_state DROP COLUMN mail_config_version; PRAGMA user_version=11`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings, err := s.MailSettings()
	if err != nil || settings.ConfigVersion != 1 || s.schemaVersion != 12 {
		t.Fatalf("migration did not initialize mail version: %+v %v", settings, err)
	}
	after := activationStatus(t, s)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("migration changed proof or policy: before=%+v after=%+v", before, after)
	}
	if err := s.SetupAutomation(settings, "UTC", now); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settings, err = s.MailSettings()
	if err != nil || settings.ConfigVersion != 2 || activationStatus(t, s).MailTested {
		t.Fatalf("restart lost the new configuration version: %+v %v", settings, err)
	}
}
