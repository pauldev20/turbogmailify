package bridge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/emersion/go-message/mail"
	"google.golang.org/api/googleapi"
)

const backfillLabel = "Label_old"

// backfillMail is an existing Gmail message from max to old1@aol.com, with
// optional extra header lines.
func backfillMail(day int, extra ...string) []byte {
	return crlf(fmt.Sprintf(`From: Max Mustermann <max@example.com>
To: Paul <old1@aol.com>
Subject: Day %d
Date: Mon, %d Jan 2018 10:00:00 +0000
Message-ID: <day%d@example.com>
%s
Body
`, day, day, day, strings.Join(append(extra, ""), "\n")))
}

func backfillEnv(t *testing.T, edit func(*Config)) *testEnv {
	t.Helper()
	env := newTestEnv(t, edit)
	env.b.accounts["a1"].BackfillQuery = "label:" + backfillLabel
	return env
}

func (e *testEnv) backfill(t *testing.T) {
	t.Helper()
	e.b.Backfill(context.Background())
}

// copies returns the ids of rewritten copies, keyed by the original's
// Message-ID.
func (e *testEnv) copies(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for id, m := range e.gm.Messages {
		h, err := readHeader(m.Raw)
		if err != nil || !h.Has(xOriginal+"From") || m.Trashed {
			continue
		}
		out[h.Get("Message-ID")] = append(out[h.Get("Message-ID")], id)
	}
	return out
}

func (e *testEnv) trashed() []string {
	var ids []string
	for _, c := range e.gm.Calls {
		if id, ok := strings.CutPrefix(c, "trash:"); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestBackfillNewestFirst(t *testing.T) {
	env := backfillEnv(t, nil)
	old := env.gm.Add(backfillMail(1), []string{"INBOX", backfillLabel}, "")
	newest := env.gm.Add(backfillMail(3), []string{"INBOX", "UNREAD", "IMPORTANT", backfillLabel}, "t-thread")
	middle := env.gm.Add(backfillMail(2), []string{backfillLabel}, "")

	env.backfill(t)

	if got, want := env.trashed(), []string{newest, middle, old}; !slices.Equal(got, want) {
		t.Fatalf("trashed %v, want newest first %v", got, want)
	}
	copies := env.copies(t)
	cp := env.gm.Messages[copies["<day3@example.com>"][0]]
	if cp.ThreadID != "t-thread" {
		t.Errorf("thread = %s, want t-thread", cp.ThreadID)
	}
	if want := []string{"INBOX", "UNREAD", "IMPORTANT", backfillLabel, "Label_1"}; !slices.Equal(cp.Labels, want) {
		t.Errorf("labels = %v, want %v", cp.Labels, want)
	}
	h, _ := readHeader(cp.Raw)
	if got := addrList(t, h, "Reply-To"); got != `"Max Mustermann (via AOL1)" <`+env.token(t, "a1", "max@example.com")+`>` {
		t.Errorf("Reply-To = %s", got)
	}
	for _, ids := range copies {
		if len(ids) != 1 || env.state(t, ids[0]) != stateIgnored {
			t.Errorf("copies %v must be unique and ignored by the relay", ids)
		}
	}
	for _, id := range []string{old, newest, middle} {
		if env.state(t, id) != stateIgnored {
			t.Errorf("original %s state = %q", id, env.state(t, id))
		}
	}
	if len(env.notes) != 1 || !strings.Contains(env.bodies[0], "3 rewritten (0 sent)") {
		t.Errorf("summary = %v %v", env.notes, env.bodies)
	}

	// Completed once: a restart does not run it again.
	calls := len(env.gm.Calls)
	env.gm.Add(backfillMail(4), []string{backfillLabel}, "")
	env.backfill(t)
	if len(env.gm.Calls) != calls {
		t.Errorf("completed backfill ran again: %v", env.gm.Calls[calls:])
	}
}

func TestBackfillSkips(t *testing.T) {
	env := backfillEnv(t, nil)
	add := func(raw []byte, labels ...string) string {
		return env.gm.Add(raw, append(labels, backfillLabel), "")
	}
	bridged := add(backfillMail(1, "X-Bridge-Original-From: Max <max@example.com>"))
	known := add(backfillMail(2), "SENT")
	if err := env.b.record(known, stateSent, "relayed@aol.com"); err != nil {
		t.Fatal(err)
	}
	draft := add(backfillMail(3), "DRAFT")
	standIn := add(crlf("From: me@gmail.com\nTo: "+env.token(t, "a1", "max@example.com")+"\nDate: Thu, 4 Jan 2018 10:00:00 +0000\nSubject: Re\n\nx\n"), "SENT")
	malformed := add(crlf("Subject: no sender\nDate: Fri, 5 Jan 2018 10:00:00 +0000\n\nx\n"))
	ok := add(backfillMail(6))

	env.backfill(t)

	if got := env.trashed(); !slices.Equal(got, []string{ok}) {
		t.Fatalf("trashed %v, want only %s", got, ok)
	}
	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 {
		t.Errorf("%d inserts, want 1", n)
	}
	for _, id := range []string{draft, standIn, malformed} {
		if m := env.gm.Messages[id]; m.Trashed {
			t.Errorf("%s must stay untouched", id)
		}
	}
	if env.state(t, bridged) != stateIgnored {
		t.Error("a bridge copy is recorded so the next run skips it without a Get")
	}
	if env.state(t, known) != stateSent {
		t.Error("existing store records must be kept")
	}
	mustContain(t, env.bodies[0], "1 rewritten", "4 skipped", "1 malformed", malformed+": malformed message")
	if done, _ := env.b.store.BackfillDone("a1", "label:"+backfillLabel); done.IsZero() {
		t.Error("malformed messages do not keep the backfill from completing")
	}
}

func TestBackfillSentMail(t *testing.T) {
	env := backfillEnv(t, func(c *Config) { c.RewriteMode = RewriteFull })
	raw := crlf(`From: Paul Geeser <OLD1@aol.com>
Reply-To: Paul Geeser <old1@aol.com>
To: Max Mustermann <max@example.com>
Cc: jane@example.com, Paul <old1@aol.com>
Date: Mon, 1 Jan 2018 10:00:00 +0000
Message-ID: <sent@aol.com>
Subject: Hi

Body
`)
	orig := env.gm.Add(raw, []string{"SENT", backfillLabel}, "t-sent")

	env.backfill(t)

	cpID := env.copies(t)["<sent@aol.com>"][0]
	cp := env.gm.Messages[cpID]
	if !slices.Equal(cp.Labels, []string{"SENT", backfillLabel, "Label_1"}) || cp.ThreadID != "t-sent" {
		t.Errorf("labels %v, thread %s", cp.Labels, cp.ThreadID)
	}
	h, _ := readHeader(cp.Raw)
	if got := h.Get("From"); got != "Paul Geeser <OLD1@aol.com>" {
		t.Errorf("From = %s, want unchanged even in full mode", got)
	}
	if got := h.Get("Reply-To"); got != "Paul Geeser <old1@aol.com>" {
		t.Errorf("Reply-To = %s, want unchanged", got)
	}
	if got := addrList(t, h, "To"); got != `"Max Mustermann (via AOL1)" <`+env.token(t, "a1", "max@example.com")+`>` {
		t.Errorf("To = %s", got)
	}
	if got := addrList(t, h, "Cc"); got != `"jane@example.com (via AOL1)" <`+env.token(t, "a1", "jane@example.com")+`>, "Paul (via AOL1)" <paul+a1.self@gmail.com>` {
		t.Errorf("Cc = %s", got)
	}
	if !env.gm.Messages[orig].Trashed {
		t.Error("original must be trashed")
	}
	mustContain(t, env.bodies[0], "1 rewritten (1 sent)")

	// The relay must never send a backfilled copy, even before it is recorded.
	again := env.gm.AddSent(cp.Raw)
	if err := env.b.process(again); err != nil {
		t.Fatal(err)
	}
	if env.state(t, again) != stateIgnored || len(env.sender.sent) != 0 {
		t.Errorf("state %q, sent %d", env.state(t, again), len(env.sender.sent))
	}
}

func TestBackfillDryRun(t *testing.T) {
	env := backfillEnv(t, func(c *Config) { c.DryRun = new(true) })
	id := env.gm.Add(backfillMail(1), []string{"INBOX", backfillLabel}, "")
	before := slices.Clone(env.gm.Messages[id].Raw)

	env.backfill(t)

	if len(env.gm.Calls) != 0 {
		t.Errorf("dry run changed Gmail: %v", env.gm.Calls)
	}
	if len(env.gm.Messages) != 1 || string(env.gm.Messages[id].Raw) != string(before) {
		t.Error("dry run must leave messages as they are")
	}
	if _, seen, _ := env.b.store.Outbound(id); seen {
		t.Error("dry run must not write outbound records")
	}
	if _, ok, _ := env.b.store.Token("a1", env.b.tokens.Token("a1", "max@example.com")); ok {
		t.Error("dry run must not record tokens")
	}
	if done, _ := env.b.store.BackfillDone("a1", "label:"+backfillLabel); !done.IsZero() {
		t.Error("a dry run must not mark the backfill complete")
	}
	if len(env.notes) != 0 {
		t.Errorf("dry run notified: %v", env.notes)
	}

	// Switching DryRun off runs the real backfill.
	*env.b.cfg.DryRun = false
	env.backfill(t)
	if !env.gm.Messages[id].Trashed {
		t.Error("real run must rewrite after a dry run")
	}
}

func TestBackfillRetriesRateLimits(t *testing.T) {
	env := backfillEnv(t, nil)
	env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	env.gm.GetErrs = []error{&googleapi.Error{Code: 503}}
	env.gm.InsertErrs = []error{
		&googleapi.Error{Code: 429},
		&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}},
	}

	env.backfill(t)

	if got := len(env.trashed()); got != 1 {
		t.Fatalf("trashed %d, want 1 after retries; calls %v", got, env.gm.Calls)
	}
	if len(env.copies(t)) != 1 {
		t.Error("want exactly one copy")
	}
}

func TestBackfillResume(t *testing.T) {
	env := backfillEnv(t, nil)
	first := env.gm.Add(backfillMail(3), []string{backfillLabel}, "")
	second := env.gm.Add(backfillMail(2), []string{backfillLabel}, "")
	third := env.gm.Add(backfillMail(1), []string{backfillLabel}, "")

	// The first insert succeeds, then Gmail keeps rate-limiting the second
	// message beyond every retry: the run stops there.
	env.gm.InsertErrs = append([]error{nil}, slices.Repeat([]error{&googleapi.Error{Code: 429}}, len(env.b.apiRetryDelays)+1)...)

	env.backfill(t)
	if got := env.trashed(); !slices.Equal(got, []string{first}) {
		t.Fatalf("first run trashed %v, want %s", got, first)
	}
	if done, _ := env.b.store.BackfillDone("a1", "label:"+backfillLabel); !done.IsZero() {
		t.Fatal("an aborted run must not be marked complete")
	}
	mustContain(t, env.notes[0], "aborted")

	env.backfill(t)
	if got := env.trashed(); !slices.Equal(got, []string{first, second, third}) {
		t.Fatalf("trashed %v", got)
	}
	for mid, ids := range env.copies(t) {
		if len(ids) != 1 {
			t.Errorf("%s has copies %v", mid, ids)
		}
	}
	if len(env.copies(t)) != 3 {
		t.Errorf("copies = %v", env.copies(t))
	}
}

// A run killed between insert and trash leaves the original in
// stateBackfilling; the next run finishes it without a second copy, even
// when Gmail filed the copy in another thread and the original no longer
// matches the query.
func TestBackfillResumeAfterCrashMidInsert(t *testing.T) {
	env := backfillEnv(t, nil)
	raw := backfillMail(1)
	orig := env.gm.Add(raw, []string{"INBOX"}, "t1")
	rewritten, err := rewrite(raw, RewriteReplyTo, func(a *mail.Address) (*mail.Address, error) {
		return env.b.tokenize(env.b.accounts["a1"], a)
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &backfillRun{b: env.b, acct: env.b.accounts["a1"]}
	h, _ := readHeader(raw)
	if rewritten, err = run.stamp(rewritten, orig, h); err != nil {
		t.Fatal(err)
	}
	// An earlier bridge import of the same mail must not count as the copy.
	twin := env.gm.Add(append([]byte("X-Bridge-Original-From: Max Mustermann <max@example.com>\r\n"), raw...), []string{"INBOX"}, "t1")
	// Nor may a copy that is in Trash.
	trashedCopy := env.gm.Add(rewritten, []string{"TRASH"}, "t-other")
	env.gm.Messages[trashedCopy].Trashed = true
	cp := env.gm.Add(rewritten, []string{"INBOX", backfillLabel}, "t-other")
	if err := env.b.store.SetOutbound(orig, OutboundRecord{State: stateBackfilling, Alias: "a1", MessageID: trashedCopy}); err != nil {
		t.Fatal(err)
	}

	env.backfill(t)

	if slices.Contains(env.gm.Calls, "insert") {
		t.Errorf("must not insert a second copy: %v", env.gm.Calls)
	}
	if env.gm.Messages[twin].Trashed {
		t.Error("the earlier import must be left alone")
	}
	if !env.gm.Messages[orig].Trashed || env.state(t, orig) != stateIgnored || env.state(t, cp) != stateIgnored {
		t.Errorf("trashed %v, states %q %q", env.gm.Messages[orig].Trashed, env.state(t, orig), env.state(t, cp))
	}
}

// Gmail may store a message and still answer with a server error. The run
// must not insert again; the next start finds the stored copy.
func TestBackfillAmbiguousInsertIsNotRetried(t *testing.T) {
	env := backfillEnv(t, nil)
	orig := env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	env.gm.InsertLost = []error{&googleapi.Error{Code: 503}}
	env.gm.IndexLag = true // the stored copy is not searchable yet

	env.backfill(t)
	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 {
		t.Fatalf("%d inserts, want 1", n)
	}
	if env.gm.Messages[orig].Trashed {
		t.Fatal("original must stay until its copy is confirmed")
	}
	mustContain(t, env.bodies[0], "insert outcome unknown")

	env.gm.Reindex()
	env.backfill(t)
	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 {
		t.Errorf("%d inserts, want the lost copy reused", n)
	}
	if !env.gm.Messages[orig].Trashed || len(env.copies(t)) != 1 {
		t.Errorf("trashed %v, copies %v", env.gm.Messages[orig].Trashed, env.copies(t))
	}
}

// Without a Message-ID the copy cannot be searched for; the id recorded
// after the insert is used instead.
func TestBackfillDiscardFailureResumesWithRecordedCopy(t *testing.T) {
	env := backfillEnv(t, nil)
	raw := crlf("From: Max <max@example.com>\nTo: old1@aol.com\nSubject: no id\nDate: Mon, 1 Jan 2018 10:00:00 +0000\n\nx\n")
	orig := env.gm.Add(raw, []string{backfillLabel}, "")
	env.gm.TrashErr = &googleapi.Error{Code: 400}

	env.backfill(t)
	mustContain(t, env.notes[0], "aborted")
	mustContain(t, env.bodies[0], "Stopped by: "+orig+": discard original")

	env.gm.TrashErr = nil
	env.backfill(t)
	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 {
		t.Errorf("%d inserts, want 1", n)
	}
	if !env.gm.Messages[orig].Trashed || env.state(t, orig) != stateIgnored {
		t.Errorf("original trashed %v, state %q", env.gm.Messages[orig].Trashed, env.state(t, orig))
	}
	if done, _ := env.b.store.BackfillDone("a1", "label:"+backfillLabel); done.IsZero() {
		t.Error("want the backfill complete")
	}
}

// The relay's own lost insert case for mail without a Message-ID: the copy
// gets a derived one, so the next start still finds it.
func TestBackfillAmbiguousInsertWithoutMessageID(t *testing.T) {
	env := backfillEnv(t, nil)
	orig := env.gm.Add(crlf("From: Max <max@example.com>\nTo: old1@aol.com\nSubject: no id\n\nx\n"), []string{backfillLabel}, "")
	env.gm.InsertLost = []error{&googleapi.Error{Code: 502}}

	env.backfill(t)
	env.backfill(t)

	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 {
		t.Errorf("%d inserts, want 1", n)
	}
	if !env.gm.Messages[orig].Trashed {
		t.Error("original must be finished on the next start")
	}
	if got := env.copies(t); len(got) != 1 || len(got["<backfill.a1."+orig+"@bridge.invalid>"]) != 1 {
		t.Errorf("copies %v", got)
	}
}

func TestBackfillAmbiguousInsertWithEmptyMessageID(t *testing.T) {
	env := backfillEnv(t, nil)
	orig := env.gm.Add(crlf("From: Max <max@example.com>\nTo: old1@aol.com\nMessage-ID: <>\nSubject: empty id\n\nx\n"), []string{backfillLabel}, "")
	env.gm.InsertLost = []error{&googleapi.Error{Code: 502}}

	env.backfill(t)
	env.backfill(t)

	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 || !env.gm.Messages[orig].Trashed {
		t.Errorf("%d inserts, trashed %v", n, env.gm.Messages[orig].Trashed)
	}
}

func TestBackfillDryRunReportsInterruptedCopy(t *testing.T) {
	env := backfillEnv(t, func(c *Config) { c.DryRun = new(true) })
	raw := backfillMail(1)
	orig := env.gm.Add(raw, []string{backfillLabel}, "")
	run := &backfillRun{b: env.b, acct: env.b.accounts["a1"]}
	h, _ := readHeader(raw)
	rewritten, err := rewrite(raw, RewriteReplyTo, func(a *mail.Address) (*mail.Address, error) {
		t, _ := env.b.tokenAddress(env.b.accounts["a1"], a)
		return t, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rewritten, err = run.stamp(rewritten, orig, h); err != nil {
		t.Fatal(err)
	}
	cp := env.gm.Add(rewritten, nil, "")
	if err := env.b.store.SetOutbound(orig, OutboundRecord{State: stateBackfilling, Alias: "a1", MessageID: cp}); err != nil {
		t.Fatal(err)
	}

	env.backfill(t)

	if len(env.gm.Calls) != 0 || env.gm.Messages[orig].Trashed {
		t.Errorf("dry run changed Gmail: %v", env.gm.Calls)
	}
	if r, _, _ := env.b.store.Outbound(orig); r.State != stateBackfilling || r.MessageID != cp {
		t.Errorf("dry run changed the record: %+v", r)
	}
	if _, seen, _ := env.b.store.Outbound(cp); seen {
		t.Error("dry run must not record the copy")
	}
}

func TestBackfillDiscardFailureStopsAllAccounts(t *testing.T) {
	env := backfillEnv(t, func(c *Config) { c.DeleteOriginal = true })
	env.b.accounts["a2"].BackfillQuery = "label:L2"
	env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	other := env.gm.Add(backfillMail(2), []string{"L2"}, "")
	env.gm.DeleteErr = &googleapi.Error{Code: 403, Message: "insufficientPermissions"}

	env.backfill(t)

	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 {
		t.Errorf("%d inserts, want the run to stop after the first", n)
	}
	if _, seen, _ := env.b.store.Outbound(other); seen {
		t.Error("the next account must not start")
	}
}

func TestClaimBackfill(t *testing.T) {
	env := newTestEnv(t, nil)
	s := env.b.store
	for id, rec := range map[string]*OutboundRecord{
		"new":      nil,
		"unrouted": {State: stateUnrouted},
		"mine":     {State: stateBackfilling, Alias: "a1"},
		"imported": {State: stateIgnored},
		"theirs":   {State: stateBackfilling, Alias: "a2"},
	} {
		if rec != nil {
			if err := s.SetOutbound(id, *rec); err != nil {
				t.Fatal(err)
			}
		}
		ok, err := s.ClaimBackfill(id, "a1", env.b.now())
		want := id == "new" || id == "unrouted" || id == "mine"
		if err != nil || ok != want {
			t.Errorf("%s: claimed %v, %v; want %v", id, ok, err, want)
		}
	}
}

// A pending original is finished before newer matches get a copy, and a
// copy found by search is remembered before the original is discarded.
func TestBackfillPendingFirst(t *testing.T) {
	env := backfillEnv(t, nil)
	pending := env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	env.gm.InsertLost = []error{&googleapi.Error{Code: 500}}
	env.gm.IndexLag = true
	env.backfill(t) // pending's copy exists, its outcome is unknown
	env.gm.Reindex()

	newer := env.gm.Add(backfillMail(5), []string{backfillLabel}, "")
	env.gm.TrashErr = &googleapi.Error{Code: 400}
	env.backfill(t)

	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 {
		t.Errorf("%d inserts, want none for %s while %s is stuck", n, newer, pending)
	}
	r, _, _ := env.b.store.Outbound(pending)
	if r.State != stateBackfilling || r.MessageID == "" {
		t.Errorf("record %+v: the recovered copy id must be kept", r)
	}
}

func TestBackfillAmbiguousInsertWithCommentedMessageID(t *testing.T) {
	env := backfillEnv(t, nil)
	orig := env.gm.Add(crlf("From: Max <max@example.com>\nTo: old1@aol.com\nMessage-ID: <c@example.com> (legacy)\nSubject: c\n\nx\n"), []string{backfillLabel}, "")
	env.gm.InsertLost = []error{&googleapi.Error{Code: 502}}

	env.backfill(t)
	env.backfill(t)

	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 || !env.gm.Messages[orig].Trashed {
		t.Errorf("%d inserts, trashed %v", n, env.gm.Messages[orig].Trashed)
	}
}

// A pending original is finished even when its account's current query
// already completed, e.g. after switching back from a wider query.
func TestBackfillPendingUnderCompletedQuery(t *testing.T) {
	env := backfillEnv(t, nil)
	q := "label:" + backfillLabel
	if err := env.b.store.SetBackfillDone("a1", q, env.b.now()); err != nil {
		t.Fatal(err)
	}
	orig := env.gm.Add(backfillMail(1), []string{"INBOX"}, "")
	if err := env.b.store.SetOutbound(orig, OutboundRecord{State: stateBackfilling, Alias: "a1"}); err != nil {
		t.Fatal(err)
	}
	other := env.gm.Add(backfillMail(2), []string{backfillLabel}, "")

	env.backfill(t)

	if !env.gm.Messages[orig].Trashed || len(env.copies(t)) != 1 {
		t.Errorf("pending original not finished: calls %v", env.gm.Calls)
	}
	if env.gm.Messages[other].Trashed {
		t.Error("a completed query must not be searched again")
	}
}

// The Google client may retry a failed upload itself and report only the
// retry's rate limit. The copy stored by the first upload is found before
// inserting again.
// Gmail answers a stored insert with a server error; the copy is found in
// the same run once the search index caught up.
func TestBackfillAmbiguousInsertFoundInRun(t *testing.T) {
	env := backfillEnv(t, nil)
	orig := env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	env.gm.InsertLost = []error{&googleapi.Error{Code: 502}}
	env.backfill(t)
	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 || !env.gm.Messages[orig].Trashed {
		t.Errorf("%d inserts, trashed %v", n, env.gm.Messages[orig].Trashed)
	}
}

func TestBackfillRateLimitAfterStoredUpload(t *testing.T) {
	env := backfillEnv(t, nil)
	orig := env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	env.gm.InsertLost = []error{&googleapi.Error{Code: 429}}

	env.backfill(t)

	if n := strings.Count(strings.Join(env.gm.Calls, " "), "insert"); n != 1 {
		t.Errorf("%d inserts, want 1", n)
	}
	if !env.gm.Messages[orig].Trashed || len(env.copies(t)) != 1 {
		t.Errorf("trashed %v, copies %v", env.gm.Messages[orig].Trashed, env.copies(t))
	}
}

func TestBackfillPendingOriginalGone(t *testing.T) {
	env := backfillEnv(t, nil)
	if err := env.b.store.SetOutbound("gone", OutboundRecord{State: stateBackfilling, Alias: "a1"}); err != nil {
		t.Fatal(err)
	}
	env.backfill(t)
	if env.state(t, "gone") != stateIgnored {
		t.Errorf("state %q: a deleted pending original must stop being resumed", env.state(t, "gone"))
	}
}

func TestBackfillLeavesOtherAccountsPendingOriginal(t *testing.T) {
	env := backfillEnv(t, nil)
	id := env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	if err := env.b.store.SetOutbound(id, OutboundRecord{State: stateBackfilling, Alias: "a2"}); err != nil {
		t.Fatal(err)
	}
	env.backfill(t)
	if len(env.gm.Calls) != 0 || env.state(t, id) != stateBackfilling {
		t.Errorf("calls %v, state %q", env.gm.Calls, env.state(t, id))
	}
}

func TestRelayKeepsPendingBackfill(t *testing.T) {
	env := backfillEnv(t, nil)
	id := env.gm.AddSent(crlf("From: Paul <old1@aol.com>\nTo: max@example.com\nSubject: x\n\nx\n"))
	if err := env.b.store.SetOutbound(id, OutboundRecord{State: stateBackfilling, Alias: "a1"}); err != nil {
		t.Fatal(err)
	}
	// The relay read the message before the backfill claimed it.
	if err := env.b.store.AddOutbound(id, OutboundRecord{State: stateUnrouted}); err != nil {
		t.Fatal(err)
	}
	if env.state(t, id) != stateBackfilling {
		t.Errorf("state %q", env.state(t, id))
	}
}

func TestBackfillAccountsInOrder(t *testing.T) {
	env := backfillEnv(t, nil)
	env.b.accounts["a1"].BackfillQuery = "label:L1 after:2018/01/01"
	env.b.accounts["a2"].BackfillQuery = "label:L2"
	one := env.gm.Add(backfillMail(1), []string{"L1"}, "")
	two := env.gm.Add(crlf("From: Max <max@example.com>\nTo: old2@aol.com\nSubject: x\nDate: Tue, 2 Jan 2018 10:00:00 +0000\n\nx\n"), []string{"L2"}, "")

	env.backfill(t)

	if got := env.trashed(); !slices.Equal(got, []string{one, two}) {
		t.Errorf("trashed %v, want a1 then a2", got)
	}
	for _, id := range env.copies(t) {
		h, _ := readHeader(env.gm.Messages[id[0]].Raw)
		want := "a1/" + one
		if strings.Contains(h.Get("Subject"), "x") {
			want = "a2/" + two
		}
		if got := h.Get(backfillOf); got != want {
			t.Errorf("%s = %q, want %q", backfillOf, got, want)
		}
	}
	for alias, q := range map[string]string{"a1": "label:L1 after:2018/01/01", "a2": "label:L2"} {
		if done, _ := env.b.store.BackfillDone(alias, q); done.IsZero() {
			t.Errorf("%s not complete", alias)
		}
	}
}

func TestBackfillDeleteOriginal(t *testing.T) {
	env := backfillEnv(t, func(c *Config) { c.DeleteOriginal = true })
	orig := env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	env.backfill(t)
	if !env.gm.Messages[orig].Deleted || len(env.trashed()) != 0 {
		t.Errorf("original must be deleted, calls %v", env.gm.Calls)
	}
}

func TestBackfillDiscardNotFoundIsDone(t *testing.T) {
	env := backfillEnv(t, nil)
	orig := env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	env.gm.TrashErr = &googleapi.Error{Code: 404}
	env.backfill(t)
	if env.state(t, orig) != stateIgnored {
		t.Errorf("state %q", env.state(t, orig))
	}
	if done, _ := env.b.store.BackfillDone("a1", "label:"+backfillLabel); done.IsZero() {
		t.Error("an original that is already gone does not fail the run")
	}
}

func TestBackfillNoFromIsMalformed(t *testing.T) {
	env := backfillEnv(t, nil)
	id := env.gm.Add(crlf("Reply-To: max@example.com\nTo: carl@example.com\nSubject: x\n\nx\n"), []string{"SENT", backfillLabel}, "")
	env.backfill(t)
	if len(env.gm.Calls) != 0 {
		t.Errorf("calls %v", env.gm.Calls)
	}
	mustContain(t, env.bodies[0], id+": malformed message: no From header")
}

// Sent mail the relay saw without token recipients is not bridge mail.
func TestBackfillRewritesMailTheRelayLeftAlone(t *testing.T) {
	env := backfillEnv(t, nil)
	id := env.gm.AddSent(crlf("From: Paul <old1@aol.com>\nTo: max@example.com\nSubject: x\nDate: Mon, 1 Jan 2018 10:00:00 +0000\n\nx\n"), backfillLabel)
	if err := env.b.process(id); err != nil {
		t.Fatal(err)
	}
	if env.state(t, id) != stateUnrouted {
		t.Fatalf("relay state %q", env.state(t, id))
	}
	env.backfill(t)
	if !env.gm.Messages[id].Trashed || len(env.copies(t)) != 1 {
		t.Errorf("trashed %v, copies %v", env.gm.Messages[id].Trashed, env.copies(t))
	}
}

func TestBackfillSearchFailureNotifies(t *testing.T) {
	env := backfillEnv(t, nil)
	env.gm.SearchErr = &googleapi.Error{Code: 400, Message: "Invalid query"}
	env.backfill(t)
	if len(env.notes) != 1 || !strings.Contains(env.bodies[0], "Invalid query") {
		t.Errorf("notes %v %v", env.notes, env.bodies)
	}
}

// A failing query does not strand originals whose copy is already stored.
func TestBackfillSearchFailureStillFinishesPending(t *testing.T) {
	env := backfillEnv(t, nil)
	raw := backfillMail(1)
	orig := env.gm.Add(raw, []string{"INBOX"}, "")
	run := &backfillRun{b: env.b, acct: env.b.accounts["a1"]}
	h, _ := readHeader(raw)
	rewritten, err := rewrite(raw, RewriteReplyTo, func(a *mail.Address) (*mail.Address, error) {
		return env.b.tokenize(env.b.accounts["a1"], a)
	})
	if err != nil {
		t.Fatal(err)
	}
	if rewritten, err = run.stamp(rewritten, orig, h); err != nil {
		t.Fatal(err)
	}
	cp := env.gm.Add(rewritten, []string{"INBOX"}, "")
	if err := env.b.store.SetOutbound(orig, OutboundRecord{State: stateBackfilling, Alias: "a1", MessageID: cp}); err != nil {
		t.Fatal(err)
	}
	env.gm.SearchErr = &googleapi.Error{Code: 400, Message: "Invalid query"}

	env.backfill(t)

	if !env.gm.Messages[orig].Trashed {
		t.Error("pending original must be finished despite the failed search")
	}
	if done, _ := env.b.store.BackfillDone("a1", "label:"+backfillLabel); !done.IsZero() {
		t.Error("a run without its query must not be marked complete")
	}
}

func TestBackfillInsertRejectedIsRetriedNextStart(t *testing.T) {
	env := backfillEnv(t, nil)
	id := env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	env.gm.InsertErrs = []error{&googleapi.Error{Code: 400, Message: "Invalid label"}}

	env.backfill(t)
	if env.gm.Messages[id].Trashed {
		t.Fatal("original must stay when insert fails")
	}
	mustContain(t, env.bodies[0], "1 failed", id+": insert:")

	env.backfill(t)
	if !env.gm.Messages[id].Trashed || len(env.copies(t)) != 1 {
		t.Error("next start must retry the failed message")
	}
}

func TestBackfillStopsOnCancel(t *testing.T) {
	env := backfillEnv(t, nil)
	env.gm.Add(backfillMail(1), []string{backfillLabel}, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env.b.Backfill(ctx)
	if len(env.trashed()) != 0 {
		t.Error("a cancelled backfill must not touch mail")
	}
}

func TestIsRetryable(t *testing.T) {
	cases := map[error]bool{
		&googleapi.Error{Code: 429}: true,
		&googleapi.Error{Code: 500}: true,
		&googleapi.Error{Code: 404}: false,
		&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}}}:       true,
		&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "insufficientPermissions"}}}: false,
		context.Canceled: false,
	}
	for err, want := range cases {
		if got := isRetryable(err); got != want {
			t.Errorf("isRetryable(%v) = %v, want %v", err, got, want)
		}
	}
	// Only outright refusals are safe to repeat for a non-idempotent insert.
	if isRateLimited(&googleapi.Error{Code: 503}) || !isRateLimited(&googleapi.Error{Code: 429}) {
		t.Error("isRateLimited must accept 429 and reject 5xx")
	}
}

func TestHeaderDiff(t *testing.T) {
	h, _ := readHeader(crlf("To: a@example.com\nCc: b@example.com\n\n"))
	got := headerDiff(h, crlf("To: x@example.com\nCc: b@example.com\n\n"))
	if got != `To: "a@example.com" -> "x@example.com"` {
		t.Errorf("diff = %s", got)
	}
}
