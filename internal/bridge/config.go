package bridge

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-message/mail"
	"google.golang.org/api/gmail/v1"
)

// Config is the top-level [Bridge] section.
type Config struct {
	GmailAddress    string
	StateDir        string // holds token.key and state.db; must be persistent
	RewriteMode     string // RewriteReplyTo or RewriteFull
	PollInterval    Duration
	MaxSendsPerHour int
	DryRun          *bool // outbound: label and log, never send; default true
	DeleteOriginal  bool  // permanently delete originals and stand-ins instead of trashing them
	LabelPrefix     string
}

// Account is the per-account [Imap.Bridge] section.
type Account struct {
	Alias           string // [a-z0-9]{1,8}, appears in token addresses
	DisplayTag      string // shown as "Name (via DisplayTag)"
	SendAs          string // From address for relayed mail, e.g. "Name <old@aol.com>"
	GmailLabel      string // optional label id added to this account's mail
	MessageIDDomain string // optional; defaults to the SendAs domain
	Smtp            Smtp
}

// Smtp is the per-account [Imap.Bridge.Smtp] section.
type Smtp struct {
	Address  string // host:port
	Security string // SecurityTLS or SecurityStartTLS
	Username string
	Password string
}

const (
	RewriteReplyTo = "reply-to"
	RewriteFull    = "full"

	SecurityTLS      = "tls"
	SecurityStartTLS = "starttls"
)

// Duration accepts strings such as "3s" in TOML and JSON.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) (err error) {
	d.Duration, err = time.ParseDuration(string(b))
	return
}

var aliasPattern = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

func (c *Config) setDefaults() {
	if c.RewriteMode == "" {
		c.RewriteMode = RewriteReplyTo
	}
	if c.PollInterval.Duration == 0 {
		c.PollInterval.Duration = 3 * time.Second
	}
	if c.MaxSendsPerHour == 0 {
		c.MaxSendsPerHour = 50
	}
	if c.DryRun == nil {
		c.DryRun = new(true)
	}
	if c.LabelPrefix == "" {
		c.LabelPrefix = "Bridge"
	}
}

func (c *Config) validate() error {
	local, _, err := splitAddress(c.GmailAddress)
	if err != nil {
		return fmt.Errorf("GmailAddress: %w", err)
	}
	if strings.Contains(local, "+") {
		return errors.New("GmailAddress must not contain a plus-address suffix")
	}
	if c.StateDir == "" {
		return errors.New("StateDir is required")
	}
	if c.RewriteMode != RewriteReplyTo && c.RewriteMode != RewriteFull {
		return fmt.Errorf("RewriteMode %q: want %q or %q", c.RewriteMode, RewriteReplyTo, RewriteFull)
	}
	if c.PollInterval.Duration < time.Second {
		return errors.New("PollInterval must be at least 1s")
	}
	if c.MaxSendsPerHour < 0 {
		return errors.New("MaxSendsPerHour must not be negative")
	}
	return nil
}

func (a *Account) validate() error {
	if !aliasPattern.MatchString(a.Alias) {
		return fmt.Errorf("Alias %q must match %s", a.Alias, aliasPattern)
	}
	if strings.TrimSpace(a.DisplayTag) == "" {
		return fmt.Errorf("%s: DisplayTag is required", a.Alias)
	}
	if _, err := mail.ParseAddress(a.SendAs); err != nil {
		return fmt.Errorf("%s: SendAs: %w", a.Alias, err)
	}
	if _, _, err := net.SplitHostPort(a.Smtp.Address); err != nil {
		return fmt.Errorf("%s: Smtp.Address: %w", a.Alias, err)
	}
	if a.Smtp.Security != SecurityTLS && a.Smtp.Security != SecurityStartTLS {
		return fmt.Errorf("%s: Smtp.Security %q: want %q or %q", a.Alias, a.Smtp.Security, SecurityTLS, SecurityStartTLS)
	}
	if a.Smtp.Username == "" || a.Smtp.Password == "" {
		return fmt.Errorf("%s: SMTP credentials are missing", a.Alias)
	}
	return nil
}

// OAuthScope returns the Gmail scope the bridge needs. Trashing requires
// gmail.modify; permanent deletion requires full mail access.
func (c *Config) OAuthScope() string {
	if c.DeleteOriginal {
		return gmail.MailGoogleComScope
	}
	return gmail.GmailModifyScope
}

// splitAddress returns the lower-cased local part and domain of a bare address.
func splitAddress(addr string) (local, domain string, err error) {
	a, err := mail.ParseAddress(addr)
	if err != nil {
		return "", "", err
	}
	local, domain, ok := strings.Cut(strings.ToLower(a.Address), "@")
	if !ok || local == "" || domain == "" {
		return "", "", fmt.Errorf("invalid address %q", addr)
	}
	return local, domain, nil
}
