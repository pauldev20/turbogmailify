package bridge

import (
	"bytes"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
)

func testOutgoing() outgoing {
	return outgoing{
		from:  &mail.Address{Name: "Paul", Address: "old1@aol.com"},
		to:    []*mail.Address{{Name: "Max", Address: "max@example.com"}},
		msgID: "new-id@aol.com",
		mapID: func(id string) string { return id },
	}
}

func scrubSecret(s string) string { return strings.ReplaceAll(s, "secret", "SCRUBBED") }

// parts returns the decoded leaf bodies of msg keyed by media type.
func parts(t *testing.T, msg []byte) map[string]string {
	t.Helper()
	e, err := message.Read(bytes.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	err = e.Walk(func(_ []int, p *message.Entity, err error) error {
		if err != nil {
			return err
		}
		mt, _, _ := p.Header.ContentType()
		if !strings.HasPrefix(mt, "multipart/") {
			b, _ := io.ReadAll(p.Body)
			out[mt] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestComposeHeaderAllowlist(t *testing.T) {
	in := crlf(`Received: by 10.0.0.1
X-Gm-Message-State: abc
X-Google-Smtp-Source: def
From: Paul <paul@gmail.com>
To: "Max (via AOL1)" <paul+a1.xxxxxxxxxx@gmail.com>
Bcc: boss@example.com
Message-ID: <CAx@mail.gmail.com>
Date: Thu, 5 Nov 2026 10:00:00 +0100
Subject: Re: Hi
In-Reply-To: <abc@example.com>
References: <abc@example.com>
MIME-Version: 1.0
Content-Type: text/plain; charset="UTF-8"
Content-Language: de

hello
`)
	out, err := compose(in, testOutgoing(), scrubSecret)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := readHeader(out)

	var keys []string
	fields := h.Fields()
	for fields.Next() {
		keys = append(keys, fields.Key())
	}
	want := "Message-Id,To,From,Date,Subject,In-Reply-To,References,Mime-Version,Content-Type,Content-Language"
	if got := strings.Join(keys, ","); got != want {
		t.Errorf("header keys = %s", got)
	}
	if got := h.Get("Message-Id"); got != "<new-id@aol.com>" {
		t.Errorf("Message-ID = %s", got)
	}
	if got := addrList(t, h, "From"); got != `"Paul" <old1@aol.com>` {
		t.Errorf("From = %s", got)
	}
	mustContain(t, string(out), "\r\n\r\nhello\r\n")
}

func TestComposeScrubsTextOnly(t *testing.T) {
	att := base64.StdEncoding.EncodeToString([]byte("secret binary"))
	in := crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain; charset=UTF-8
Content-Transfer-Encoding: quoted-printable

a secret =C3=A4
--b
Content-Type: text/html; charset=UTF-8
Content-Transfer-Encoding: base64

` + base64.StdEncoding.EncodeToString([]byte("<b>secret</b>")) + `
--b
Content-Type: application/octet-stream
Content-Transfer-Encoding: base64

` + att + `
--b--
`)
	out, err := compose(in, testOutgoing(), scrubSecret)
	if err != nil {
		t.Fatal(err)
	}
	p := parts(t, out)
	if p["text/plain"] != "a SCRUBBED ä\r\n" && p["text/plain"] != "a SCRUBBED ä" {
		t.Errorf("text/plain = %q", p["text/plain"])
	}
	if p["text/html"] != "<b>SCRUBBED</b>" {
		t.Errorf("text/html = %q", p["text/html"])
	}
	if p["application/octet-stream"] != "secret binary" {
		t.Errorf("attachment = %q", p["application/octet-stream"])
	}
}

func TestComposeLeavesTextAttachments(t *testing.T) {
	in := crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

a secret
--b
Content-Type: text/plain
Content-Disposition: attachment; filename=notes.txt

attached secret
--b--
`)
	out, err := compose(in, testOutgoing(), scrubSecret)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(out), "a SCRUBBED", "attached secret")

	// Attachment bytes and charset stay exactly as sent.
	latin := "--b\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Disposition: attachment\r\n" +
		"Content-Transfer-Encoding: 8bit\r\n\r\nJ\xfcrgen secret\r\n--b--\r\n"
	out, err = compose([]byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n"+latin), testOutgoing(), scrubSecret)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(out), latin)

	// So do multipart attachments.
	nested := "--b\r\nContent-Type: multipart/alternative; boundary=c\r\nContent-Disposition: attachment\r\n\r\n" +
		"--c\r\nContent-Type: text/plain\r\n\r\nnested secret\r\n--c--\r\n\r\n--b--\r\n"
	out, err = compose([]byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n"+nested), testOutgoing(), scrubSecret)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(out), "nested secret")
}

func TestComposeConvertsCharsetToUTF8(t *testing.T) {
	in := crlf("Content-Type: text/plain; charset=iso-8859-1\nContent-Transfer-Encoding: quoted-printable\n\nJ=FCrgen\n")
	out, err := compose(in, testOutgoing(), scrubSecret)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(out), "charset=utf-8")

	out, err = compose(crlf("Content-Type: text/plain; charset=iso-8859-1\n\nJ\xfcrgen\n"), testOutgoing(), scrubSecret)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, string(out), "Content-Transfer-Encoding: quoted-printable", "J=C3=BCrgen")
	if got := parts(t, out)["text/plain"]; !strings.HasPrefix(got, "Jürgen") {
		t.Errorf("body = %q", got)
	}
}

func TestComposeKeepsSignedBody(t *testing.T) {
	body := "--s\r\nContent-Type: text/plain\r\n\r\nsecret\r\n--s\r\nContent-Type: application/pkcs7-signature\r\n\r\nSIG\r\n--s--\r\n"
	in := []byte("Content-Type: multipart/signed; boundary=s; protocol=\"application/pkcs7-signature\"\r\n\r\n" + body)
	out, err := compose(in, testOutgoing(), scrubSecret)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(out, []byte("\r\n\r\n"+body)) {
		t.Errorf("signed body changed:\n%s", out)
	}
}

func TestLeakCheck(t *testing.T) {
	env := newTestEnv(t, nil)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	leaky := map[string]string{
		"own address":   "To: x@example.com\r\n\r\nwrite to Paul@Gmail.com\r\n",
		"token address": "To: x@example.com\r\n\r\nmailto:paul+a1.abcdefghij@gmail.com\r\n",
		"html entity":   "To: x@example.com\r\n\r\npaul&#64;gmail.com\r\n",
		"named entity":  "To: x@example.com\r\n\r\npaul&commat;gm&#97;il.com\r\n",
		"url encoded":   "To: x@example.com\r\n\r\nmailto:paul%40gmail.com\r\n",
		"url plus":      "To: x@example.com\r\n\r\nmailto:paul%2Ba1.abcdefghij%40gmail.com\r\n",
		"attached message": "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: message/rfc822\r\n\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\ncmVhY2ggbWUgYXQgcGF1bEBnbWFpbC5jb20=\r\n--b--\r\n",
		"stray percent":   "To: x@example.com\r\n\r\n100% done, mailto:paul%40gmail.com\r\n",
		"via name":        "To: x@example.com\r\n\r\nMax (via AOL2) wrote\r\n",
		"encoded subject": "Subject: =?utf-8?b?" + b64("paul@gmail.com") + "?=\r\n\r\nx\r\n",
		"base64 body":     "Content-Transfer-Encoding: base64\r\n\r\n" + b64("from paul@gmail.com") + "\r\n",
	}
	for name, msg := range leaky {
		if err := leakCheck([]byte(msg), env.b.leaks); err == nil {
			t.Errorf("%s: leak not detected", name)
		}
	}
	// Correspondents on Gmail have mail.gmail.com Message-IDs in the thread.
	clean := "To: friend@gmail.com, mypaul@gmail.com\r\nReferences: <CAx@mail.gmail.com>\r\n" +
		"Subject: via email (via post)\r\n\r\npaul@gmail.co.uk 100%\r\n"
	if err := leakCheck([]byte(clean), env.b.leaks); err != nil {
		t.Errorf("false positive: %v", err)
	}
}
