package maintenance

import (
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/localclient"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

func TestReportBadgeOnlyGreenWithoutProblems(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	snapshot := localclient.Snapshot{
		Status:  model.AutomationStatus{Enabled: true, MailTested: true, MonitoringState: model.MonitoringLearning, DeletionBlocked: true, Timezone: "Europe/Rome"},
		Keys:    []model.Key{{ID: "one", Name: "Linux"}},
		Backups: []model.Backup{{Receipt: model.Receipt{Status: model.BackupComplete, ReceivedAt: now.Format(time.RFC3339), Size: 1 << 20}, KeyID: "one"}},
	}
	filesystem := policy.Filesystem{Blocks: 1000, Bfree: 990, Bavail: 990, BlockSize: 1}
	good := buildEmailReport(snapshot, filesystem, nil, now)
	if !strings.HasPrefix(good, "Esito: OK\n") || !strings.Contains(good, "Azione: nessuna oggi") ||
		!strings.Contains(renderMailHTML(model.MailMessage{Subject: "OnlyBackup: report periodico", Body: good}), "TUTTO OK") {
		t.Fatalf("report sano non chiaro: %q", good)
	}

	snapshot.Status.MonitoringState = model.MonitoringRegular
	snapshot.Status.MailTested = true
	snapshot.Status.ModelRevision = 7
	snapshot.Status.LastCheckAt = now.Unix()
	snapshot.Status.LastCheckRegular = true
	blocked := buildEmailReport(snapshot, filesystem, nil, now)
	if !strings.HasPrefix(blocked, "Esito: ATTENZIONE\n") || !strings.Contains(blocked, "retention resume") ||
		!strings.Contains(renderMailHTML(model.MailMessage{Subject: "OnlyBackup: report periodico", Body: blocked}), "ATTENZIONE") {
		t.Fatalf("sblocco richiesto non evidenziato: %q", blocked)
	}

	snapshot.Status.MonitoringState = model.MonitoringLearning
	snapshot.Status.ActiveAnomalies = 1
	if report := buildEmailReport(snapshot, filesystem, nil, now); !strings.HasPrefix(report, "Esito: ATTENZIONE\n") {
		t.Fatalf("anomalia senza badge rosso: %q", report)
	}
}

func TestHTMLMailKeepsPlainTextAndEscapesAnomaly(t *testing.T) {
	message := model.MailMessage{StableID: "anomaly:example", Kind: "anomaly", Subject: "OnlyBackup: nuova anomalia", Body: "Backup <Windows> mancante\nControllare il client"}
	raw, err := composeMail("backup@example.test", []string{"admin@example.test"}, message, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(parsed.Header.Get("Subject"), "OnlyBackup: ATTENZIONE") {
		t.Fatalf("oggetto anomalia non riconoscibile nella casella: %q", parsed.Header.Get("Subject"))
	}
	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("email non multipart: %q, %v", mediaType, err)
	}
	parts := multipart.NewReader(parsed.Body, params["boundary"])
	for index, wantType := range []string{"text/plain", "text/html"} {
		part, partErr := parts.NextPart()
		if partErr != nil {
			t.Fatal(partErr)
		}
		partType, _, partErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if partErr != nil || partType != wantType {
			t.Fatalf("parte %d: tipo %q, errore %v", index, partType, partErr)
		}
		decoded, partErr := io.ReadAll(quotedprintable.NewReader(part))
		if partErr != nil {
			t.Fatal(partErr)
		}
		if index == 0 && !strings.Contains(string(decoded), "Backup <Windows> mancante") {
			t.Fatalf("testo semplice perso: %q", decoded)
		}
		if index == 1 && (!strings.Contains(string(decoded), "ATTENZIONE") || !strings.Contains(string(decoded), "&lt;Windows&gt;") || strings.Contains(string(decoded), "<Windows>")) {
			t.Fatalf("HTML non sicuro o senza badge: %q", decoded)
		}
	}
}

func TestReportSubjectShowsBadgeBeforeOpening(t *testing.T) {
	for _, test := range []struct{ subject, body, want string }{
		{"OnlyBackup: report periodico", "Esito: OK\nAzione: nessuna oggi", "OnlyBackup: TUTTO OK | report"},
		{"OnlyBackup: report periodico", "Esito: ATTENZIONE\nAzione: controllare", "OnlyBackup: ATTENZIONE | report"},
		{"OnlyBackup: report periodico [TEST]", "Esito: OK\nAzione: email di prova", "OnlyBackup: TUTTO OK | report [TEST]"},
	} {
		raw, err := composeMail("backup@example.test", []string{"admin@example.test"}, model.MailMessage{
			StableID: "report:test", Subject: test.subject, Body: test.body,
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := mail.ReadMessage(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if subject := parsed.Header.Get("Subject"); subject != test.want {
			t.Fatalf("oggetto %q, atteso %q", subject, test.want)
		}
	}
}
