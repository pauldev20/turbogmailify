package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message/mail"
	"github.com/pauldev20/turbogmailbridge/internal/mocks"
)

type sent struct {
	server, from string
	to           []string
	msg          []byte
}

// fakeSender records submissions; errs are returned one per call, in order.
type fakeSender struct {
	sent []sent
	errs []error
}

func (s *fakeSender) Send(server Smtp, from string, to []string, msg []byte) error {
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		if err != nil {
			return err
		}
	}
	s.sent = append(s.sent, sent{server.Address, from, to, msg})
	return nil
}

type testEnv struct {
	b      *Bridge
	gm     *mocks.FakeGmail
	sender *fakeSender
	notes  []string // notification subjects
}

func testAccounts() []Account {
	return []Account{
		{Alias: "a1", DisplayTag: "AOL1", SendAs: "Paul Geeser <old1@aol.com>", GmailLabel: "Label_1",
			Smtp: Smtp{Address: "smtp.aol.com:465", Security: SecurityTLS, Username: "old1@aol.com", Password: "p1"}},
		{Alias: "a2", DisplayTag: "AOL2", SendAs: "Paul Geeser <old2@aol.com>",
			Smtp: Smtp{Address: "smtp2.aol.com:587", Security: SecurityStartTLS, Username: "old2@aol.com", Password: "p2"}},
	}
}

func newTestEnv(t *testing.T, edit func(*Config)) *testEnv {
	t.Helper()
	cfg := Config{GmailAddress: "paul@gmail.com", StateDir: t.TempDir(), DryRun: new(false)}
	if edit != nil {
		edit(&cfg)
	}
	env := &testEnv{gm: mocks.NewFakeGmail(), sender: &fakeSender{}}
	b, err := New(cfg, testAccounts(), env.gm, env.sender, func(subject, _ string) {
		env.notes = append(env.notes, subject)
	})
	if err != nil {
		t.Fatal(err)
	}
	b.retryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { b.Close() })
	env.b = b
	return env
}

// token returns the token address the bridge hands out for addr under alias.
func (e *testEnv) token(t *testing.T, alias, addr string) string {
	t.Helper()
	a, err := e.b.tokenize(e.b.accounts[alias], &mail.Address{Address: addr})
	if err != nil {
		t.Fatal(err)
	}
	return a.Address
}

func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

func mustContain(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("missing %q in:\n%s", sub, s)
		}
	}
}

func mustNotContain(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Errorf("unexpected %q in:\n%s", sub, s)
		}
	}
}

func TestNewValidates(t *testing.T) {
	base := func() (Config, []Account) {
		return Config{GmailAddress: "paul@gmail.com", StateDir: t.TempDir()}, testAccounts()
	}
	cases := map[string]func(*Config, []Account) []Account{
		"bad gmail":     func(c *Config, a []Account) []Account { c.GmailAddress = "nope"; return a },
		"plus gmail":    func(c *Config, a []Account) []Account { c.GmailAddress = "paul+x@gmail.com"; return a },
		"no state dir":  func(c *Config, a []Account) []Account { c.StateDir = ""; return a },
		"bad mode":      func(c *Config, a []Account) []Account { c.RewriteMode = "both"; return a },
		"dup alias":     func(_ *Config, a []Account) []Account { a[1].Alias = "a1"; return a },
		"bad alias":     func(_ *Config, a []Account) []Account { a[0].Alias = "A-1"; return a },
		"no tag":        func(_ *Config, a []Account) []Account { a[0].DisplayTag = " "; return a },
		"bad send as":   func(_ *Config, a []Account) []Account { a[0].SendAs = "x"; return a },
		"bad security":  func(_ *Config, a []Account) []Account { a[0].Smtp.Security = "none"; return a },
		"no smtp creds": func(_ *Config, a []Account) []Account { a[0].Smtp.Password = ""; return a },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, accts := base()
			accts = edit(&cfg, accts)
			if b, err := New(cfg, accts, mocks.NewFakeGmail(), &fakeSender{}, nil); err == nil {
				b.Close()
				t.Fatal("want error")
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	var c Config
	c.setDefaults()
	if c.RewriteMode != RewriteReplyTo || c.PollInterval.Duration != 3*time.Second || c.MaxSendsPerHour != 50 ||
		c.LabelPrefix != "Bridge" || !*c.DryRun {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestDurationUnmarshal(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("1m30s")); err != nil || d.Duration != 90*time.Second {
		t.Fatalf("got %v, %v", d, err)
	}
}
