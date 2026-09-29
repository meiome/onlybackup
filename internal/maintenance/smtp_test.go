package maintenance

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

func startTestSMTP(t *testing.T, messages int) (model.MailSettings, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	received := make(chan string, messages)
	go func() {
		defer close(received)
		for accepted := 0; accepted < messages; accepted++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			reader := bufio.NewReader(connection)
			writer := bufio.NewWriter(connection)
			fmt.Fprint(writer, "220 test-smtp ESMTP\r\n")
			writer.Flush()
			var data strings.Builder
			inData := false
			for {
				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					break
				}
				if inData {
					if line == ".\r\n" {
						received <- data.String()
						inData = false
						fmt.Fprint(writer, "250 queued\r\n")
						writer.Flush()
						continue
					}
					data.WriteString(line)
					continue
				}
				command := strings.ToUpper(strings.TrimSpace(line))
				switch {
				case strings.HasPrefix(command, "EHLO"):
					fmt.Fprint(writer, "250-test-smtp\r\n250 HELP\r\n")
				case strings.HasPrefix(command, "HELO"), strings.HasPrefix(command, "MAIL FROM:"), strings.HasPrefix(command, "RCPT TO:"):
					fmt.Fprint(writer, "250 ok\r\n")
				case command == "DATA":
					inData = true
					fmt.Fprint(writer, "354 end with dot\r\n")
				case command == "QUIT":
					fmt.Fprint(writer, "221 bye\r\n")
					writer.Flush()
					connection.Close()
					goto nextConnection
				default:
					fmt.Fprint(writer, "500 unsupported\r\n")
				}
				writer.Flush()
			}
			connection.Close()
		nextConnection:
		}
	}()
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return model.MailSettings{Host: host, Port: port, From: "OnlyBackup <backup@example.test>", Recipients: "admin@example.test"}, received
}

func TestSMTPSendsStableMessageIDToLocalServer(t *testing.T) {
	settings, received := startTestSMTP(t, 2)
	sender := SMTP{Timeout: 2 * time.Second}
	message := model.MailMessage{StableID: "anomaly:key:2026-09-22", Subject: "OnlyBackup test\nignored", Body: "first line\nsecond line"}
	for attempt := 0; attempt < 2; attempt++ {
		if err := sender.Send(context.Background(), settings, message); err != nil {
			t.Fatal(err)
		}
	}
	first, ok := <-received
	if !ok {
		t.Fatal("SMTP server received no message")
	}
	second, ok := <-received
	if !ok {
		t.Fatal("SMTP server received only one message")
	}
	messageID := ""
	for _, line := range strings.Split(first, "\r\n") {
		if strings.HasPrefix(line, "Message-ID:") {
			messageID = line
			break
		}
	}
	if messageID == "" || !strings.Contains(second, messageID+"\r\n") {
		t.Fatalf("Message-ID not stable across retry: %q", messageID)
	}
	if strings.Contains(first, "Subject: OnlyBackup test\r\nignored") || !strings.Contains(first, "first line\r\nsecond line") {
		t.Fatalf("unsafe header or malformed body: %q", first)
	}
}
