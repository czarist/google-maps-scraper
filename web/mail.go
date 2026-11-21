package web

import (
	"context"
	"crypto/tls"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/gosom/google-maps-scraper/gmaps"
)

// MailVars is the only data a campaign template can read.
type MailVars struct {
	Title    string
	Email    string
	Phone    string
	Website  string
	Address  string
	Category string
}

// Recipient is one address taken from a job result.
type Recipient struct {
	JobID    string
	JobName  string
	Email    string
	Title    string
	Phone    string
	Website  string
	Address  string
	Category string
}

// Vars returns the template data for this recipient.
func (r *Recipient) Vars() MailVars {
	if r == nil {
		return MailVars{}
	}

	return MailVars{
		Title:    r.Title,
		Email:    r.Email,
		Phone:    r.Phone,
		Website:  r.Website,
		Address:  r.Address,
		Category: r.Category,
	}
}

// MailSender delivers one message. Only Service.deliver calls it, after the
// unsubscribe check, so every send path goes through that check.
type MailSender func(ctx context.Context, cfg *MailConfig, msg *OutgoingMail) error

// mailTemplateFuncs lets {{UNSUBSCRIBE_URL}} pass through rendering unchanged.
// Service.deliver replaces it with the campaign unsubscribe link.
var mailTemplateFuncs = template.FuncMap{
	"UNSUBSCRIBE_URL": func() string { return unsubscribeMarker },
}

type renderedMail struct {
	To      string
	Subject string
	Body    string
}

func parseMailTemplates(subject, body string) (subjectT, bodyT *template.Template, err error) {
	subjectT, err = template.New("subject").Funcs(mailTemplateFuncs).Option("missingkey=error").Parse(subject) //nolint:gosec // G708: the campaign owner writes this template. Execution only receives MailVars.
	if err != nil {
		return nil, nil, err
	}

	bodyT, err = template.New("body").Funcs(mailTemplateFuncs).Option("missingkey=error").Parse(body) //nolint:gosec // G708: the campaign owner writes this template. Execution only receives MailVars.
	if err != nil {
		return nil, nil, err
	}

	return subjectT, bodyT, nil
}

func renderMail(subjectT, bodyT *template.Template, vars *MailVars) (subject, body string, err error) {
	subject, err = executeTemplate(subjectT, vars)
	if err != nil {
		return "", "", err
	}

	subject = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}

		return r
	}, subject)
	if strings.TrimSpace(subject) == "" {
		return "", "", errors.New("subject is empty")
	}

	body, err = executeTemplate(bodyT, vars)
	if err != nil {
		return "", "", err
	}

	return subject, body, nil
}

func executeTemplate(tmpl *template.Template, vars *MailVars) (string, error) {
	var buf strings.Builder
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func prepareMessages(cfg *MailConfig, recs []Recipient) ([]renderedMail, int, error) {
	subjectT, bodyT, err := parseMailTemplates(cfg.Subject, cfg.Body)
	if err != nil {
		return nil, 0, errors.New("email template is invalid")
	}

	msgs := make([]renderedMail, 0, len(recs))
	failed := 0

	for i := range recs {
		vars := recs[i].Vars()

		subject, body, err := renderMail(subjectT, bodyT, &vars)
		if err != nil {
			failed++

			continue
		}

		msgs = append(msgs, renderedMail{To: recs[i].Email, Subject: subject, Body: body})
	}

	return msgs, failed, nil
}

// recipientsFromCSV reads places from a job CSV and returns one recipient per address.
func recipientsFromCSV(jobID, jobName string, r io.Reader) ([]Recipient, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true

	header, err := reader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}

		return nil, err
	}

	col := make(map[string]int, len(header))
	for i, name := range header {
		col[strings.ToLower(strings.TrimSpace(name))] = i
	}

	if _, ok := col["emails"]; !ok {
		return nil, nil
	}

	var recs []Recipient

	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err
		}

		cell := csvCell(row, col, "emails")
		for _, email := range parseEmailCell(cell) {
			recs = append(recs, Recipient{
				JobID:    jobID,
				JobName:  jobName,
				Email:    email,
				Title:    csvCell(row, col, "title"),
				Phone:    csvCell(row, col, "phone"),
				Website:  csvCell(row, col, "website"),
				Address:  csvCell(row, col, "address"),
				Category: csvCell(row, col, "category"),
			})
		}
	}

	return recs, nil
}

func csvCell(row []string, col map[string]int, name string) string {
	idx, ok := col[name]
	if !ok || idx >= len(row) {
		return ""
	}

	return strings.TrimSpace(row[idx])
}

func parseEmailCell(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	parts := splitEmailCell(raw)
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))

	for _, part := range parts {
		addr, err := addressOnly(part)
		if err != nil {
			continue
		}

		// Older CSVs may hold "99545-3421contato@..." from before the scraper fixed it.
		addr = gmaps.StripPhonePrefix(addr)

		key := strings.ToLower(addr)
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		out = append(out, addr)
	}

	return out
}

func splitEmailCell(raw string) []string {
	if strings.HasPrefix(raw, "[") {
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err == nil {
			return list
		}
	}

	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';'
	})
}

// SendSMTP delivers one message with the campaign SMTP settings.
func SendSMTP(ctx context.Context, cfg *MailConfig, out *OutgoingMail) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	normalized := cfg.Normalize()
	cfg = &normalized

	from, err := addressOnly(cfg.FromEmail)
	if err != nil {
		return err
	}

	toAddr, err := addressOnly(out.To)
	if err != nil {
		return err
	}

	msg := buildMessage(cfg.FromName, from, toAddr, out, cfg.Format)

	return deliverSMTP(ctx, cfg, from, toAddr, msg)
}

// CheckSMTP opens the server, upgrades encryption, and logs in when a username is set.
// It does not send a message.
func CheckSMTP(ctx context.Context, cfg *MailConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	normalized := cfg.Normalize()
	cfg = &normalized

	client, err := prepareSMTPClient(ctx, cfg)
	if err != nil {
		return err
	}

	defer func() {
		_ = client.Close()
	}()

	return client.Quit()
}

func buildMessage(fromName, from, to string, out *OutgoingMail, format MailFormat) string {
	fromHeader := (&mail.Address{Name: fromName, Address: from}).String()
	toHeader := (&mail.Address{Address: to}).String()
	media := "text/plain"

	if format == MailFormatHTML {
		media = "text/html"
	}

	var b strings.Builder
	b.WriteString("From: " + fromHeader + "\r\n")
	b.WriteString("To: " + toHeader + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", out.Subject) + "\r\n")

	if link := headerSafe(out.ListUnsubscribe); link != "" {
		// No List-Unsubscribe-Post: the page asks for confirmation, so one click does not unsubscribe.
		b.WriteString("List-Unsubscribe: <" + link + ">\r\n")
	}

	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: " + media + "; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalizeNewlines(out.Body))

	return b.String()
}

// headerSafe drops a value that could break out of its header line.
func headerSafe(v string) string {
	if strings.ContainsAny(v, "\r\n<>") {
		return ""
	}

	return strings.TrimSpace(v)
}

func normalizeNewlines(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")

	return strings.ReplaceAll(body, "\n", "\r\n")
}

func deliverSMTP(ctx context.Context, cfg *MailConfig, from, to, msg string) error {
	client, err := prepareSMTPClient(ctx, cfg)
	if err != nil {
		return err
	}

	defer func() {
		_ = client.Close()
	}()

	// from and to were parsed with mail.ParseAddress and reject spaces and newlines.
	if err := client.Mail(from); err != nil { //nolint:gosec // G707: address was parsed and cannot change the SMTP command
		return err
	}

	if err := client.Rcpt(to); err != nil { //nolint:gosec // G707: address was parsed and cannot change the SMTP command
		return err
	}

	writer, err := client.Data()
	if err != nil {
		return err
	}

	if _, err := io.WriteString(writer, msg); err != nil {
		_ = writer.Close()

		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}

	return client.Quit()
}

func prepareSMTPClient(ctx context.Context, cfg *MailConfig) (*smtp.Client, error) {
	client, err := dialSMTP(ctx, cfg)
	if err != nil {
		return nil, err
	}

	if cfg.Encryption == MailStartTLS && !smtpImplicitTLS(cfg) {
		tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
		if err := client.StartTLS(tlsCfg); err != nil {
			_ = client.Close()

			return nil, err
		}
	}

	if cfg.Username != "" {
		if err := client.Auth(smtpAuth(cfg)); err != nil {
			_ = client.Close()

			return nil, err
		}
	}

	return client, nil
}

func smtpAuth(cfg *MailConfig) smtp.Auth {
	if cfg.Encryption == MailPlain {
		return plainAuth{username: cfg.Username, password: cfg.Password, host: cfg.Host}
	}

	return smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
}

// smtpImplicitTLS reports a connection that must handshake TLS before SMTP.
// Port 465 is that kind of port even when the form still says STARTTLS.
func smtpImplicitTLS(cfg *MailConfig) bool {
	if cfg == nil {
		return false
	}

	return cfg.Encryption == MailSSL || (cfg.Port == 465 && cfg.Encryption == MailStartTLS)
}

func dialSMTP(ctx context.Context, cfg *MailConfig) (*smtp.Client, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: 20 * time.Second}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	if smtpImplicitTLS(cfg) {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()

			return nil, err
		}

		conn = tlsConn
	}

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	return client, nil
}

// plainAuth is AUTH PLAIN for servers the user explicitly left unencrypted.
// net/smtp refuses PlainAuth unless the connection is already TLS or local.
type plainAuth struct {
	username string
	password string
	host     string
}

func (a plainAuth) Start(server *smtp.ServerInfo) (proto string, resp []byte, err error) {
	if server.Name != a.host {
		return "", nil, errors.New("smtp host mismatch")
	}

	resp = []byte("\x00" + a.username + "\x00" + a.password)

	return "PLAIN", resp, nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected smtp challenge")
	}

	return nil, nil
}
