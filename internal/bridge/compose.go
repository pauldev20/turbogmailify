package bridge

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

// keptHeaders are copied from the stand-in along with all Content-* fields.
// Everything else (Bcc, Received, X-Gm-*, X-Google-*, ...) is dropped.
var keptHeaders = map[string]bool{
	"Date":         true,
	"Subject":      true,
	"Mime-Version": true,
	"In-Reply-To":  true,
	"References":   true,
}

type outgoing struct {
	from   *mail.Address
	to, cc []*mail.Address
	msgID  string              // without angle brackets
	mapID  func(string) string // maps referenced Message-IDs
}

// compose rebuilds the stand-in as the message recipients receive: allowlisted
// headers, new addresses and Message-ID, and text passed through scrub.
func compose(raw []byte, o outgoing, scrub func(string) string) ([]byte, error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	th, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, err
	}

	var kept [][]byte
	fields := th.Fields()
	for fields.Next() {
		if k := fields.Key(); keptHeaders[k] || strings.HasPrefix(k, "Content-") {
			f, err := fields.Raw()
			if err != nil {
				return nil, err
			}
			kept = append(kept, f)
		}
	}
	var h mail.Header
	// AddRaw prepends on output, so add in reverse to keep the original order.
	for i := len(kept) - 1; i >= 0; i-- {
		h.AddRaw(kept[i])
	}
	// A reply to our own stand-in must reference the ID recipients received.
	for _, k := range []string{"In-Reply-To", "References"} {
		ids, err := h.MsgIDList(k)
		if err != nil || len(ids) == 0 {
			continue
		}
		mapped := make([]string, len(ids))
		for i, id := range ids {
			mapped[i] = o.mapID(id)
		}
		if !slices.Equal(ids, mapped) {
			h.SetMsgIDList(k, mapped)
		}
	}
	h.SetAddressList("From", []*mail.Address{o.from})
	h.SetAddressList("To", o.to)
	h.SetAddressList("Cc", o.cc)
	h.SetMessageID(o.msgID)

	var out bytes.Buffer
	if err := writeEntity(&out, h.Header.Header, br, scrub); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// writeEntity writes a MIME entity whose body is still transfer-encoded. Only
// inline text/plain and text/html are decoded, scrubbed and re-encoded; all
// other parts, including attachments and multipart/signed, are copied byte for
// byte. The leak check still inspects them.
func writeEntity(w io.Writer, th textproto.Header, body io.Reader, scrub func(string) string) error {
	h := message.Header{Header: th}
	mt, params, _ := h.ContentType()
	disp, _, _ := h.ContentDisposition()
	boundary := params["boundary"]

	switch {
	case disp == "attachment":
		// Attachments, even multipart ones, are copied unchanged below.
	case strings.HasPrefix(mt, "multipart/") && mt != "multipart/signed" && boundary != "":
		if err := textproto.WriteHeader(w, th); err != nil {
			return err
		}
		mr := textproto.NewMultipartReader(body, boundary)
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "--%s\r\n", boundary); err != nil {
				return err
			}
			if err := writeEntity(w, p.Header, p, scrub); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\r\n"); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(w, "--%s--\r\n", boundary)
		return err

	case mt == "" || mt == "text/plain" || mt == "text/html":
		e, err := message.New(h, body) // decodes transfer encoding and charset
		if err != nil {
			return err
		}
		text, err := io.ReadAll(e.Body)
		if err != nil {
			return err
		}
		// message.New converted the text to UTF-8, the only charset it writes.
		h = h.Copy()
		if cs := strings.ToLower(params["charset"]); cs != "" && cs != "utf-8" && cs != "us-ascii" {
			params["charset"] = "utf-8"
			h.SetContentType(mt, params)
			// UTF-8 may contain 8-bit bytes that a 7bit label would misstate.
			switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
			case "", "7bit", "8bit":
				h.Set("Content-Transfer-Encoding", "quoted-printable")
			}
		}
		tw, err := message.CreateWriter(w, h)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(tw, scrub(string(text))); err != nil {
			return err
		}
		return tw.Close()
	}

	if err := textproto.WriteHeader(w, th); err != nil {
		return err
	}
	_, err := io.Copy(w, body)
	return err
}

// leakCheck fails if any pattern occurs in the raw message, in a decoded
// header field, or in any decoded body part, each also checked with HTML
// entities and URL escapes decoded.
func leakCheck(msg []byte, patterns []*regexp.Regexp) error {
	texts, err := decodedTexts(msg)
	if err != nil {
		return fmt.Errorf("leak check: %w", err)
	}
	for _, t := range texts {
		for _, v := range unescaped(string(t)) {
			for _, p := range patterns {
				if m := p.FindString(v); m != "" {
					return fmt.Errorf("leak check: found %q", strings.TrimSpace(m))
				}
			}
		}
	}
	return nil
}

// decodedTexts returns msg, its decoded header values and decoded leaf bodies,
// recursing into attached messages.
func decodedTexts(msg []byte) ([][]byte, error) {
	texts := [][]byte{msg}
	e, err := message.Read(bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	err = e.Walk(func(_ []int, part *message.Entity, err error) error {
		if err != nil {
			return err
		}
		fields := part.Header.Fields()
		for fields.Next() {
			if v, err := fields.Text(); err == nil {
				texts = append(texts, []byte(v))
			}
		}
		mt, _, _ := part.Header.ContentType()
		if strings.HasPrefix(mt, "multipart/") {
			return nil
		}
		b, err := io.ReadAll(part.Body)
		if err != nil {
			return err
		}
		texts = append(texts, b)
		// Walk does not descend into attached messages.
		if mt == "message/rfc822" || mt == "message/global" {
			inner, err := decodedTexts(b)
			if err != nil {
				return err
			}
			texts = append(texts, inner...)
		}
		return nil
	})
	return texts, err
}

var percentEscape = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// unescaped returns s as written, HTML-unescaped, and additionally with every
// valid %XX escape decoded (stray "%" signs are kept).
func unescaped(s string) []string {
	h := html.UnescapeString(s)
	u := percentEscape.ReplaceAllStringFunc(h, func(m string) string {
		b, _ := hex.DecodeString(m[1:])
		return string(b)
	})
	return []string{s, h, u}
}
