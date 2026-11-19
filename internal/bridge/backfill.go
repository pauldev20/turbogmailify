package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

// Gmail allows 6,000 quota units per user and minute. One backfilled message
// costs 105 (raw get 60, insert 25, trash 20), so 40 messages a minute leave
// room for the relay and the IMAP imports running alongside.
const defaultBackfillInterval = 1500 * time.Millisecond

var defaultAPIRetryDelays = []time.Duration{
	time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
	16 * time.Second, 32 * time.Second, 64 * time.Second,
}

// How often the backfill logs its progress, and how many problem messages a
// summary lists by id.
const (
	backfillLogEvery  = 30 * time.Second
	backfillListLimit = 100
)

// Backfill rewrites mail that reached Gmail without the bridge, once per
// account and BackfillQuery. Accounts are processed one after another. It
// can be stopped at any time: the next start skips what is already done.
func (b *Bridge) Backfill(ctx context.Context) {
	for _, alias := range b.aliases {
		acct := b.accounts[alias]
		q := strings.TrimSpace(acct.BackfillQuery)
		if q == "" {
			continue
		}
		if err := b.backfillAccount(ctx, acct, q); err != nil {
			log.Printf("Bridge backfill %s: stopped: %v", alias, err)
			if errors.Is(err, errDiscard) {
				return // the other accounts would fail the same way
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

type backfillRun struct {
	b       *Bridge
	ctx     context.Context
	acct    *account
	query   string
	dryRun  bool
	started time.Time

	total, done, paced                  int
	rewritten, sent, skipped, dryRunned int
	malformed, failed                   []string // "id: reason"
}

func (b *Bridge) backfillAccount(ctx context.Context, acct *account, query string) error {
	at, err := b.store.BackfillDone(acct.Alias, query)
	if err != nil {
		return err
	}
	// Finish originals of an interrupted run first, before any new copy is
	// inserted, including those that no longer match the query (e.g.
	// newer_than:7d, or a query changed since).
	pending, err := b.store.PendingBackfills(acct.Alias)
	if err != nil {
		return err
	}
	completed := !at.IsZero()
	if completed && len(pending) == 0 {
		log.Printf("Bridge backfill %s: %q already completed on %s", acct.Alias, query, at.Format(time.DateOnly))
		return nil
	}

	r := &backfillRun{b: b, ctx: ctx, acct: acct, query: query, dryRun: *b.cfg.DryRun, started: time.Now()}
	var ids []string
	var searchErr error
	if !completed {
		// Only the pending originals are left after a completed run.
		searchErr = r.call(func() (err error) { ids, err = b.api.Search(query); return })
	}
	if searchErr != nil {
		searchErr = fmt.Errorf("search %q: %w", query, searchErr)
		if !r.dryRun {
			b.notify("Bridge: backfill of "+acct.DisplayTag+" failed", searchErr.Error())
		}
		if len(pending) == 0 || ctx.Err() != nil {
			return searchErr
		}
		// Still finish the pending originals; the query runs on the next start.
	}
	ids = slices.DeleteFunc(ids, func(id string) bool { return slices.Contains(pending, id) })
	ids = append(pending, ids...)
	r.total = len(ids)
	mode := ""
	if r.dryRun {
		mode = " (dry run: nothing is changed)"
	}
	log.Printf("Bridge backfill %s: %d messages match %q, newest first%s", acct.Alias, r.total, query, mode)

	lastLog := time.Now()
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			r.summary("interrupted", nil)
			return err
		}
		if err := r.one(id); err != nil {
			if ctx.Err() != nil {
				r.summary("interrupted", nil)
				return ctx.Err()
			}
			err = fmt.Errorf("%s: %w", id, err)
			r.summary("aborted", err)
			return err
		}
		r.done++
		if time.Since(lastLog) >= backfillLogEvery {
			r.progress()
			lastLog = time.Now()
		}
	}
	how := "finished"
	if len(r.failed) > 0 {
		how = "finished with failures"
	}
	r.summary(how, nil)
	if searchErr != nil {
		return searchErr
	}
	if completed || r.dryRun || len(r.failed) > 0 {
		// Failed messages are retried on the next start.
		return nil
	}
	return b.store.SetBackfillDone(acct.Alias, query, b.now())
}

// one handles a single message. Only errors that make further progress
// pointless (state db, exhausted Gmail retries) are returned.
func (r *backfillRun) one(id string) error {
	b := r.b
	rec, seen, err := b.store.Outbound(id)
	if err != nil {
		return err
	}
	if seen && rec.State != stateBackfilling && rec.State != stateUnrouted {
		r.skipped++ // imported by the bridge, relayed, or backfilled before
		return nil
	}
	if rec.State == stateBackfilling && rec.Alias != r.acct.Alias {
		r.skipped++ // another account's queries match it too; that account finishes it
		return nil
	}

	if err := r.pace(); err != nil {
		return err
	}
	var msg *gmail.Message
	err = r.call(func() (err error) { msg, err = b.api.Get(id); return })
	if isNotFound(err) {
		r.skipped++
		if rec.State == stateBackfilling && !r.dryRun {
			// Deleted while pending: stop resuming it on every start.
			return b.record(id, stateIgnored, "")
		}
		return nil
	}
	if r.fatal(err) {
		return err
	}
	if err != nil {
		r.failed = append(r.failed, fmt.Sprintf("%s: get: %v", id, err))
		return nil
	}
	if slices.Contains(msg.LabelIds, "DRAFT") {
		r.skipped++ // Gmail cannot insert drafts, and their headers are still in flux
		return nil
	}
	raw, err := base64.URLEncoding.DecodeString(msg.Raw)
	if err != nil {
		r.malformed = append(r.malformed, id+": undecodable raw message")
		return nil
	}
	h, err := readHeader(raw)
	if err != nil {
		r.malformed = append(r.malformed, fmt.Sprintf("%s: %v: read header: %v", id, errMalformed, err))
		return nil
	}
	if h.Has(xOriginal + "From") {
		// Already a bridge copy. Recording it saves the Get on the next run.
		r.skipped++
		if !r.dryRun {
			return b.record(id, stateIgnored, "")
		}
		return nil
	}
	if b.hasTokenAddress(h) {
		// A stand-in reply or other mail the relay owns.
		r.skipped++
		return nil
	}

	if h.Get("From") == "" {
		// rewrite() would accept a Reply-To alone, but the copy would then lack
		// X-Bridge-Original-From, which marks it for the relay and for resume.
		r.malformed = append(r.malformed, fmt.Sprintf("%s: %v: no From header", id, errMalformed))
		return nil
	}

	if rec.State == stateBackfilling {
		// A previous run may have inserted the copy already.
		copyID, err := r.findCopy(id, h, rec.MessageID)
		if r.fatal(err) {
			return err
		}
		if err != nil {
			r.failed = append(r.failed, fmt.Sprintf("%s: look for an earlier copy: %v", id, err))
			return nil
		}
		if copyID != "" && r.dryRun {
			r.skipped++
			log.Printf("Bridge backfill %s: dry run: would finish %s, already copied to %s", r.acct.Alias, id, copyID)
			return nil
		}
		if copyID != "" {
			log.Printf("Bridge backfill %s: %s was already copied to %s; finishing", r.acct.Alias, id, copyID)
			return r.finish(id, copyID, r.isSent(h))
		}
	}

	sent := r.isSent(h)
	mode := b.cfg.RewriteMode
	if sent {
		mode = rewriteSent
	}
	tokenize := func(a *mail.Address) (*mail.Address, error) { return b.tokenize(r.acct, a) }
	if r.dryRun {
		tokenize = func(a *mail.Address) (*mail.Address, error) {
			t, _ := b.tokenAddress(r.acct, a)
			return t, nil
		}
	}
	rewritten, err := rewrite(raw, mode, tokenize)
	if errors.Is(err, errMalformed) {
		r.malformed = append(r.malformed, fmt.Sprintf("%s: %v", id, err))
		return nil
	}
	if errors.Is(err, ErrTokenCollision) {
		r.failed = append(r.failed, fmt.Sprintf("%s: %v", id, err))
		return nil
	}
	if err != nil {
		return err
	}
	if rewritten, err = r.stamp(rewritten, id, h); err != nil {
		r.malformed = append(r.malformed, fmt.Sprintf("%s: %v: %v", id, errMalformed, err))
		return nil
	}

	labels := slices.Clone(msg.LabelIds)
	if r.acct.GmailLabel != "" && !slices.Contains(labels, r.acct.GmailLabel) {
		labels = append(labels, r.acct.GmailLabel)
	}

	if r.dryRun {
		r.dryRunned++
		if sent {
			r.sent++
		}
		log.Printf("Bridge backfill %s: dry run: would rewrite %s%s, labels %v: %s",
			r.acct.Alias, id, sentTag(sent), labels, headerDiff(h, rewritten))
		return nil
	}

	// From here on a crash may leave an inserted copy behind; the next run
	// finds it instead of inserting a second one.
	claimed, err := b.store.ClaimBackfill(id, r.acct.Alias, b.now())
	if err != nil {
		return err
	}
	if !claimed {
		r.skipped++ // the IMAP import or the relay handled it meanwhile
		return nil
	}
	// Insert is not idempotent: after any error the copy may exist (the
	// client may itself have retried a failed upload), so look for it before
	// every new attempt and never insert twice after a server or network
	// error.
	var inserted *gmail.Message
	for attempt := 0; ; attempt++ {
		inserted, err = b.api.Insert(rewritten, labels, msg.ThreadId)
		if err == nil {
			return r.finish(id, inserted.Id, sent)
		}
		if r.ctx.Err() != nil {
			return err
		}
		if isRateLimited(err) && attempt >= len(b.apiRetryDelays) {
			return err // Gmail keeps refusing; stop and resume later
		}
		if !isRetryable(err) {
			r.failed = append(r.failed, fmt.Sprintf("%s: insert: %v", id, err))
			return nil
		}
		wait := b.backfillSettle // let the search index catch up
		if isRateLimited(err) {
			wait = max(wait, b.apiRetryDelays[attempt])
		}
		log.Printf("Bridge backfill %s: insert of %s failed, checking for a stored copy in %s: %v", r.acct.Alias, id, wait, err)
		if err := sleepCtx(r.ctx, wait); err != nil {
			return err
		}
		copyID, ferr := r.findCopy(id, h, "")
		if r.fatal(ferr) {
			return ferr
		}
		if ferr == nil && copyID != "" {
			return r.finish(id, copyID, sent)
		}
		if ferr != nil || !isRateLimited(err) {
			r.failed = append(r.failed, fmt.Sprintf("%s: insert outcome unknown, checked on the next start: %v", id, err))
			return nil
		}
	}
}

// setPending marks original id as being backfilled, with its copy if known.
func (r *backfillRun) setPending(id, copyID string) error {
	return r.b.store.SetOutbound(id, OutboundRecord{
		State: stateBackfilling, MessageID: copyID, Alias: r.acct.Alias, At: r.b.now(),
	})
}

// finish records the copy, then discards the original, as Inbox.DoImport
// does.
func (r *backfillRun) finish(origID, copyID string, sent bool) error {
	b := r.b
	// Remember the copy first, so a later start finds it without searching.
	if err := r.setPending(origID, copyID); err != nil {
		return err
	}
	if err := b.record(copyID, stateIgnored, ""); err != nil {
		return err
	}
	err := r.call(func() error { return b.discard(origID) })
	if isNotFound(err) {
		err = nil // already gone, e.g. a retry after an unconfirmed success
	}
	if err != nil {
		// Every original would fail the same way (e.g. DeleteOriginal without
		// the full mail scope) and leave a duplicate behind, so stop. The
		// original keeps stateBackfilling with its copy id; the next start
		// finishes it.
		return fmt.Errorf("%w (copy %s is stored): %w", errDiscard, copyID, err)
	}
	if err := b.record(origID, stateIgnored, ""); err != nil {
		log.Println("Bridge:", err)
	}
	r.rewritten++
	if sent {
		r.sent++
	}
	return nil
}

// errDiscard means an original could not be trashed or deleted after its copy
// was stored. It stops the whole backfill: every further message would be
// left twice.
var errDiscard = errors.New("discard original")

// backfillOf names the original a backfilled copy was made from, so an
// interrupted run recognises its own copy and nothing else.
const backfillOf = "X-Bridge-Backfill-Of"

func (r *backfillRun) tag(id string) string { return r.acct.Alias + "/" + id }

// messageID returns the Message-ID used to find the copy of original id: its
// own, or for mail without one the Message-ID stamp gives the copy.
func (r *backfillRun) messageID(id string, h mail.Header) string {
	if mid, ok := parsedMessageID(h); ok {
		return mid
	}
	// Independent of config, so it stays the same across restarts.
	return "backfill." + r.acct.Alias + "." + id + "@bridge.invalid"
}

// parsedMessageID returns h's Message-ID without brackets or comments.
func parsedMessageID(h mail.Header) (string, bool) {
	mid, err := h.MessageID()
	return mid, err == nil && mid != ""
}

// stamp marks rewritten as the copy of original id.
func (r *backfillRun) stamp(rewritten []byte, id string, h mail.Header) ([]byte, error) {
	br := bufio.NewReader(bytes.NewReader(rewritten))
	th, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, err
	}
	th.Set(backfillOf, r.tag(id))
	// The copy must carry the Message-ID findCopy searches for.
	if _, ok := parsedMessageID(h); !ok {
		th.Set("Message-ID", "<"+r.messageID(id, h)+">")
	}
	return writeMessage(mail.Header{Header: message.Header{Header: th}}, br)
}

// findCopy returns the copy of original id inserted by an earlier,
// interrupted run: known is the copy id recorded after the insert, if any;
// otherwise copies are looked up by Message-ID. A copy in Trash does not
// count.
func (r *backfillRun) findCopy(id string, h mail.Header, known string) (string, error) {
	isCopy := func(cid string, strict bool) (bool, error) {
		var m *gmail.Message
		err := r.call(func() (err error) { m, err = r.b.api.Get(cid); return })
		if isNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if slices.Contains(m.LabelIds, "TRASH") {
			return false, nil
		}
		raw, err := base64.URLEncoding.DecodeString(m.Raw)
		var ch mail.Header
		if err == nil {
			ch, err = readHeader(raw)
		}
		if err != nil {
			if strict {
				// The recorded copy exists but cannot be checked; inserting
				// again could duplicate it.
				return false, fmt.Errorf("recorded copy %s unreadable: %w", cid, err)
			}
			return false, nil
		}
		return ch.Get(backfillOf) == r.tag(id), nil
	}
	if known != "" {
		if ok, err := isCopy(known, true); ok || err != nil {
			return known, err
		}
	}
	var ids []string
	if err := r.call(func() (err error) { ids, err = r.b.api.Search("rfc822msgid:" + r.messageID(id, h)); return }); err != nil {
		return "", err
	}
	for _, cid := range ids {
		if cid == id {
			continue
		}
		if ok, err := isCopy(cid, false); ok || err != nil {
			return cid, err
		}
	}
	return "", nil
}

// isSent reports whether the account itself wrote the message.
func (r *backfillRun) isSent(h mail.Header) bool {
	from, err := h.AddressList("From")
	if err != nil {
		return false
	}
	return slices.ContainsFunc(from, func(a *mail.Address) bool {
		return strings.EqualFold(a.Address, r.acct.self.Address)
	})
}

// hasTokenAddress reports whether any address header holds a token address.
func (b *Bridge) hasTokenAddress(h mail.Header) bool {
	for _, k := range []string{"From", "Reply-To", "To", "Cc", "Bcc"} {
		addrs, _ := h.AddressList(k)
		for _, a := range addrs {
			if _, _, ok := b.tokens.Parse(a.Address); ok {
				return true
			}
		}
	}
	return false
}

// pace spaces out messages that cost Gmail quota.
func (r *backfillRun) pace() error {
	if d := time.Until(r.b.backfillNext); d > 0 {
		if err := sleepCtx(r.ctx, d); err != nil {
			return err
		}
	}
	r.b.backfillNext = time.Now().Add(r.b.backfillInterval)
	r.paced++
	return nil
}

// call runs f, retrying rate limits, server and network errors with backoff.
func (r *backfillRun) call(f func() error) error { return r.callIf(isRetryable, f) }

// callIf runs f, retrying errors for which retry is true with backoff.
func (r *backfillRun) callIf(retry func(error) bool, f func() error) error {
	for attempt := 0; ; attempt++ {
		err := f()
		if err == nil || !retry(err) || attempt >= len(r.b.apiRetryDelays) {
			return err
		}
		d := r.b.apiRetryDelays[attempt]
		log.Printf("Bridge backfill %s: Gmail error, retrying in %s: %v", r.acct.Alias, d, err)
		if err := sleepCtx(r.ctx, d); err != nil {
			return err
		}
	}
}

// fatal reports errors that end the run: Gmail still failing after every
// retry, or the run being stopped.
func (r *backfillRun) fatal(err error) bool {
	return err != nil && (isRetryable(err) || r.ctx.Err() != nil)
}

// isRetryable reports Gmail rate limits, server errors and network failures.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if isRateLimited(err) {
		return true
	}
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		return gerr.Code >= 500
	}
	_, isNet := errors.AsType[net.Error](err)
	return isNet || errors.Is(err, io.ErrUnexpectedEOF)
}

// isRateLimited reports errors with which Gmail refused a request outright.
func isRateLimited(err error) bool {
	gerr, ok := errors.AsType[*googleapi.Error](err)
	if !ok {
		return false
	}
	switch gerr.Code {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		// Gmail reports per-user rate limits as 403.
		return slices.ContainsFunc(gerr.Errors, func(e googleapi.ErrorItem) bool {
			return e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded"
		})
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *backfillRun) progress() {
	pct := 100 * float64(r.done) / float64(max(r.total, 1))
	elapsed := time.Since(r.started)
	eta := "unknown"
	if r.paced > 0 {
		// Messages skipped from the state db cost no quota; count only paced ones.
		perMsg := elapsed / time.Duration(r.paced)
		eta = (time.Duration(r.total-r.done) * perMsg).Round(time.Second).String()
	}
	log.Printf("Bridge backfill %s: %d/%d (%.1f%%): %s; ETA %s",
		r.acct.Alias, r.done, r.total, pct, r.counts(), eta)
}

func (r *backfillRun) counts() string {
	rewritten := fmt.Sprintf("%d rewritten (%d sent)", r.rewritten, r.sent)
	if r.dryRun {
		rewritten = fmt.Sprintf("%d would be rewritten (%d sent)", r.dryRunned, r.sent)
	}
	return fmt.Sprintf("%s, %d skipped, %d malformed, %d failed",
		rewritten, r.skipped, len(r.malformed), len(r.failed))
}

// summary logs the outcome and, outside dry runs, notifies the user.
func (r *backfillRun) summary(how string, cause error) {
	var body strings.Builder
	fmt.Fprintf(&body, "Backfill of %s (%s) %s after %s.\nQuery: %s\n%d of %d messages handled: %s.\n",
		r.acct.Alias, r.acct.DisplayTag, how, time.Since(r.started).Round(time.Second), r.query, r.done, r.total, r.counts())
	if cause != nil {
		fmt.Fprintf(&body, "Stopped by: %v\nRestart the bridge to resume.\n", cause)
	}
	listIDs(&body, "Left untouched because the bridge cannot rewrite them (malformed)", r.malformed)
	listIDs(&body, "Failed; retried on the next start", r.failed)
	log.Printf("Bridge backfill %s: %s", r.acct.Alias, strings.TrimSpace(body.String()))
	if !r.dryRun {
		r.b.notify(fmt.Sprintf("Bridge: backfill of %s %s", r.acct.DisplayTag, how), body.String())
	}
}

func listIDs(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	for i, s := range items {
		if i == backfillListLimit {
			fmt.Fprintf(w, "... and %d more\n", len(items)-i)
			break
		}
		fmt.Fprintf(w, "  %s\n", s)
	}
}

func sentTag(sent bool) string {
	if sent {
		return " (sent by the account: To and Cc only)"
	}
	return ""
}

// headerDiff describes how rewriting changed the address headers.
func headerDiff(before mail.Header, rewritten []byte) string {
	after, err := readHeader(rewritten)
	if err != nil {
		return err.Error()
	}
	var parts []string
	for _, k := range []string{"From", "Reply-To", "To", "Cc"} {
		if o, n := before.Get(k), after.Get(k); o != n {
			parts = append(parts, fmt.Sprintf("%s: %q -> %q", k, o, n))
		}
	}
	if len(parts) == 0 {
		return "no address changes"
	}
	return strings.Join(parts, "; ")
}
