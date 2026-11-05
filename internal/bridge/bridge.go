// Package bridge turns turbogmailify into a two-way bridge: inbound mail is
// rewritten so replies go to token addresses, and the outbound relay sends
// those replies through each source account's own SMTP server.
package bridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-message/mail"
)

// ErrPermanent marks inbound failures that retrying cannot fix.
var ErrPermanent = errors.New("permanent bridge error")

// Importer matches turbogmailify's gmailInbox interface.
type Importer interface {
	DoImport(envelope []byte, neverMarkSpam, processForCalendar bool, labels ...string) error
}

type account struct {
	Account
	self *mail.Address // parsed SendAs
	via  string        // " (via <DisplayTag>)"
}

// Bridge holds state shared by all accounts.
type Bridge struct {
	cfg      Config
	api      API
	sender   Sender
	notify   func(subject, body string)
	store    *Store
	tokens   *Tokens
	accounts map[string]*account // by alias

	textToken *regexp.Regexp   // token address inside body text
	viaNames  []*regexp.Regexp // " (via <tag>)" for every account
	leaks     []*regexp.Regexp // must never reach a recipient

	labelIDs    map[string]string // label suffix -> Gmail label id
	retryDelays []time.Duration   // SMTP backoff on transient errors
	now         func() time.Time
}

// Label suffixes under Config.LabelPrefix.
const (
	labelBlocked = "Blocked"
	labelMixed   = "Mixed"
	labelFailed  = "Failed"
	labelDryRun  = "DryRun"
)

// viaSpace matches whitespace in "(via <tag>)", including line wraps and the
// non-breaking spaces HTML editors insert.
const viaSpace = `(?:\s|\x{a0}|&nbsp;)`

// atVariants matches "@" as written in text, HTML entities and URLs.
const atVariants = `(?:@|&#0*64;|&#x0*40;|%40)`

// New validates the configuration and opens the state directory.
func New(cfg Config, accounts []Account, api API, sender Sender, notify func(subject, body string)) (*Bridge, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	b := &Bridge{
		cfg:         cfg,
		api:         api,
		sender:      sender,
		notify:      notify,
		accounts:    make(map[string]*account, len(accounts)),
		retryDelays: []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute},
		now:         time.Now,
	}
	for _, a := range accounts {
		if err := a.validate(); err != nil {
			return nil, err
		}
		if _, dup := b.accounts[a.Alias]; dup {
			return nil, fmt.Errorf("duplicate Alias %q", a.Alias)
		}
		self, _ := mail.ParseAddress(a.SendAs) // checked by validate
		b.accounts[a.Alias] = &account{Account: a, self: self, via: " (via " + a.DisplayTag + ")"}
		b.viaNames = append(b.viaNames, regexp.MustCompile(viaSpace+`*\(via`+viaSpace+`+`+regexp.QuoteMeta(a.DisplayTag)+`\)`))
	}

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(filepath.Join(cfg.StateDir, "token.key"))
	if err != nil {
		return nil, fmt.Errorf("token key: %w", err)
	}
	if b.tokens, err = newTokens(key, cfg.GmailAddress); err != nil {
		return nil, err
	}
	if b.store, err = openStore(filepath.Join(cfg.StateDir, "state.db")); err != nil {
		return nil, fmt.Errorf("state db: %w", err)
	}

	local, domain := b.tokens.local, b.tokens.domain
	b.textToken = regexp.MustCompile(tokenAddressPattern(local, domain, atVariants))
	// Own address and any plus-variant of it, not preceded by a local-part char.
	// mail.gmail.com is not a pattern: it is in every Gmail user's Message-ID,
	// so replies to them would always be blocked.
	b.leaks = append([]*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:^|[^a-z0-9._%+-])` + regexp.QuoteMeta(local) + `(?:\+[^\s@]*)?@` + regexp.QuoteMeta(domain)),
	}, b.viaNames...)
	return b, nil
}

func (b *Bridge) Close() error { return b.store.Close() }

// Inbox wraps next for one bridged account. next receives notifications
// unmodified.
func (b *Bridge) Inbox(alias string, next Importer) (*Inbox, error) {
	a, ok := b.accounts[alias]
	if !ok {
		return nil, fmt.Errorf("unknown alias %q", alias)
	}
	return &Inbox{Next: next, b: b, acct: a}, nil
}

// tokenize returns the token address for a correspondent of acct and records
// it. The account's own address maps to the self token.
func (b *Bridge) tokenize(acct *account, a *mail.Address) (*mail.Address, error) {
	name := a.Name
	if name == "" {
		name = a.Address
	}
	if strings.EqualFold(a.Address, acct.self.Address) {
		return &mail.Address{Name: name + acct.via, Address: b.tokens.Address(acct.Alias, selfToken)}, nil
	}

	tok := b.tokens.Token(acct.Alias, a.Address)
	now := b.now()
	err := b.store.PutToken(TokenRecord{
		Alias: acct.Alias, Token: tok, Address: a.Address, Name: a.Name, FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		return nil, err
	}
	return &mail.Address{Name: name + acct.via, Address: b.tokens.Address(acct.Alias, tok)}, nil
}

// resolve maps a token back to the real address of acct's correspondent.
func (b *Bridge) resolve(acct *account, token string) (*mail.Address, bool, error) {
	if token == selfToken {
		return acct.self, true, nil
	}
	r, ok, err := b.store.Token(acct.Alias, token)
	if err != nil || !ok {
		return nil, ok, err
	}
	return &mail.Address{Name: r.Name, Address: r.Address}, true, nil
}

// discard trashes or deletes a Gmail message per DeleteOriginal.
func (b *Bridge) discard(id string) error {
	if b.cfg.DeleteOriginal {
		return b.api.Delete(id)
	}
	return b.api.Trash(id)
}
