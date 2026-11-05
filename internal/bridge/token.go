package bridge

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

const (
	tokenLen  = 10     // base32 chars, ~50 bits
	selfToken = "self" // stands for the account's own address
	keyLen    = 32
)

var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Tokens builds and parses token addresses of the form
// <gmailLocal>+<alias>.<token>@<gmailDomain>.
type Tokens struct {
	key           []byte
	local, domain string
	pattern       *regexp.Regexp // matches a whole bare address
}

func newTokens(key []byte, gmailAddress string) (*Tokens, error) {
	local, domain, err := splitAddress(gmailAddress)
	if err != nil {
		return nil, err
	}
	return &Tokens{
		key:     key,
		local:   local,
		domain:  domain,
		pattern: regexp.MustCompile(`^` + tokenAddressPattern(local, domain, "@") + `$`),
	}, nil
}

// tokenAddressPattern matches a token address with capture groups for alias
// and token. at is the regexp for the "@" separator.
func tokenAddressPattern(local, domain, at string) string {
	return `(?i)` + regexp.QuoteMeta(local) + `\+([a-z0-9]{1,8})\.([a-z2-7]{10}|` + selfToken + `)` + at + regexp.QuoteMeta(domain)
}

// Token is stable per (alias, address) and unguessable without the key.
func (t *Tokens) Token(alias, addr string) string {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte(alias + "\x00" + strings.ToLower(addr)))
	return strings.ToLower(tokenEncoding.EncodeToString(m.Sum(nil))[:tokenLen])
}

// Address returns the Gmail plus-address for a token.
func (t *Tokens) Address(alias, token string) string {
	return t.local + "+" + alias + "." + token + "@" + t.domain
}

// Parse reports whether addr is a token address and returns its parts.
func (t *Tokens) Parse(addr string) (alias, token string, ok bool) {
	m := t.pattern.FindStringSubmatch(addr)
	if m == nil {
		return "", "", false
	}
	return strings.ToLower(m[1]), strings.ToLower(m[2]), true
}

// loadOrCreateKey reads the HMAC key, creating it on first start. Losing it
// breaks every token address already handed out.
func loadOrCreateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != keyLen {
			return nil, fmt.Errorf("%s: want %d bytes, got %d", path, keyLen, len(key))
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	key = make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	return key, f.Close()
}
