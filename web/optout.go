package web

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
)

const (
	// ServiceTrentin is the default sending service of a campaign.
	ServiceTrentin = "trentin"

	// unsubscribeMarker is replaced with the unsubscribe link in every message.
	unsubscribeMarker = "{{UNSUBSCRIBE_URL}}"
	// unsubscribeReasonPrefix is the skip reason stored for opted-out addresses.
	unsubscribeReasonPrefix = "descadastro:"
	// maxServiceName matches servicos.servico_nome VARCHAR(64) on the website.
	maxServiceName  = 64
	maxServiceLabel = 120
)

// DefaultServices are created with the database. More can be added on the campaigns page.
var DefaultServices = []MailService{
	{Name: ServiceTrentin, Label: "Trentin"},
	{Name: "splitquota", Label: "Splitquota"},
	{Name: "caderna", Label: "Caderna"},
}

// MailService is a sending service. Name must equal servicos.servico_nome on the website.
type MailService struct {
	Name      string
	Label     string
	Campaigns int
}

// ErrInvalidService means a campaign has no service or one that is not registered.
// The message is not sent.
var ErrInvalidService = errors.New("campaign service is missing or invalid")

// ErrServiceInUse is returned when deleting a service that campaigns still use.
var ErrServiceInUse = errors.New("service is used by a campaign")

// ErrServiceExists is returned when creating a service name twice.
var ErrServiceExists = errors.New("service already exists")

// ErrOptOutUnavailable means the unsubscribe database could not answer.
// The message is not sent and stays eligible for the next run.
var ErrOptOutUnavailable = errors.New("unsubscribe check failed")

// errUnsubscribeLink means the unsubscribe base URL is not usable. Nothing is sent.
var errUnsubscribeLink = errors.New("invalid unsubscribe base URL")

// ErrUnsubscribed means the recipient opted out of the campaign service.
type ErrUnsubscribed struct {
	Service string
}

func (e ErrUnsubscribed) Error() string {
	return unsubscribeReasonPrefix + e.Service
}

// OptOutChecker answers whether an address opted out of one service.
// email is already normalized with NormalizeOptOutEmail.
type OptOutChecker interface {
	OptedOut(ctx context.Context, email, service string) (bool, error)
	// ServiceExists reports whether the website knows the service name.
	ServiceExists(ctx context.Context, service string) (bool, error)
}

// OutgoingMail is one rendered message ready for the mail provider.
type OutgoingMail struct {
	To              string
	Subject         string
	Body            string
	ListUnsubscribe string
}

// NormalizeOptOutEmail applies the same rule as the unsubscribe site: trim and lowercase.
func NormalizeOptOutEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidServiceName reports whether name has the shape of a service name:
// 1 to 64 lowercase letters, digits, dash, or underscore.
// Whether the service is registered is checked against the campaign store.
func ValidServiceName(name string) bool {
	if name == "" || len(name) > maxServiceName {
		return false
	}

	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}

	return true
}

// UnsubscribeURL returns base with the servico query parameter set.
func UnsubscribeURL(base, service string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errUnsubscribeLink
	}

	q := u.Query()
	q.Set("servico", service)
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// withUnsubscribeLink replaces the marker or appends a short footer with link.
func withUnsubscribeLink(body, link string, format MailFormat) string {
	if format == MailFormatHTML {
		escaped := html.EscapeString(link)
		if strings.Contains(body, unsubscribeMarker) {
			return strings.ReplaceAll(body, unsubscribeMarker, escaped)
		}

		footer := `<p style="font-size:12px;color:#888">Não quer mais receber estes e-mails? <a href="` +
			escaped + `">Descadastre-se</a>.</p>`

		idx := strings.LastIndex(strings.ToLower(body), "</body>")
		if idx < 0 {
			return body + "\n" + footer
		}

		return body[:idx] + footer + "\n" + body[idx:]
	}

	if strings.Contains(body, unsubscribeMarker) {
		return strings.ReplaceAll(body, unsubscribeMarker, link)
	}

	return strings.TrimRight(body, "\r\n") + "\n\n--\nNão quer mais receber estes e-mails? Descadastre-se: " + link + "\n"
}

// SetOptOut sets the unsubscribe database and the base of the unsubscribe link.
// Without a checker every send is refused.
func (s *Service) SetOptOut(checker OptOutChecker, unsubscribeBase string) {
	s.optOut = checker
	s.unsubscribeBase = unsubscribeBase
}

// deliver is the only path to the mail provider. It refuses to send when the
// service is invalid, when the unsubscribe database cannot answer, or when the
// recipient opted out of the service. It adds the unsubscribe link and header.
func (s *Service) deliver(ctx context.Context, cfg *MailConfig, service string, msg *OutgoingMail, send MailSender) error {
	if !ValidServiceName(service) {
		return ErrInvalidService
	}

	link, err := UnsubscribeURL(s.unsubscribeBase, service)
	if err != nil {
		return err
	}

	if s.optOut == nil {
		return fmt.Errorf("%w: unsubscribe database is not configured", ErrOptOutUnavailable)
	}

	out, err := s.optOut.OptedOut(ctx, NormalizeOptOutEmail(msg.To), service)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrOptOutUnavailable, err)
	}

	if out {
		return ErrUnsubscribed{Service: service}
	}

	final := *msg
	final.Body = withUnsubscribeLink(msg.Body, link, cfg.Normalize().Format)
	final.ListUnsubscribe = link

	return send(ctx, cfg, &final)
}
