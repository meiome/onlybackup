package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

func TestMissingBackupMailIdentifiesClientAndMissedAppointment(t *testing.T) {
	for _, hasCopy := range []bool{false, true} {
		t.Run(fmt.Sprintf("hasCopy=%t", hasCopy), func(t *testing.T) {
			s, keyID, token := setup(t, model.Profile{Name: "missing-mail", TotalBytes: 1000, MaxBackupBytes: 500, UploadsPerDay: 20, Concurrent: 2})
			expected := time.Date(2026, 10, 4, 13, 0, 0, 0, time.UTC)
			detected := expected.Add(18 * time.Hour)
			if err := s.SetupAutomation(MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "Europe/Rome", detected); err != nil {
				t.Fatal(err)
			}
			latest := "nessuna"
			if hasCopy {
				completedBackup(t, s, token, "missing-mail-previous", expected.Add(-24*time.Hour))
				latest = "03/10/2026 15:00 CEST"
			}
			// A newer copy for another client must not appear in this alert.
			_, otherToken, err := s.CreateKey("altro client", "missing-mail")
			if err != nil {
				t.Fatal(err)
			}
			completedBackup(t, s, otherToken, "missing-mail-other-client", expected.Add(time.Hour))
			stableKey := "slot:" + keyID + ":" + expected.Format(time.RFC3339)
			created, err := s.OpenAnomalyAt(stableKey, "schedule_missing", keyID, "", "copia attesa non ricevuta", expected, detected)
			if err != nil || !created {
				t.Fatalf("anomalia non creata: %v, %v", created, err)
			}
			created, err = s.OpenAnomalyAt(stableKey, "schedule_missing", keyID, "", "copia attesa non ricevuta", expected, detected.Add(time.Minute))
			if err != nil || created {
				t.Fatalf("anomalia duplicata: %v, %v", created, err)
			}
			messages, err := s.DueMail(detected.Add(time.Minute), 20)
			if err != nil || len(messages) != 1 {
				t.Fatalf("email attese 1: %+v, %v", messages, err)
			}
			message := messages[0]
			wantBody := "cliente condiviso: backup del 04/10/2026 15:00 CEST non ricevuto.\nUltima ricevuta: " + latest + ".\nVerifica server e invio del client."
			if message.Subject != "OnlyBackup: backup mancante | cliente condiviso" || message.Body != wantBody {
				t.Fatalf("avviso errato: %+v", message)
			}
			anomalies, err := s.Anomalies(true)
			if err != nil || len(anomalies) != 1 || anomalies[0].Detail != "copia attesa non ricevuta" || anomalies[0].EventAt != expected.Unix() {
				t.Fatalf("dati del monitoraggio alterati: %+v, %v", anomalies, err)
			}
		})
	}
}
