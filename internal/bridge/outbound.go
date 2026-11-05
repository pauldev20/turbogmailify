package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
	"github.com/google/uuid"
	"google.golang.org/api/gmail/v1"
)

// Run polls Gmail's SENT history and relays replies until ctx is done.
func (b *Bridge) Run(ctx context.Context) {
	t := time.NewTicker(b.cfg.PollInterval.Duration)
	defer t.Stop()
	for {
		if err := b.poll(); err != nil {
			log.Println("Bridge: poll error:", err)
		} else {
			b.touchHeartbeat()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// poll processes SENT messages added since the stored history id. The id only
// advances after every message is handled, so transient errors are retried.
func (b *Bridge) poll() error {
	if err := b.ensureLabels(); err != nil {
		return err
	}
	start, err := b.store.HistoryID()
	if err != nil {
		return err
	}
	if start == 0 {
		// First start: relay only what is sent from now on.
		id, err := b.api.HistoryID()
		if err != nil {
			return err
		}
		return b.store.SetHistoryID(id)
	}

	var (
		ids       []string
		latest    uint64
		pageToken string
	)
	for {
		r, err := b.api.History(start, pageToken)
		if isNotFound(err) {
			return b.resync()
		}
		if err != nil {
			return err
		}
		add := func(m *gmail.Message) {
			if m != nil && !slices.Contains(ids, m.Id) {
				ids = append(ids, m.Id)
			}
		}
		for _, h := range r.History {
			for _, a := range h.MessagesAdded {
				add(a.Message)
			}
			// Scheduled sends gain SENT after they were added.
			for _, l := range h.LabelsAdded {
				if slices.Contains(l.LabelIds, "SENT") {
					add(l.Message)
				}
			}
		}
		latest = r.HistoryId
		if pageToken = r.NextPageToken; pageToken == "" {
			break
		}
	}

	for _, id := range ids {
		if err := b.process(id); err != nil {
			return err
		}
	}
	if latest > start {
		return b.store.SetHistoryID(latest)
	}
	return nil
}

// resync recovers from an expired history id by scanning recent sent mail.
func (b *Bridge) resync() error {
	log.Println("Bridge: history id expired; rescanning the last day of sent mail")
	id, err := b.api.HistoryID()
	if err != nil {
		return err
	}
	ids, err := b.api.Search("in:sent newer_than:1d")
	if err != nil {
		return err
	}
	for _, msgID := range ids {
		if err := b.process(msgID); err != nil {
			return err
		}
	}
	return b.store.SetHistoryID(id)
}

func (b *Bridge) ensureLabels() error {
	if b.labelIDs != nil {
		return nil
	}
	if _, err := b.api.LabelID(b.cfg.LabelPrefix); err != nil {
		return err
	}
	ids := make(map[string]string)
	for _, s := range []string{labelBlocked, labelMixed, labelFailed, labelDryRun} {
		id, err := b.api.LabelID(b.cfg.LabelPrefix + "/" + s)
		if err != nil {
			return err
		}
		ids[s] = id
	}
	b.labelIDs = ids
	return nil
}

// route is the authorized destination of one stand-in message.
type route struct {
	acct        *account
	to, cc, bcc []*mail.Address
	direct      []string // non-token recipients Gmail already delivered to
}

// errBlocked carries the reason a message must not be relayed.
type errBlocked struct{ reason string }

func (e errBlocked) Error() string { return e.reason }

func blocked(format string, a ...any) error { return errBlocked{fmt.Sprintf(format, a...)} }

// process handles one SENT message. It returns an error only for transient
// failures that should be retried on the next poll.
func (b *Bridge) process(id string) error {
	rec, seen, err := b.store.Outbound(id)
	if err != nil {
		return err
	}
	if seen {
		if rec.State == stateSending {
			// Crashed during submission: the mail may or may not be out.
			b.fail(id, "", "interrupted during SMTP submission; it may or may not have been sent")
		}
		return nil
	}

	msg, err := b.api.Get(id)
	if isNotFound(err) {
		return b.record(id, stateIgnored, "")
	}
	if err != nil {
		return err
	}
	if !slices.Contains(msg.LabelIds, "SENT") {
		return b.record(id, stateIgnored, "")
	}
	raw, err := base64.URLEncoding.DecodeString(msg.Raw)
	if err != nil {
		return b.block(id, "", "undecodable raw message: "+err.Error())
	}
	h, err := readHeader(raw)
	if err != nil {
		return b.block(id, "", "unreadable header: "+err.Error())
	}
	subject, _ := h.Subject()
	if h.Has(xOriginal + "From") {
		// A rewritten inbound copy, never a reply.
		return b.record(id, stateIgnored, "")
	}

	r, err := b.route(h)
	if blk, ok := errors.AsType[errBlocked](err); ok {
		return b.block(id, subject, blk.reason)
	}
	if err != nil {
		return err
	}
	if r == nil {
		return b.record(id, stateIgnored, "")
	}
	if len(r.direct) > 0 {
		b.label(id, labelMixed)
		b.notify("Bridge: recipients reached directly from Gmail",
			fmt.Sprintf("Subject: %s\nDelivered by Gmail, not the bridge: %s\n\nThe token recipients are still relayed.",
				subject, strings.Join(r.direct, ", ")))
	}

	msgID := uuid.NewString() + "@" + r.acct.messageIDDomain()
	o := outgoing{from: r.acct.self, to: r.to, cc: r.cc, msgID: msgID, mapID: b.relayedID}
	out, err := compose(raw, o, func(s string) string { return b.scrub(r.acct, s) })
	if err != nil {
		return b.block(id, subject, "compose: "+err.Error())
	}
	if err := leakCheck(out, b.leaks); err != nil {
		return b.block(id, subject, err.Error())
	}

	if *b.cfg.DryRun {
		log.Printf("Bridge: dry run, not sending %s via %s (subject %s)", id, r.acct.Alias, subject)
		b.label(id, labelDryRun)
		return b.record(id, stateDryRun, msgID)
	}
	ok, err := b.store.ReserveSend(b.now(), b.cfg.MaxSendsPerHour)
	if err != nil {
		return err
	}
	if !ok {
		return b.block(id, subject, fmt.Sprintf("rate limit of %d SMTP attempts per hour reached", b.cfg.MaxSendsPerHour))
	}

	if err := b.record(id, stateSending, msgID); err != nil {
		return err
	}
	if err := b.send(r, out); err != nil {
		b.fail(id, subject, err.Error())
		return nil
	}
	if err := b.record(id, stateSent, msgID); err != nil {
		return err
	}
	if standID, err := h.MessageID(); err == nil && standID != "" {
		if err := b.store.SetRelayedID(standID, msgID); err != nil {
			log.Println("Bridge:", err)
		}
	}
	log.Printf("Bridge: relayed %s via %s (subject %s)", id, r.acct.Alias, subject)
	b.file(msg, r.acct, out, h)
	return nil
}

// route authorizes a stand-in: its recipients must include known tokens of
// exactly one alias. A nil route without error means "not for the bridge".
func (b *Bridge) route(h mail.Header) (*route, error) {
	r := &route{}
	lists := map[string]*[]*mail.Address{"To": &r.to, "Cc": &r.cc, "Bcc": &r.bcc}
	found := false
	for _, k := range []string{"To", "Cc", "Bcc"} {
		addrs, err := h.AddressList(k)
		if err != nil {
			return nil, blocked("unparseable %s header: %v", k, err)
		}
		for _, a := range addrs {
			alias, token, ok := b.tokens.Parse(a.Address)
			if !ok {
				r.direct = append(r.direct, a.Address)
				continue
			}
			found = true
			acct, ok := b.accounts[alias]
			if !ok {
				return nil, blocked("unknown alias %q in %s", alias, a.Address)
			}
			if r.acct != nil && r.acct != acct {
				return nil, blocked("recipients belong to more than one account (%s, %s)", r.acct.Alias, alias)
			}
			r.acct = acct
			real, ok, err := b.resolve(acct, token)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, blocked("unknown token address %s", a.Address)
			}
			*lists[k] = append(*lists[k], real)
		}
	}
	if !found {
		return nil, nil
	}
	// Reply-all keeps the self token in To/Cc; drop it there when anyone else
	// is addressed so the account does not receive its own reply.
	isSelf := func(a *mail.Address) bool { return a == r.acct.self } // resolve returns acct.self
	if others := slices.DeleteFunc(slices.Concat(r.to, r.cc, r.bcc), isSelf); len(others) > 0 {
		r.to = slices.DeleteFunc(r.to, isSelf)
		r.cc = slices.DeleteFunc(r.cc, isSelf)
	}
	return r, nil
}

// scrub replaces acct's token addresses and "(via <tag>)" names in body text.
// Tokens of other accounts are left for leakCheck to block, so a reply never
// links two source identities.
func (b *Bridge) scrub(acct *account, s string) string {
	s = b.textToken.ReplaceAllStringFunc(s, func(m string) string {
		sub := b.textToken.FindStringSubmatch(m)
		if strings.ToLower(sub[1]) != acct.Alias {
			return m
		}
		real, ok, err := b.resolve(acct, strings.ToLower(sub[2]))
		if err != nil || !ok {
			return m
		}
		return real.Address
	})
	for _, re := range b.viaNames {
		s = re.ReplaceAllString(s, "")
	}
	return s
}

func (b *Bridge) send(r *route, msg []byte) error {
	var rcpts []string
	for _, list := range [][]*mail.Address{r.to, r.cc, r.bcc} {
		for _, a := range list {
			rcpts = append(rcpts, a.Address)
		}
	}
	// The first attempt was reserved by process. Retries count too: an
	// unconfirmed attempt may have gone out.
	var err error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			ok, err := b.store.ReserveSend(b.now(), b.cfg.MaxSendsPerHour)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("rate limit reached while retrying")
			}
		}
		if err = b.sender.Send(r.acct.Smtp, r.acct.self.Address, rcpts, msg); err == nil {
			return nil
		}
		if !isTransient(err) || attempt >= len(b.retryDelays) {
			return err
		}
		log.Printf("Bridge: SMTP error via %s, retrying in %s: %v", r.acct.Alias, b.retryDelays[attempt], err)
		time.Sleep(b.retryDelays[attempt])
	}
}

// file stores the relayed mail in Gmail's Sent and removes the stand-in plus
// the copy Gmail delivered to the token address. Failures only cost tidiness,
// so they are reported instead of retried.
func (b *Bridge) file(stand *gmail.Message, acct *account, out []byte, h mail.Header) {
	labels := []string{"SENT"}
	if acct.GmailLabel != "" {
		labels = append(labels, acct.GmailLabel)
	}
	filed, err := b.api.Insert(out, labels, stand.ThreadId)
	if err != nil {
		b.notify("Bridge: relayed mail not filed in Sent", fmt.Sprintf("Gmail message %s was sent but could not be filed: %v", stand.Id, err))
		return
	}
	if err := b.record(filed.Id, stateIgnored, ""); err != nil {
		log.Println("Bridge:", err)
	}

	ids := []string{stand.Id}
	if mid := h.Get("Message-ID"); mid != "" {
		copies, err := b.api.Search("rfc822msgid:" + strings.Trim(mid, "<> "))
		if err != nil {
			log.Println("Bridge: search for stand-in copies:", err)
		}
		for _, c := range copies {
			if c != filed.Id && !slices.Contains(ids, c) && b.isDeliveredCopy(c, h) {
				ids = append(ids, c)
			}
		}
	}
	for _, id := range ids {
		if err := b.discard(id); err != nil {
			log.Printf("Bridge: failed to discard stand-in %s: %v", id, err)
		}
	}
}

// isDeliveredCopy reports whether message id is Gmail's delivery of the
// stand-in to the token address rather than an unrelated message that reuses
// its Message-ID.
func (b *Bridge) isDeliveredCopy(id string, stand mail.Header) bool {
	m, err := b.api.Get(id)
	if err != nil || slices.Contains(m.LabelIds, "SENT") {
		return false
	}
	raw, err := base64.URLEncoding.DecodeString(m.Raw)
	if err != nil {
		return false
	}
	h, err := readHeader(raw)
	return err == nil && !h.Has(xOriginal+"From") &&
		h.Get("From") == stand.Get("From") && h.Get("Subject") == stand.Get("Subject")
}

// relayedID maps a stand-in's Message-ID to the one recipients received.
func (b *Bridge) relayedID(id string) string {
	if r, err := b.store.RelayedID(id); err == nil && r != "" {
		return r
	}
	return id
}

func (b *Bridge) record(id, state, msgID string) error {
	return b.store.SetOutbound(id, OutboundRecord{State: state, MessageID: msgID, At: b.now()})
}

func (b *Bridge) block(id, subject, reason string) error {
	log.Printf("Bridge: blocked %s: %s", id, reason)
	b.label(id, labelBlocked)
	b.notify("Bridge: reply blocked", fmt.Sprintf("Subject: %s\nGmail message: %s\nReason: %s\n\nNothing was sent.", subject, id, reason))
	return b.record(id, stateBlocked, "")
}

func (b *Bridge) fail(id, subject, reason string) {
	log.Printf("Bridge: failed %s: %s", id, reason)
	b.label(id, labelFailed)
	b.notify("Bridge: reply failed", fmt.Sprintf("Subject: %s\nGmail message: %s\nReason: %s", subject, id, reason))
	if err := b.record(id, stateFailed, ""); err != nil {
		log.Println("Bridge:", err)
	}
}

func (b *Bridge) label(id, suffix string) {
	if err := b.api.AddLabels(id, []string{b.labelIDs[suffix]}); err != nil {
		log.Printf("Bridge: failed to label %s as %s: %v", id, suffix, err)
	}
}

func (a *account) messageIDDomain() string {
	if a.MessageIDDomain != "" {
		return a.MessageIDDomain
	}
	_, domain, _ := strings.Cut(a.self.Address, "@")
	return domain
}

func readHeader(raw []byte) (mail.Header, error) {
	th, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	return mail.Header{Header: message.Header{Header: th}}, err
}

// Heartbeat is touched after every successful poll; see Healthcheck.
const heartbeatFile = "heartbeat"

func (b *Bridge) touchHeartbeat() {
	p := filepath.Join(b.cfg.StateDir, heartbeatFile)
	now := b.now()
	if err := os.Chtimes(p, now, now); err != nil {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			log.Println("Bridge: heartbeat:", err)
		}
	}
}

// Healthcheck fails if the outbound poller has not completed a poll recently.
func Healthcheck(cfg Config) error {
	cfg.setDefaults()
	st, err := os.Stat(filepath.Join(cfg.StateDir, heartbeatFile))
	if err != nil {
		return err
	}
	limit := max(5*time.Minute, 10*cfg.PollInterval.Duration)
	if age := time.Since(st.ModTime()); age > limit {
		return fmt.Errorf("last successful poll %s ago", age.Round(time.Second))
	}
	return nil
}
