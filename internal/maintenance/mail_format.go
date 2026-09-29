package maintenance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

func composeMail(from string, recipients []string, message model.MailMessage, now time.Time) (string, error) {
	stableHash := sha256.Sum256([]byte(message.StableID))
	subject := message.Subject
	isReport := strings.HasPrefix(message.Subject, "OnlyBackup: report periodico")
	testSuffix := ""
	if strings.HasSuffix(message.Subject, " [TEST]") {
		testSuffix = " [TEST]"
	}
	switch {
	case isReport && strings.HasPrefix(message.Body, "Esito: OK\n"):
		subject = "OnlyBackup: TUTTO OK | report" + testSuffix
	case isReport:
		subject = "OnlyBackup: ATTENZIONE | report" + testSuffix
	case message.Kind == "anomaly" || strings.Contains(message.Subject, "anomalie attive"):
		subject = "OnlyBackup: ATTENZIONE | " + strings.TrimPrefix(message.Subject, "OnlyBackup: ")
	}
	header := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s@onlybackup.local>\r\nDate: %s\r\nMIME-Version: 1.0\r\n",
		sanitizeHeader(from), strings.Join(recipients, ", "), sanitizeHeader(subject),
		hex.EncodeToString(stableHash[:]), now.Format(time.RFC1123Z))
	markup := renderMailHTML(message)
	if markup == "" {
		return header + "Content-Type: text/plain; charset=UTF-8\r\n\r\n" + strings.ReplaceAll(message.Body, "\n", "\r\n") + "\r\n", nil
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, part := range []struct{ contentType, text string }{
		{"text/plain; charset=UTF-8", message.Body},
		{"text/html; charset=UTF-8", markup},
	} {
		partWriter, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.contentType},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return "", err
		}
		encoded := quotedprintable.NewWriter(partWriter)
		if _, err = encoded.Write([]byte(part.text)); err != nil {
			return "", err
		}
		if err = encoded.Close(); err != nil {
			return "", err
		}
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	return header + "Content-Type: multipart/alternative; boundary=" + writer.Boundary() + "\r\n\r\n" + body.String(), nil
}

func renderMailHTML(message model.MailMessage) string {
	isReport := strings.HasPrefix(message.Subject, "OnlyBackup: report periodico")
	isAlert := message.Kind == "anomaly" || strings.Contains(message.Subject, "anomalie attive")
	if !isReport && !isAlert {
		return ""
	}
	good := isReport && strings.HasPrefix(message.Body, "Esito: OK\n")
	badge, title, summary, badgeBackground, badgeText, accent :=
		"ATTENZIONE", "Controlla OnlyBackup", "È presente un problema o un intervento richiesto.", "#a83220", "#ffffff", "#fdf0ec"
	if good {
		badge, title, summary, badgeBackground, badgeText, accent =
			"TUTTO OK", "Nessun problema rilevato", "Riepilogo delle copie e dello stato del servizio.", "#166b3e", "#ffffff", "#eff8f0"
	} else if isAlert {
		title, summary = "Avviso OnlyBackup", "È stata rilevata un'anomalia."
	}
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="it"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>`)
	b.WriteString(`<body style="margin:0;padding:0;background:#e9edf0;color:#18303a;font-family:Arial,Helvetica,sans-serif;font-size:15px;line-height:1.5">`)
	b.WriteString(`<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:#e9edf0"><tr><td align="center" style="padding:20px 10px">`)
	b.WriteString(`<table role="presentation" width="640" cellspacing="0" cellpadding="0" style="width:100%;max-width:640px;background:#ffffff;border:1px solid #d7e0e3;border-radius:12px">`)
	fmt.Fprintf(&b, `<tr><td style="padding:21px 25px;background:%s;color:%s;font-size:23px;font-weight:bold;letter-spacing:.03em;border-radius:12px 12px 0 0">&#9679;&nbsp; %s</td></tr>`, badgeBackground, badgeText, badge)
	b.WriteString(`<tr><td style="padding:16px 25px;background:#17343d;color:#ffffff;font-size:13px;font-weight:bold;letter-spacing:.06em">ONLYBACKUP</td></tr>`)
	b.WriteString(`<tr><td style="padding:27px 25px">`)
	fmt.Fprintf(&b, `<h1 style="margin:0 0 8px;color:#17343d;font-size:27px;line-height:1.2">%s</h1>`, html.EscapeString(title))
	fmt.Fprintf(&b, `<p style="margin:0 0 20px;color:#526a73">%s</p>`, html.EscapeString(summary))
	if isReport {
		renderReportHTML(&b, message.Body, accent, good)
	} else {
		fmt.Fprintf(&b, `<div style="padding:16px 18px;background:%s;border-left:4px solid %s;border-radius:0 7px 7px 0;color:#18303a">%s</div>`,
			accent, badgeBackground, htmlParagraphs(message.Body))
	}
	b.WriteString(`</td></tr><tr><td style="padding:15px 25px;border-top:1px solid #e6ecee;color:#647981;font-size:11px">OnlyBackup · avviso automatico dal server</td></tr>`)
	b.WriteString(`</table></td></tr></table></body></html>`)
	return b.String()
}

func renderReportHTML(b *strings.Builder, body, accent string, good bool) {
	lines := strings.Split(body, "\n")
	var action string
	for _, line := range lines {
		if strings.HasPrefix(line, "Azione: ") {
			action = strings.TrimPrefix(line, "Azione: ")
		}
	}
	if action != "" {
		label := "Cosa fare"
		if good {
			label = "Oggi"
		}
		fmt.Fprintf(b, `<div style="margin:0 0 19px;padding:14px 16px;background:%s;border-radius:8px"><strong>%s:</strong> %s</div>`,
			accent, label, html.EscapeString(action))
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "Esito: ") || strings.HasPrefix(line, "Azione: ") || line == "" {
			continue
		}
		if line == "Ultime copie:" {
			b.WriteString(`<p style="margin:22px 0 6px;color:#526a73;font-size:12px;font-weight:bold;letter-spacing:.08em">ULTIME COPIE</p>`)
			continue
		}
		label, value, ok := strings.Cut(strings.TrimPrefix(line, "- "), ": ")
		if !ok {
			continue
		}
		fmt.Fprintf(b, `<table role="presentation" width="100%%" cellspacing="0" cellpadding="0" style="border-bottom:1px solid #e5ebed"><tr><td style="padding:10px 0;color:#18303a;font-weight:bold;vertical-align:top">%s</td><td align="right" style="padding:10px 0;color:#526a73;vertical-align:top">%s</td></tr></table>`,
			html.EscapeString(label), html.EscapeString(value))
	}
}

func htmlParagraphs(body string) string {
	var parts []string
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) != "" {
			parts = append(parts, `<p style="margin:0 0 8px">`+html.EscapeString(line)+`</p>`)
		}
	}
	return strings.Join(parts, "")
}
