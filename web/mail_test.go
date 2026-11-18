//nolint:testpackage // tests unexported template and CSV helpers directly
package web

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
)

func TestParseEmailCellSplitsJSONAndCommas(t *testing.T) {
	got := parseEmailCell(`["a@cafe.test", "b@cafe.test"]`)
	if len(got) != 2 || got[0] != "a@cafe.test" || got[1] != "b@cafe.test" {
		t.Fatalf("json list: %#v", got)
	}

	got = parseEmailCell("a@cafe.test, b@cafe.test; a@cafe.test")
	if len(got) != 2 {
		t.Fatalf("deduped list: %#v", got)
	}
}

func TestRecipientsFromCSVUsesEmailsColumn(t *testing.T) {
	csv := "title,emails,phone\nCafe,\"a@cafe.test, b@cafe.test\",555\nNoMail,,\n"

	recs, err := recipientsFromCSV("job", "Job", strings.NewReader(csv))
	if err != nil {
		t.Fatalf("recipientsFromCSV: %v", err)
	}

	if len(recs) != 2 || recs[0].Title != "Cafe" || recs[1].Email != "b@cafe.test" {
		t.Fatalf("recipients: %#v", recs)
	}
}

func TestRenderMailStripsSubjectNewlines(t *testing.T) {
	subjectT, bodyT, err := parseMailTemplates("Hello\nBcc: x {{.Title}}", "Hi {{.Email}}")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	subject, body, err := renderMail(subjectT, bodyT, &MailVars{Title: "Cafe", Email: "a@cafe.test"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if strings.ContainsAny(subject, "\r\n") {
		t.Fatalf("subject kept a newline: %q", subject)
	}

	if body != "Hi a@cafe.test" {
		t.Fatalf("body: %q", body)
	}
}

func TestRenderMailRejectsUnknownField(t *testing.T) {
	_, _, err := parseMailTemplates("Hi {{.Password}}", "Body")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	subjectT, bodyT, err := parseMailTemplates("Hi {{.Password}}", "Body")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if _, _, err := renderMail(subjectT, bodyT, &MailVars{}); err == nil {
		t.Fatal("expected unknown field to fail")
	}
}

func TestBuildMessageUsesHTMLContentType(t *testing.T) {
	html := buildMessage("Cafe", "from@cafe.test", "to@cafe.test", &OutgoingMail{Subject: "Hello", Body: "<p>Hi</p>"}, MailFormatHTML)
	if !strings.Contains(html, "Content-Type: text/html") || !strings.Contains(html, "<p>Hi</p>") {
		t.Fatalf("html message: %s", html)
	}

	plain := buildMessage("Cafe", "from@cafe.test", "to@cafe.test", &OutgoingMail{Subject: "Hello", Body: "Hi"}, MailFormatPlain)
	if !strings.Contains(plain, "Content-Type: text/plain") {
		t.Fatalf("plain message: %s", plain)
	}
}

func TestCheckSMTPQuitsWithoutSending(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer func() {
		_ = ln.Close()
	}()

	got := make(chan string, 1)

	go serveOneSMTP(t, ln, got)

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	cfg := MailConfig{Host: "127.0.0.1", Encryption: MailPlain}
	cfg.Port = mustPort(t, port)

	if err := CheckSMTP(t.Context(), &cfg); err != nil {
		t.Fatalf("CheckSMTP: %v", err)
	}
}

func TestSendSMTPWritesPlainText(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer func() {
		_ = ln.Close()
	}()

	got := make(chan string, 1)

	go serveOneSMTP(t, ln, got)

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	cfg := MailConfig{
		Host:       "127.0.0.1",
		Port:       0,
		FromEmail:  "from@cafe.test",
		FromName:   "Cafe",
		Encryption: MailPlain,
	}

	cfg.Port = mustPort(t, port)

	err = SendSMTP(t.Context(), &cfg, &OutgoingMail{To: "to@cafe.test", Subject: "Hello", Body: "Body line"})
	if err != nil {
		t.Fatalf("SendSMTP: %v", err)
	}

	msg := <-got
	if !strings.Contains(msg, "Subject: Hello") || !strings.Contains(msg, "Body line") {
		t.Fatalf("message: %s", msg)
	}

	if !strings.Contains(msg, "from@cafe.test") {
		t.Fatalf("from missing: %s", msg)
	}
}

func serveOneSMTP(t *testing.T, ln net.Listener, got chan<- string) {
	t.Helper()

	conn, err := ln.Accept()
	if err != nil {
		return
	}

	defer func() {
		_ = conn.Close()
	}()

	reader := bufio.NewReader(conn)
	write := func(line string) {
		_, _ = io.WriteString(conn, line+"\r\n")
	}

	write("220 localhost")

	var data strings.Builder

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			write("250 localhost")
		case strings.HasPrefix(upper, "MAIL"), strings.HasPrefix(upper, "RCPT"):
			write("250 OK")
		case upper == "DATA":
			write("354 OK")

			for {
				row, err := reader.ReadString('\n')
				if err != nil {
					return
				}

				if strings.TrimRight(row, "\r\n") == "." {
					break
				}

				data.WriteString(row)
			}

			write("250 OK")

			got <- data.String()
		case upper == "QUIT":
			write("221 bye")

			return
		default:
			write("250 OK")
		}
	}
}

func mustPort(t *testing.T, raw string) int {
	t.Helper()

	var port int

	for _, c := range raw {
		if c < '0' || c > '9' {
			t.Fatalf("port %q", raw)
		}

		port = port*10 + int(c-'0')
	}

	return port
}
