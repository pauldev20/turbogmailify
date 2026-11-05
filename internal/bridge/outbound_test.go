package bridge

import (
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message/mail"
	"google.golang.org/api/gmail/v1"
)

// standIn loads the synthetic Gmail reply with real token addresses.
func standIn(t *testing.T, env *testEnv) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/gmail-web-reply.synthetic.eml")
	if err != nil {
		t.Fatal(err)
	}
	a1 := env.b.accounts["a1"]
	maxTok, _ := env.b.tokenize(a1, &mail.Address{Name: "Max Mustermann", Address: "max@example.com"})
	bossTok, _ := env.b.tokenize(a1, &mail.Address{Name: "Boss", Address: "boss@example.com"})
	return []byte(strings.NewReplacer(
		"TOKEN_MAX_ENTITY", strings.Replace(maxTok.Address, "@", "&#64;", 1),
		"TOKEN_MAX", maxTok.Address,
		"TOKEN_BOSS", bossTok.Address,
	).Replace(string(raw)))
}

// simpleStandIn is a minimal reply to the given recipients.
func simpleStandIn(to string) []byte {
	return crlf("From: paul@gmail.com\nTo: " + to + "\nSubject: Re: x\nMessage-ID: <s@mail.gmail.com>\nContent-Type: text/plain\n\nok\n")
}

// startedEnv returns an env whose history baseline is already stored.
func startedEnv(t *testing.T, edit func(*Config)) *testEnv {
	t.Helper()
	env := newTestEnv(t, edit)
	env.gm.HistoryIDNow = 100
	if err := env.b.poll(); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *testEnv) poll(t *testing.T) {
	t.Helper()
	if err := e.b.poll(); err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) state(t *testing.T, id string) string {
	t.Helper()
	r, _, err := e.b.store.Outbound(id)
	if err != nil {
		t.Fatal(err)
	}
	return r.State
}

func (e *testEnv) labelled(id, suffix string) bool {
	return slices.Contains(e.gm.Labelled[id], "L:Bridge/"+suffix)
}

func TestRelayGmailReply(t *testing.T) {
	env := startedEnv(t, nil)
	if len(env.gm.Messages) != 0 || env.sender.sent != nil {
		t.Fatal("first poll must only store the history baseline")
	}
	stand := env.gm.AddSent(standIn(t, env), "INBOX")
	// The copy Gmail delivers to the token address carries the same Message-ID.
	delivered := env.gm.Add(standIn(t, env), []string{"INBOX"}, "")
	unrelated := env.gm.Add(crlf("From: x@example.com\nMessage-ID: <CAF3xyz0000@mail.gmail.com>\nSubject: other\n\nx\n"), []string{"INBOX"}, "")
	env.poll(t)

	if len(env.sender.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(env.sender.sent))
	}
	s := env.sender.sent[0]
	if s.server != "smtp.aol.com:465" || s.from != "old1@aol.com" {
		t.Errorf("sent via %s from %s", s.server, s.from)
	}
	if !slices.Equal(s.to, []string{"max@example.com", "boss@example.com"}) {
		t.Errorf("envelope recipients = %v (self must be dropped, Bcc kept)", s.to)
	}

	h, _ := readHeader(s.msg)
	if got := addrList(t, h, "From"); got != `"Paul Geeser" <old1@aol.com>` {
		t.Errorf("From = %s", got)
	}
	if got := addrList(t, h, "To"); got != `"Max Mustermann" <max@example.com>` {
		t.Errorf("To = %s", got)
	}
	for _, k := range []string{"Cc", "Bcc", "X-Gm-Message-State", "X-Google-Smtp-Source", "Received"} {
		if h.Has(k) {
			t.Errorf("%s must be dropped", k)
		}
	}
	if !strings.HasSuffix(h.Get("Message-Id"), "@aol.com>") || h.Get("In-Reply-To") != "<abc@example.com>" {
		t.Errorf("Message-ID %s, In-Reply-To %s", h.Get("Message-Id"), h.Get("In-Reply-To"))
	}

	p := parts(t, s.msg)
	mustContain(t, p["text/plain"], "Max Mustermann <\r\nmax@example.com> wrote:", "sounds good.")
	mustContain(t, p["text/html"], `href="mailto:max@example.com"`, ">max@example.com</a>")
	mustNotContain(t, p["text/plain"]+p["text/html"], "via AOL1", "gmail.com")
	if p["application/octet-stream"] != "\x00\x01binary attachment" {
		t.Errorf("attachment = %q", p["application/octet-stream"])
	}

	// Filed in Sent on the stand-in's thread; stand-in and delivered copy trashed.
	var filed *string
	for id, m := range env.gm.Messages {
		if string(m.Raw) == string(s.msg) {
			filed = &id
			if !slices.Equal(m.Labels, []string{"SENT", "Label_1"}) || m.ThreadID != env.gm.Messages[stand].ThreadID {
				t.Errorf("filed copy labels %v thread %s", m.Labels, m.ThreadID)
			}
		}
	}
	if filed == nil {
		t.Fatal("relayed mail not filed")
	}
	if !env.gm.Messages[stand].Trashed || !env.gm.Messages[delivered].Trashed {
		t.Error("stand-in and delivered copy must be trashed")
	}
	if env.gm.Messages[unrelated].Trashed {
		t.Error("unrelated message with the same Message-ID must be kept")
	}
	if env.state(t, stand) != stateSent {
		t.Errorf("state = %s", env.state(t, stand))
	}
	if id, _ := env.b.store.HistoryID(); id != env.gm.HistoryIDNow {
		t.Errorf("history id = %d, want %d", id, env.gm.HistoryIDNow)
	}

	// Idempotent: reprocessing sends nothing.
	if err := env.b.process(stand); err != nil {
		t.Fatal(err)
	}
	if err := env.b.process(*filed); err != nil {
		t.Fatal(err)
	}
	if len(env.sender.sent) != 1 {
		t.Errorf("resent: %d sends", len(env.sender.sent))
	}
}

func TestRouteBlocks(t *testing.T) {
	env := newTestEnv(t, nil)
	t1 := env.token(t, "a1", "max@example.com")
	t2 := env.token(t, "a2", "max@example.com")
	cases := map[string]string{
		"two accounts":   "To: " + t1 + ", " + t2,
		"unknown token":  "To: paul+a1.abcdefghij@gmail.com",
		"unknown alias":  "To: paul+a9.abcdefghij@gmail.com",
		"bad recipients": "To: " + t1 + "\r\nCc: <<<",
	}
	for name, hdr := range cases {
		h, _ := readHeader([]byte(hdr + "\r\n\r\n"))
		if _, err := env.b.route(h); err == nil {
			t.Errorf("%s: want blocked", name)
		} else if _, ok := err.(errBlocked); !ok {
			t.Errorf("%s: want errBlocked, got %v", name, err)
		}
	}

	h, _ := readHeader([]byte("To: friend@example.com\r\n\r\n"))
	if r, err := env.b.route(h); r != nil || err != nil {
		t.Errorf("no tokens: want ignore, got %v %v", r, err)
	}
}

func TestRouteSelfRecipients(t *testing.T) {
	env := newTestEnv(t, nil)
	max := env.token(t, "a1", "max@example.com")
	addrs := func(l []*mail.Address) (s []string) {
		for _, a := range l {
			s = append(s, a.Address)
		}
		return
	}
	cases := []struct{ hdr, to, cc, bcc string }{
		{"To: paul+a1.self@gmail.com", "old1@aol.com", "", ""},
		{"To: " + max + "\r\nCc: paul+a1.self@gmail.com", "max@example.com", "", ""},
		{"To: " + max + "\r\nBcc: paul+a1.self@gmail.com", "max@example.com", "", "old1@aol.com"},
	}
	for _, c := range cases {
		h, _ := readHeader([]byte(c.hdr + "\r\n\r\n"))
		r, err := env.b.route(h)
		if err != nil {
			t.Fatalf("%q: %v", c.hdr, err)
		}
		got := strings.Join(addrs(r.to), ",") + "|" + strings.Join(addrs(r.cc), ",") + "|" + strings.Join(addrs(r.bcc), ",")
		if want := c.to + "|" + c.cc + "|" + c.bcc; got != want {
			t.Errorf("%q: got %s, want %s", c.hdr, got, want)
		}
	}
}

func TestBlockedReplyIsLabelledAndNotSent(t *testing.T) {
	env := startedEnv(t, nil)
	id := env.gm.AddSent(simpleStandIn("paul+a1.abcdefghij@gmail.com"))
	env.poll(t)
	if env.sender.sent != nil || !env.labelled(id, labelBlocked) || env.state(t, id) != stateBlocked {
		t.Fatalf("sent %v, labels %v, state %s", env.sender.sent, env.gm.Labelled[id], env.state(t, id))
	}
	if len(env.notes) != 1 {
		t.Errorf("notifications = %v", env.notes)
	}
}

func TestLeakBlocksSend(t *testing.T) {
	env := startedEnv(t, nil)
	raw := strings.Replace(string(simpleStandIn(env.token(t, "a1", "max@example.com"))), "ok\r\n", "--\r\nPaul, paul@gmail.com\r\n", 1)
	id := env.gm.AddSent([]byte(raw))
	env.poll(t)
	if env.sender.sent != nil || !env.labelled(id, labelBlocked) {
		t.Fatal("leaking reply must be blocked")
	}
}

func TestMixedRecipientsStillRelayed(t *testing.T) {
	env := startedEnv(t, nil)
	id := env.gm.AddSent(simpleStandIn(env.token(t, "a2", "max@example.com") + ", direct@example.com"))
	env.poll(t)
	if !env.labelled(id, labelMixed) {
		t.Error("want Mixed label")
	}
	if len(env.sender.sent) != 1 || !slices.Equal(env.sender.sent[0].to, []string{"max@example.com"}) ||
		env.sender.sent[0].server != "smtp2.aol.com:587" {
		t.Errorf("sent = %+v", env.sender.sent)
	}
}

func TestIgnoresNonSentAndInboundCopies(t *testing.T) {
	env := startedEnv(t, nil)
	tok := env.token(t, "a1", "max@example.com")
	notSent := env.gm.Add(simpleStandIn(tok), []string{"INBOX"}, "")
	inboundCopy := env.gm.AddSent(append([]byte("X-Bridge-Original-From: max@example.com\r\n"), simpleStandIn(tok)...))
	for _, id := range []string{notSent, inboundCopy} {
		if err := env.b.process(id); err != nil {
			t.Fatal(err)
		}
		if env.state(t, id) != stateIgnored {
			t.Errorf("%s: state %s", id, env.state(t, id))
		}
	}
	if env.sender.sent != nil {
		t.Error("nothing may be sent")
	}
}

func TestDryRun(t *testing.T) {
	env := startedEnv(t, func(c *Config) { c.DryRun = new(true) })
	id := env.gm.AddSent(simpleStandIn(env.token(t, "a1", "max@example.com")))
	env.poll(t)
	if env.sender.sent != nil || !env.labelled(id, labelDryRun) || env.state(t, id) != stateDryRun {
		t.Fatal("dry run must label without sending")
	}
	if env.gm.Messages[id].Trashed {
		t.Error("dry run must keep the stand-in")
	}
}

func TestRateLimit(t *testing.T) {
	env := startedEnv(t, func(c *Config) { c.MaxSendsPerHour = 1 })
	tok := env.token(t, "a1", "max@example.com")
	env.gm.AddSent(simpleStandIn(tok))
	second := env.gm.AddSent(simpleStandIn(tok))
	env.poll(t)
	if len(env.sender.sent) != 1 || !env.labelled(second, labelBlocked) {
		t.Fatalf("sent %d, second labels %v", len(env.sender.sent), env.gm.Labelled[second])
	}
}

func TestSMTPRetriesRespectRateLimit(t *testing.T) {
	env := startedEnv(t, func(c *Config) { c.MaxSendsPerHour = 1 })
	env.sender.errs = []error{&textproto.Error{Code: 421, Msg: "try later"}, nil}
	id := env.gm.AddSent(simpleStandIn(env.token(t, "a1", "max@example.com")))
	env.poll(t)
	if env.sender.sent != nil || env.state(t, id) != stateFailed {
		t.Fatalf("sent %d, state %s", len(env.sender.sent), env.state(t, id))
	}
}

func TestSMTPRetriesTransientErrors(t *testing.T) {
	env := startedEnv(t, nil)
	env.sender.errs = []error{&textproto.Error{Code: 421, Msg: "try later"}, nil}
	id := env.gm.AddSent(simpleStandIn(env.token(t, "a1", "max@example.com")))
	env.poll(t)
	if len(env.sender.sent) != 1 || env.state(t, id) != stateSent {
		t.Fatalf("sent %d, state %s", len(env.sender.sent), env.state(t, id))
	}
}

func TestSMTPUncertainDeliveryIsNotRetried(t *testing.T) {
	env := startedEnv(t, nil)
	env.sender.errs = []error{errUncertain, nil}
	id := env.gm.AddSent(simpleStandIn(env.token(t, "a1", "max@example.com")))
	env.poll(t)
	if env.sender.sent != nil || env.state(t, id) != stateFailed {
		t.Fatalf("sent %d, state %s", len(env.sender.sent), env.state(t, id))
	}
	if ok, _ := env.b.store.ReserveSend(env.b.now(), 1); ok {
		t.Error("uncertain attempt must count toward the rate limit")
	}
}

func TestSMTPPermanentFailure(t *testing.T) {
	env := startedEnv(t, nil)
	env.sender.errs = []error{&textproto.Error{Code: 550, Msg: "rejected"}}
	id := env.gm.AddSent(simpleStandIn(env.token(t, "a1", "max@example.com")))
	env.poll(t)
	if env.sender.sent != nil || env.state(t, id) != stateFailed || !env.labelled(id, labelFailed) {
		t.Fatalf("state %s, labels %v", env.state(t, id), env.gm.Labelled[id])
	}
	if env.gm.Messages[id].Trashed {
		t.Error("failed stand-in must stay for the user")
	}
}

func TestInterruptedSendIsNotRetried(t *testing.T) {
	env := startedEnv(t, nil)
	id := env.gm.AddSent(simpleStandIn(env.token(t, "a1", "max@example.com")))
	env.b.store.SetOutbound(id, OutboundRecord{State: stateSending})
	env.poll(t)
	if env.sender.sent != nil || env.state(t, id) != stateFailed || !env.labelled(id, labelFailed) {
		t.Fatalf("sent %v, state %s", env.sender.sent, env.state(t, id))
	}
}

func TestExpiredHistoryResyncs(t *testing.T) {
	env := startedEnv(t, nil)
	id := env.gm.AddSent(simpleStandIn(env.token(t, "a1", "max@example.com")))
	env.gm.HistoryExpire = true
	env.poll(t)
	if env.state(t, id) != stateSent {
		t.Fatalf("state %s", env.state(t, id))
	}
	if got, _ := env.b.store.HistoryID(); got != env.gm.HistoryIDNow {
		t.Errorf("history id = %d", got)
	}
}

func TestScheduledSendViaLabelAdded(t *testing.T) {
	env := startedEnv(t, nil)
	id := env.gm.Add(simpleStandIn(env.token(t, "a1", "max@example.com")), []string{"SENT"}, "")
	env.gm.HistoryIDNow++
	env.gm.Events = append(env.gm.Events, &gmail.History{
		Id:          env.gm.HistoryIDNow,
		LabelsAdded: []*gmail.HistoryLabelAdded{{LabelIds: []string{"SENT"}, Message: &gmail.Message{Id: id}}},
	})
	env.poll(t)
	if env.state(t, id) != stateSent {
		t.Fatalf("state %s", env.state(t, id))
	}
}

func TestOtherAccountsTokensAreNotScrubbed(t *testing.T) {
	env := startedEnv(t, nil)
	raw := strings.Replace(string(simpleStandIn(env.token(t, "a1", "max@example.com"))), "ok\r\n",
		"> from "+env.token(t, "a2", "eve@example.com")+"\r\n", 1)
	id := env.gm.AddSent([]byte(raw))
	env.poll(t)
	if env.sender.sent != nil || !env.labelled(id, labelBlocked) {
		t.Fatal("a reply quoting another account's token must be blocked, not scrubbed")
	}
}

func TestWrappedViaNameIsScrubbed(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, in := range []string{"Max (via\r\nAOL1) wrote", "Max&nbsp;(via&nbsp;AOL1) wrote", "Max (via\u00a0AOL1) wrote"} {
		if got := env.b.scrub(env.b.accounts["a1"], in); got != "Max wrote" {
			t.Errorf("scrub(%q) = %q", in, got)
		}
	}
}

func TestReplyToOwnStandInReferencesRelayedID(t *testing.T) {
	env := startedEnv(t, nil)
	tok := env.token(t, "a1", "max@example.com")
	env.gm.AddSent(simpleStandIn(tok))
	env.poll(t)
	h0 := readHeaderT(t, env.sender.sent[0].msg)
	first := h0.Get("Message-Id")

	followUp := strings.Replace(string(simpleStandIn(tok)), "Message-ID: <s@mail.gmail.com>",
		"Message-ID: <s2@mail.gmail.com>\r\nIn-Reply-To: <s@mail.gmail.com>\r\nReferences: <abc@example.com> <s@mail.gmail.com>", 1)
	env.gm.AddSent([]byte(followUp))
	env.poll(t)
	h := readHeaderT(t, env.sender.sent[1].msg)
	if h.Get("In-Reply-To") != first || h.Get("References") != "<abc@example.com> "+first {
		t.Errorf("In-Reply-To %s, References %s, want %s", h.Get("In-Reply-To"), h.Get("References"), first)
	}
}

func readHeaderT(t *testing.T, raw []byte) mail.Header {
	t.Helper()
	h, err := readHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHealthcheck(t *testing.T) {
	env := newTestEnv(t, nil)
	if err := Healthcheck(env.b.cfg); err == nil {
		t.Error("no heartbeat yet: want unhealthy")
	}
	env.b.touchHeartbeat()
	if err := Healthcheck(env.b.cfg); err != nil {
		t.Errorf("fresh heartbeat: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(env.b.cfg.StateDir, heartbeatFile), old, old)
	if err := Healthcheck(env.b.cfg); err == nil {
		t.Error("stale heartbeat: want unhealthy")
	}
}
