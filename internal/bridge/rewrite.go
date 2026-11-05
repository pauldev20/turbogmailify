package bridge

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decode non-UTF-8 encoded words
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

const xOriginal = "X-Bridge-Original-"

// errMalformed marks messages whose headers cannot be rewritten.
var errMalformed = errors.New("malformed message")

// preservedHeaders are copied to X-Bridge-Original-<name> before rewriting.
var preservedHeaders = []string{"From", "Reply-To", "To", "Cc", "Mail-Followup-To", "Mail-Reply-To", "List-Unsubscribe", "List-Post"}

// tokenizer maps a real address to its token address.
type tokenizer func(*mail.Address) (*mail.Address, error)

// rewrite returns raw with its reply paths and recipients replaced by token
// addresses. All other header fields and the body are kept byte for byte.
func rewrite(raw []byte, mode string, tokenize tokenizer) ([]byte, error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	th, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("%w: read header: %v", errMalformed, err)
	}
	h := mail.Header{Header: message.Header{Header: th}}

	// Bridge headers from outside are untrusted; replace them with our own.
	for _, k := range preservedHeaders {
		h.Del(xOriginal + k)
	}
	for _, k := range preservedHeaders {
		if v := h.Get(k); v != "" {
			h.Set(xOriginal+k, v)
		}
	}

	// Every reply must reach a token address, so an unusable reply path is
	// fatal. An unusable Reply-To falls back to From.
	replyTo, err := h.AddressList("Reply-To")
	if err != nil || len(replyTo) == 0 {
		if replyTo, err = h.AddressList("From"); err != nil || len(replyTo) == 0 {
			return nil, fmt.Errorf("%w: no usable Reply-To or From: %v", errMalformed, err)
		}
	}
	if replyTo, err = tokenizeAll(replyTo, tokenize); err != nil {
		return nil, err
	}
	h.SetAddressList("Reply-To", replyTo)

	// Mail-Followup-To and Mail-Reply-To steer reply-all and reply in some
	// IMAP clients (Thunderbird, mutt).
	keys := []string{"To", "Cc", "Mail-Followup-To", "Mail-Reply-To"}
	if mode == RewriteFull {
		keys = append(keys, "From")
	}
	for _, k := range keys {
		addrs, err := h.AddressList(k)
		if err != nil {
			// Unparseable recipients would leak via reply-all; drop them.
			h.Del(k)
			continue
		}
		if addrs, err = tokenizeAll(addrs, tokenize); err != nil {
			return nil, err
		}
		h.SetAddressList(k, addrs)
	}

	// Gmail's unsubscribe button and clients' reply-to-list use these.
	for _, k := range []string{"List-Unsubscribe", "List-Post"} {
		v := h.Get(k)
		if v == "" {
			continue
		}
		if v, err = rewriteMailtoList(v, tokenize); err != nil {
			return nil, err
		}
		if v == "" {
			h.Del(k)
		} else {
			h.Set(k, v)
		}
	}

	var out bytes.Buffer
	if err := textproto.WriteHeader(&out, h.Header.Header); err != nil {
		return nil, err
	}
	if _, err := io.Copy(&out, br); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func tokenizeAll(addrs []*mail.Address, tokenize tokenizer) ([]*mail.Address, error) {
	out := make([]*mail.Address, 0, len(addrs))
	for _, a := range addrs {
		t, err := tokenize(a)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

var angleBracketed = regexp.MustCompile(`<[^>]*>`)

// rewriteMailtoList points the mailto: entries of a list header at token
// addresses. Unparseable mailto: entries are dropped; other URIs are kept.
func rewriteMailtoList(v string, tokenize tokenizer) (string, error) {
	var kept []string
	for _, m := range angleBracketed.FindAllString(v, -1) {
		u, err := url.Parse(strings.TrimSpace(m[1 : len(m)-1]))
		if err != nil {
			continue
		}
		if !strings.EqualFold(u.Scheme, "mailto") {
			kept = append(kept, m)
			continue
		}
		addr, err := url.PathUnescape(u.Opaque)
		if err != nil {
			continue
		}
		a, err := mail.ParseAddress(addr)
		if err != nil {
			continue
		}
		t, err := tokenize(&mail.Address{Address: a.Address})
		if err != nil {
			return "", err
		}
		u.Opaque = t.Address
		// Extra recipients in the query would be mailed directly from Gmail.
		// Other parameters are kept exactly as written.
		var params []string
		for _, kv := range strings.Split(u.RawQuery, "&") {
			k, _, _ := strings.Cut(kv, "=")
			k, _ = url.QueryUnescape(k)
			switch strings.ToLower(k) {
			case "to", "cc", "bcc":
				continue
			}
			params = append(params, kv)
		}
		u.RawQuery = strings.Join(params, "&")
		kept = append(kept, "<"+u.String()+">")
	}
	return strings.Join(kept, ", "), nil
}
