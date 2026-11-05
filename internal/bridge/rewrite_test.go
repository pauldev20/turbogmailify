package bridge

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-message/mail"
)

// fakeTokenize maps addresses to readable stand-ins for golden comparisons.
func fakeTokenize(a *mail.Address) (*mail.Address, error) {
	local, _, _ := strings.Cut(a.Address, "@")
	name := a.Name
	if name == "" {
		name = a.Address
	}
	return &mail.Address{Name: name + " (via T)", Address: "me+t." + local + "@gmail.com"}, nil
}

func rewriteString(t *testing.T, mode, in string) (string, mail.Header) {
	t.Helper()
	out, err := rewrite(crlf(in), mode, fakeTokenize)
	if err != nil {
		t.Fatal(err)
	}
	h, err := readHeader(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), h
}

func addrList(t *testing.T, h mail.Header, k string) string {
	t.Helper()
	l, err := h.AddressList(k)
	if err != nil {
		t.Fatalf("%s: %v", k, err)
	}
	var s []string
	for _, a := range l {
		s = append(s, a.String())
	}
	return strings.Join(s, ", ")
}

const simpleMail = `Received: from mx.example.com
From: Max Mustermann <max@example.com>
To: Paul <old1@aol.com>, carl@example.com
Cc: "Doe, Jane" <jane@example.com>
Subject: Hello
Message-ID: <abc@example.com>
In-Reply-To: <prev@example.com>
X-Bridge-Original-From: spoofed@evil.com

Body line 1
Body line 2
`

func TestRewriteReplyToMode(t *testing.T) {
	out, h := rewriteString(t, RewriteReplyTo, simpleMail)

	checks := map[string]string{
		"From":     `"Max Mustermann" <max@example.com>`,
		"Reply-To": `"Max Mustermann (via T)" <me+t.max@gmail.com>`,
		"To":       `"Paul (via T)" <me+t.old1@gmail.com>, "carl@example.com (via T)" <me+t.carl@gmail.com>`,
		"Cc":       `"Doe, Jane (via T)" <me+t.jane@gmail.com>`,
	}
	for k, want := range checks {
		if got := addrList(t, h, k); got != want {
			t.Errorf("%s = %s\nwant %s", k, got, want)
		}
	}
	if got := h.Get(xOriginal + "From"); got != "Max Mustermann <max@example.com>" {
		t.Errorf("spoofed original header survived: %q", got)
	}
	if got := h.Get(xOriginal + "To"); got != "Paul <old1@aol.com>, carl@example.com" {
		t.Errorf("original To = %q", got)
	}
	if h.Has(xOriginal + "Reply-To") {
		t.Error("no original Reply-To to preserve")
	}
	for _, k := range []string{"Message-ID", "In-Reply-To", "Subject", "Received"} {
		if h.Get(k) == "" {
			t.Errorf("%s was lost", k)
		}
	}
	if !strings.HasSuffix(out, "\r\n\r\nBody line 1\r\nBody line 2\r\n") {
		t.Errorf("body changed:\n%q", out)
	}
}

func TestRewriteKeepsExistingReplyToAndFullMode(t *testing.T) {
	in := "From: Max <max@example.com>\nReply-To: list@example.com\nTo: old1@aol.com\nSubject: x\n\nhi\n"

	_, h := rewriteString(t, RewriteReplyTo, in)
	if got := addrList(t, h, "Reply-To"); got != `"list@example.com (via T)" <me+t.list@gmail.com>` {
		t.Errorf("Reply-To = %s", got)
	}
	if got := addrList(t, h, "From"); got != `"Max" <max@example.com>` {
		t.Errorf("reply-to mode must keep From, got %s", got)
	}

	_, h = rewriteString(t, RewriteFull, in)
	if got := addrList(t, h, "From"); got != `"Max (via T)" <me+t.max@gmail.com>` {
		t.Errorf("full mode From = %s", got)
	}
	if got := h.Get(xOriginal + "Reply-To"); got != "list@example.com" {
		t.Errorf("original Reply-To = %q", got)
	}
}

func TestRewriteEncodedNames(t *testing.T) {
	in := "From: =?iso-8859-1?q?J=FCrgen_M=FCller?= <j@example.de>\nTo: old1@aol.com\n\nx\n"
	_, h := rewriteString(t, RewriteReplyTo, in)
	l, _ := h.AddressList("Reply-To")
	if len(l) != 1 || l[0].Name != "Jürgen Müller (via T)" {
		t.Errorf("Reply-To = %+v", l)
	}
}

func TestRewriteListUnsubscribe(t *testing.T) {
	in := "From: news@example.com\nTo: old1@aol.com\n" +
		"List-Unsubscribe: <https://example.com/u?x=1>, <mailto:unsub@example.com?subject=stop>, <mailto:not an address>\n\nx\n"
	_, h := rewriteString(t, RewriteReplyTo, in)
	want := "<https://example.com/u?x=1>, <mailto:me+t.unsub@gmail.com?subject=stop>"
	if got := h.Get("List-Unsubscribe"); got != want {
		t.Errorf("List-Unsubscribe = %s\nwant %s", got, want)
	}
}

func TestRewriteClientReplyHeaders(t *testing.T) {
	in := "From: a@example.com\nReply-To: <<<broken\nMail-Followup-To: list@example.com, a@example.com\n" +
		"List-Post: <mailto:list@example.com?CC=copy@example.com&subject=hi%20there>\nTo: old1@aol.com\n\nx\n"
	_, h := rewriteString(t, RewriteReplyTo, in)
	checks := map[string]string{
		"Reply-To":         `"a@example.com (via T)" <me+t.a@gmail.com>`,
		"Mail-Followup-To": `"list@example.com (via T)" <me+t.list@gmail.com>, "a@example.com (via T)" <me+t.a@gmail.com>`,
	}
	for k, want := range checks {
		if got := addrList(t, h, k); got != want {
			t.Errorf("%s = %s\nwant %s", k, got, want)
		}
	}
	if got := h.Get("List-Post"); got != "<mailto:me+t.list@gmail.com?subject=hi%20there>" {
		t.Errorf("List-Post = %s", got)
	}
}

func TestRewriteDropsUnparseableRecipients(t *testing.T) {
	_, h := rewriteString(t, RewriteReplyTo, "From: a@example.com\nTo: <<<broken\nCc: c@example.com\n\nx\n")
	if h.Has("To") {
		t.Error("unparseable To must be removed")
	}
	if h.Get(xOriginal+"To") != "<<<broken" {
		t.Error("original To must be preserved")
	}
	if addrList(t, h, "Cc") == "" {
		t.Error("Cc lost")
	}
}

func TestRewriteRequiresReplyPath(t *testing.T) {
	for _, in := range []string{"To: a@example.com\n\nx\n", "From: <<<broken\n\nx\n", "Reply-To: <<<\nFrom: <<<\n\nx\n"} {
		if _, err := rewrite(crlf(in), RewriteReplyTo, fakeTokenize); err == nil {
			t.Errorf("want error for %q", in)
		}
	}
}

func TestRewritePropagatesTokenizeErrors(t *testing.T) {
	boom := errors.New("boom")
	_, err := rewrite(crlf(simpleMail), RewriteReplyTo, func(*mail.Address) (*mail.Address, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestRewriteKeepsSignedBodyBytes(t *testing.T) {
	body := "--b\r\nContent-Type: text/plain\r\n\r\nsigned text\r\n--b\r\nContent-Type: application/pkcs7-signature\r\n\r\nSIG\r\n--b--\r\n"
	in := "From: a@example.com\r\nTo: old1@aol.com\r\nContent-Type: multipart/signed; boundary=b\r\n\r\n" + body
	out, err := rewrite([]byte(in), RewriteReplyTo, fakeTokenize)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(out, []byte("\r\n\r\n"+body)) {
		t.Errorf("signed body changed:\n%s", out)
	}
}
