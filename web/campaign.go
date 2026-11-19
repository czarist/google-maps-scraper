package web

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"
)

const (
	// MailStartTLS is SMTP with STARTTLS, usually port 587.
	MailStartTLS MailEncryption = "starttls"
	// MailSSL is SMTP wrapped in TLS from the first byte, usually port 465.
	MailSSL MailEncryption = "ssl"
	// MailPlain is SMTP without encryption.
	MailPlain MailEncryption = "none"

	// MailFormatPlain is a text/plain message.
	MailFormatPlain MailFormat = "plain"
	// MailFormatHTML is a text/html message.
	MailFormatHTML MailFormat = "html"

	// DispatchRunning means a send is in progress.
	DispatchRunning = "running"
	// DispatchOK means the send finished and at least the attempt was recorded.
	DispatchOK = "ok"
	// DispatchFailed means every message in the run failed, or the run was interrupted.
	DispatchFailed = "failed"

	// MailStatusSent is an address that should be skipped on the next run.
	MailStatusSent = "sent"
	// MailStatusFailed is an address that can be tried again.
	MailStatusFailed = "failed"
	// MailStatusUnsubscribed is an address that opted out of the campaign service.
	// It is left out of later runs and is never counted as sent.
	MailStatusUnsubscribed = "unsubscribed"

	defaultSMTPPort    = 587
	maxCampaignName    = 200
	maxMailText        = 500_000
	maxSubjectLen      = 500
	maxSMTPHostLen     = 255
	dispatchStaleAfter = 2 * time.Hour

	// mailBatchSize is how many messages go out before the longer pause.
	mailBatchSize = 10
	// intraBatchMinWait and intraBatchMaxWait are the pause between messages in one batch.
	// 9 pauses averaging 20s spread a batch of 10 over about 3 minutes.
	intraBatchMinWait = 12 * time.Second
	intraBatchMaxWait = 28 * time.Second
	// batchGapMinWait and batchGapMaxWait are the pause between batches (about one minute).
	batchGapMinWait = 60 * time.Second
	batchGapMaxWait = 75 * time.Second
)

// ErrDispatchRunning is returned when a campaign already has a send in progress.
var ErrDispatchRunning = errors.New("dispatch already running")

// ErrMailNotReady is returned when a campaign is missing the fields required to send.
var ErrMailNotReady = errors.New("email settings are incomplete")

// ErrJobNotFound is returned when a campaign action names a job that does not exist.
var ErrJobNotFound = errors.New("job not found")

// MailEncryption is how the SMTP connection is protected.
type MailEncryption string

// MailFormat is the body media type stored for one campaign.
type MailFormat string

// Campaign is a named group of jobs with its own email settings.
type Campaign struct {
	ID   string
	Name string
	// ServiceName is the sending service checked against the unsubscribe list.
	// It must be a registered MailService. Stored campaigns default to ServiceTrentin.
	ServiceName string
	CreatedAt   time.Time
}

// Validate reports whether the campaign can be stored.
func (c Campaign) Validate() error {
	name := strings.TrimSpace(c.Name)
	if c.ID == "" {
		return errors.New("missing campaign id")
	}

	if name == "" {
		return errors.New("missing campaign name")
	}

	if len(name) > maxCampaignName {
		return errors.New("campaign name is too long")
	}

	if c.ServiceName != "" && !ValidServiceName(c.ServiceName) {
		return ErrInvalidService
	}

	return nil
}

// CampaignSummary is a campaign plus the facts shown in the list.
type CampaignSummary struct {
	Campaign
	JobCount   int
	MailReady  bool
	LastStatus string
}

// CampaignRef is the campaign a job belongs to.
type CampaignRef struct {
	ID   string
	Name string
}

// MailConfig is the email dispatch setup for one campaign.
// Password is stored for this personal app and must not be written into logs or HTML.
type MailConfig struct {
	Host       string
	Port       int
	Username   string
	Password   string
	FromEmail  string
	FromName   string
	Subject    string
	Body       string
	Encryption MailEncryption
	Format     MailFormat
}

// Normalize trims fields and fills the default port and encryption.
// The password is left unchanged, including surrounding spaces.
func (c *MailConfig) Normalize() MailConfig {
	out := MailConfig{}
	if c != nil {
		out = *c
	}

	c = &out
	c.Host = strings.TrimSpace(c.Host)
	c.Username = strings.TrimSpace(c.Username)
	c.FromEmail = strings.TrimSpace(c.FromEmail)
	c.FromName = strings.ReplaceAll(strings.TrimSpace(c.FromName), "\r", "")
	c.FromName = strings.ReplaceAll(c.FromName, "\n", "")
	c.Subject = strings.TrimSpace(c.Subject)
	c.Body = strings.TrimSpace(c.Body)
	c.Encryption = MailEncryption(strings.TrimSpace(string(c.Encryption)))
	c.Format = MailFormat(strings.TrimSpace(string(c.Format)))

	if c.Port == 0 {
		c.Port = defaultSMTPPort
	}

	if c.Encryption == "" {
		c.Encryption = MailStartTLS
	}

	if c.Format != MailFormatHTML {
		c.Format = MailFormatPlain
	}

	return *c
}

// EncryptionIs reports whether the selected encryption is name.
func (c *MailConfig) EncryptionIs(name string) bool {
	return c != nil && string(c.Encryption) == name
}

// FormatIs reports whether the body format is name. An empty format is plain text.
func (c *MailConfig) FormatIs(name string) bool {
	format := MailFormatPlain
	if c != nil && c.Format != "" {
		format = c.Format
	}

	return string(format) == name
}

// ValidateShape checks types and lengths. Empty settings are allowed so a campaign
// can be saved before it is ready to send.
func (c *MailConfig) ValidateShape() error {
	cfg := c.Normalize()

	if cfg.Port < 1 || cfg.Port > 65535 {
		return errors.New("invalid SMTP port")
	}

	switch cfg.Encryption {
	case MailStartTLS, MailSSL, MailPlain:
	default:
		return errors.New("invalid encryption")
	}

	if cfg.Host != "" && !validSMTPHost(cfg.Host) {
		return errors.New("invalid SMTP host")
	}

	if len(cfg.Subject) > maxSubjectLen {
		return errors.New("subject is too long")
	}

	if len(cfg.Body) > maxMailText {
		return errors.New("body is too long")
	}

	if cfg.FromEmail != "" {
		if _, err := addressOnly(cfg.FromEmail); err != nil {
			return errors.New("invalid from address")
		}
	}

	return nil
}

// ReadyToSend reports whether a dispatch can use this config.
func (c *MailConfig) ReadyToSend() error {
	cfg := c.Normalize()

	if err := cfg.ValidateShape(); err != nil {
		return err
	}

	if cfg.Host == "" || cfg.FromEmail == "" || cfg.Subject == "" || cfg.Body == "" || cfg.Password == "" {
		return ErrMailNotReady
	}

	if _, _, err := parseMailTemplates(cfg.Subject, cfg.Body); err != nil {
		return errors.New("email template is invalid")
	}

	return nil
}

func validSMTPHost(host string) bool {
	if host == "" || len(host) > maxSMTPHostLen {
		return false
	}

	return !strings.ContainsAny(host, " \r\n\t/\\")
}

func addressOnly(raw string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil || addr.Address == "" || strings.ContainsAny(addr.Address, " \r\n") {
		return "", errors.New("invalid email address")
	}

	return addr.Address, nil
}

// Dispatch is one send run for a campaign.
type Dispatch struct {
	ID         string
	CampaignID string
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
	Sent       int
	Failed     int
	Skipped    int
	Detail     string
}

// StartedLabel is the UTC start time for the dispatch table.
func (d *Dispatch) StartedLabel() string {
	if d == nil || d.StartedAt.IsZero() {
		return ""
	}

	return d.StartedAt.UTC().Format("2006-01-02 15:04")
}

// DispatchResult is what StartDispatch tells the page.
type DispatchResult struct {
	Started bool
	Detail  string
}

// DispatchPreview counts who a dispatch would reach before it starts.
type DispatchPreview struct {
	Ready          int
	AlreadySent    int
	MissingResults int
}

// CampaignStore persists campaigns. The SQLite job repository implements it.
// MailLogEntry is the latest send result for one address of a campaign.
type MailLogEntry struct {
	Email  string
	Status string
	Error  string
	SentAt time.Time
}

// SentLabel is the UTC send time for tables.
func (e MailLogEntry) SentLabel() string {
	return e.SentAt.UTC().Format("2006-01-02 15:04")
}

type CampaignStore interface {
	CreateCampaign(context.Context, *Campaign) error
	ListCampaigns(context.Context) ([]CampaignSummary, error)
	GetCampaign(context.Context, string) (Campaign, error)
	RenameCampaign(context.Context, string, string) error
	SetCampaignService(context.Context, string, string) error
	ListServices(context.Context) ([]MailService, error)
	GetService(context.Context, string) (MailService, error)
	CreateService(context.Context, *MailService) error
	UpdateService(context.Context, *MailService) error
	// DeleteService returns ErrServiceInUse while a campaign uses the service.
	DeleteService(context.Context, string) error
	DeleteCampaign(context.Context, string) error
	AssignJob(context.Context, string, string) error
	UnassignJob(context.Context, string, string) error
	CampaignJobs(context.Context, string) ([]string, error)
	CampaignsForJobs(context.Context, []string) (map[string]CampaignRef, error)
	GetMail(context.Context, string) (MailConfig, bool, error)
	SaveMail(context.Context, string, *MailConfig) error
	// SentEmails returns addresses not to queue again: sent or unsubscribed.
	SentEmails(context.Context, string) (map[string]struct{}, error)
	RecordMail(context.Context, string, string, string, string) error
	MailLog(context.Context, string) ([]MailLogEntry, error)
	InsertDispatch(context.Context, *Dispatch) error
	UpdateDispatch(context.Context, *Dispatch) error
	ListDispatches(context.Context, string, int) ([]Dispatch, error)
	LatestRunningDispatch(context.Context, string) (Dispatch, bool, error)
}
