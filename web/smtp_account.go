package web

import (
	"context"
	"errors"
	"log"
	"strings"
	"uuid"
)

// SMTPAccount is an extra server for a campaign. It sends the same subject and
// body as the server saved on the Email tab.
type SMTPAccount struct {
	ID         string
	Host       string
	Port       int
	Username   string
	Password   string
	Encryption MailEncryption
	FromEmail  string
	FromName   string
	// PasswordSet reports a stored password. It is not saved.
	PasswordSet bool
}

// SMTPAccountStore keeps the extra SMTP servers of a campaign, in send order.
type SMTPAccountStore interface {
	ListSMTPAccounts(ctx context.Context, campaignID string) ([]SMTPAccount, error)
	InsertSMTPAccount(ctx context.Context, campaignID string, a *SMTPAccount) error
	UpdateSMTPAccount(ctx context.Context, campaignID string, a *SMTPAccount) error
	DeleteSMTPAccount(ctx context.Context, campaignID, accountID string) error
}

// Normalize trims an extra SMTP server and fills the default port and encryption.
func (a *SMTPAccount) Normalize() SMTPAccount {
	out := SMTPAccount{}
	if a != nil {
		out = *a
	}

	out.ID = strings.TrimSpace(out.ID)
	out.Host = strings.TrimSpace(out.Host)
	out.Username = strings.TrimSpace(out.Username)
	out.FromEmail = strings.TrimSpace(out.FromEmail)
	out.FromName = strings.ReplaceAll(strings.TrimSpace(out.FromName), "\r", "")
	out.FromName = strings.ReplaceAll(out.FromName, "\n", "")
	out.Encryption = MailEncryption(strings.TrimSpace(string(out.Encryption)))

	if out.Port == 0 {
		out.Port = defaultSMTPPort
	}

	if out.Encryption == "" {
		out.Encryption = MailStartTLS
	}

	// Port 465 speaks TLS immediately. STARTTLS there waits for a banner that
	// never comes and the read times out.
	if out.Port == 465 && out.Encryption == MailStartTLS {
		out.Encryption = MailSSL
	}

	return out
}

// Validate checks an extra SMTP server that is about to be saved.
// The password and the from address are required. The message is not stored here.
func (a SMTPAccount) Validate() error {
	cfg := a.Normalize()

	if cfg.Host == "" {
		return errors.New("SMTP host is required")
	}

	if !validSMTPHost(cfg.Host) {
		return errors.New("invalid SMTP host")
	}

	if cfg.Port < 1 || cfg.Port > 65535 {
		return errors.New("invalid SMTP port")
	}

	switch cfg.Encryption {
	case MailStartTLS, MailSSL, MailPlain:
	default:
		return errors.New("invalid encryption")
	}

	if cfg.Password == "" {
		return errors.New("password is required")
	}

	if cfg.FromEmail == "" {
		return errors.New("from address is required")
	}

	if _, err := addressOnly(cfg.FromEmail); err != nil {
		return errors.New("invalid from address")
	}

	return nil
}

// mailAccountAt picks the server for message index. Index 0 is the campaign
// server. The following messages walk the extra servers, then start again.
// Subject and body stay on the campaign message.
func mailAccountAt(primary MailConfig, extras []SMTPAccount, index int) MailConfig {
	if index < 0 {
		index = 0
	}

	if len(extras) == 0 {
		return primary
	}

	slot := index % (1 + len(extras))
	if slot == 0 {
		return primary
	}

	extra := extras[slot-1]
	out := primary
	out.Host = extra.Host
	out.Port = extra.Port
	out.Username = extra.Username
	out.Password = extra.Password
	out.Encryption = extra.Encryption
	out.FromEmail = extra.FromEmail
	out.FromName = extra.FromName

	return out
}

// liveAccounts is one entry per sender that dead does not reject, in send order.
// A wave sends one email from each of them at the same time.
func liveAccounts(primary MailConfig, extras []SMTPAccount, dead func(MailConfig) bool) []MailConfig {
	n := 1 + len(extras)
	out := make([]MailConfig, 0, n)

	for i := range n {
		cfg := mailAccountAt(primary, extras, i)
		if dead != nil && dead(cfg) {
			continue
		}

		out = append(out, cfg)
	}

	return out
}

// senderSlot is the position of account in the full send order.
func senderSlot(primary MailConfig, extras []SMTPAccount, account MailConfig) int {
	n := 1 + len(extras)
	key := senderKey(account)

	for i := range n {
		if senderKey(mailAccountAt(primary, extras, i)) == key {
			return i
		}
	}

	return 0
}

// nextLiveAccount walks the send order from start and returns the first server
// that dead does not reject. It reports false when every server is rejected.
func nextLiveAccount(primary MailConfig, extras []SMTPAccount, start int, dead func(MailConfig) bool) (MailConfig, bool) {
	n := 1 + len(extras)

	for step := 0; step < n; step++ {
		cfg := mailAccountAt(primary, extras, start+step)
		if dead == nil || !dead(cfg) {
			return cfg, true
		}
	}

	return MailConfig{}, false
}

func senderKey(cfg MailConfig) string {
	return strings.ToLower(strings.TrimSpace(cfg.Host)) + "\n" + strings.ToLower(strings.TrimSpace(cfg.Username))
}

func (s *Service) smtpStore() (SMTPAccountStore, error) {
	store, ok := s.repo.(SMTPAccountStore)
	if !ok || store == nil {
		return nil, errCampaignsUnavailable
	}

	return store, nil
}

// ListSMTPAccounts returns the extra servers in send order. Passwords are included.
func (s *Service) ListSMTPAccounts(ctx context.Context, campaignID string) ([]SMTPAccount, error) {
	store, err := s.smtpStore()
	if err != nil {
		return nil, err
	}

	return store.ListSMTPAccounts(ctx, campaignID)
}

// AddSMTPAccount stores one more server for the same campaign message.
func (s *Service) AddSMTPAccount(ctx context.Context, campaignID string, a *SMTPAccount) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	if _, err := store.GetCampaign(ctx, campaignID); err != nil {
		return err
	}

	accounts, err := s.smtpStore()
	if err != nil {
		return err
	}

	normalized := a.Normalize()
	if err := normalized.Validate(); err != nil {
		return err
	}

	addr, err := addressOnly(normalized.FromEmail)
	if err != nil {
		return errors.New("invalid from address")
	}

	normalized.FromEmail = addr
	if normalized.ID == "" {
		normalized.ID = uuid.NewV4().String()
	}

	if err := accounts.InsertSMTPAccount(ctx, campaignID, &normalized); err != nil {
		return err
	}

	s.mailSettingsChanged(campaignID)

	return nil
}

// UpdateSMTPAccount saves one extra sender. A blank password keeps the stored one.
// The from address and the from name are this sender's own.
func (s *Service) UpdateSMTPAccount(ctx context.Context, campaignID string, a *SMTPAccount) error {
	if _, err := s.campaignStore(); err != nil {
		return err
	}

	accounts, err := s.smtpStore()
	if err != nil {
		return err
	}

	list, err := accounts.ListSMTPAccounts(ctx, campaignID)
	if err != nil {
		return err
	}

	var current *SMTPAccount

	for i := range list {
		if list[i].ID == strings.TrimSpace(a.ID) {
			current = &list[i]

			break
		}
	}

	if current == nil {
		return errors.New("SMTP server not found")
	}

	normalized := a.Normalize()
	normalized.ID = current.ID

	if normalized.Password == "" {
		normalized.Password = current.Password
	}

	if err := normalized.Validate(); err != nil {
		return err
	}

	addr, err := addressOnly(normalized.FromEmail)
	if err != nil {
		return errors.New("invalid from address")
	}

	normalized.FromEmail = addr

	if err := accounts.UpdateSMTPAccount(ctx, campaignID, &normalized); err != nil {
		return err
	}

	s.mailSettingsChanged(campaignID)

	return nil
}

// SavePrimarySender stores the first sender. Subject and body stay on the message.
// A blank password keeps the stored one.
func (s *Service) SavePrimarySender(ctx context.Context, campaignID string, a *SMTPAccount) error {
	store, err := s.campaignStore()
	if err != nil {
		return err
	}

	if _, err := store.GetCampaign(ctx, campaignID); err != nil {
		return err
	}

	existing, ok, err := store.GetMail(ctx, campaignID)
	if err != nil {
		return err
	}

	normalized := a.Normalize()
	if normalized.Password == "" && ok {
		normalized.Password = existing.Password
	}

	if err := normalized.Validate(); err != nil {
		return err
	}

	addr, err := addressOnly(normalized.FromEmail)
	if err != nil {
		return errors.New("invalid from address")
	}

	normalized.FromEmail = addr

	if !ok {
		existing = MailConfig{Port: defaultSMTPPort, Encryption: MailStartTLS, Format: MailFormatPlain}
	}

	existing.Host = normalized.Host
	existing.Port = normalized.Port
	existing.Username = normalized.Username
	existing.Password = normalized.Password
	existing.Encryption = normalized.Encryption
	existing.FromEmail = normalized.FromEmail
	existing.FromName = normalized.FromName

	if err := store.SaveMail(ctx, campaignID, &existing); err != nil {
		return err
	}

	s.mailSettingsChanged(campaignID)

	return nil
}

// SenderMail is one sender's connection and identity, including the password.
// senderID "primary" is the first sender.
func (s *Service) SenderMail(ctx context.Context, campaignID, senderID string) (MailConfig, error) {
	store, err := s.campaignStore()
	if err != nil {
		return MailConfig{}, err
	}

	if _, err := store.GetCampaign(ctx, campaignID); err != nil {
		return MailConfig{}, err
	}

	senderID = strings.TrimSpace(senderID)
	if senderID == "" || senderID == "primary" {
		cfg, ok, err := store.GetMail(ctx, campaignID)
		if err != nil {
			return MailConfig{}, err
		}

		if !ok {
			return MailConfig{}, errors.New("SMTP host is required")
		}

		return cfg.Normalize(), nil
	}

	list, err := s.ListSMTPAccounts(ctx, campaignID)
	if err != nil {
		return MailConfig{}, err
	}

	for i := range list {
		if list[i].ID != senderID {
			continue
		}

		a := list[i]
		cfg := MailConfig{
			Host:       a.Host,
			Port:       a.Port,
			Username:   a.Username,
			Password:   a.Password,
			Encryption: a.Encryption,
			FromEmail:  a.FromEmail,
			FromName:   a.FromName,
		}

		return cfg.Normalize(), nil
	}

	return MailConfig{}, errors.New("SMTP server not found")
}

// CheckSender dials one saved sender and does not send mail.
func (s *Service) CheckSender(ctx context.Context, campaignID, senderID string) error {
	cfg, err := s.SenderMail(ctx, campaignID, senderID)
	if err != nil {
		return err
	}

	if cfg.Host == "" {
		return errors.New("SMTP host is required")
	}

	if cfg.Username != "" && cfg.Password == "" {
		return errors.New("password is required to test the login")
	}

	return CheckSMTP(ctx, &cfg)
}

// DeleteSMTPAccount removes one extra server. The server on the Email tab stays.
func (s *Service) DeleteSMTPAccount(ctx context.Context, campaignID, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("SMTP server not found")
	}

	accounts, err := s.smtpStore()
	if err != nil {
		return err
	}

	if err := accounts.DeleteSMTPAccount(ctx, campaignID, accountID); err != nil {
		return err
	}

	s.mailSettingsChanged(campaignID)

	return nil
}

// smtpAccounts is the extra servers for a running send. A store without them, or
// a read error, leaves the run on the campaign server.
func (s *Service) smtpAccounts(ctx context.Context, campaignID string) []SMTPAccount {
	store, ok := s.repo.(SMTPAccountStore)
	if !ok || store == nil {
		return nil
	}

	list, err := store.ListSMTPAccounts(ctx, campaignID)
	if err != nil {
		log.Printf("smtp accounts %s: %v", campaignID, err) //nolint:gosec // G706: campaignID is a parsed UUID.

		return nil
	}

	return list
}
