package bridge

import (
	"errors"
	"slices"
	"testing"

	"github.com/pauldev20/turbogmailbridge/internal/mocks"
)

func inbox(t *testing.T, env *testEnv, alias string) *Inbox {
	t.Helper()
	in, err := env.b.Inbox(alias, &mocks.MockInbox{})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestInboundImportInsertTrash(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gm.ImportLabels = []string{"SPAM", "CATEGORY_PROMOTIONS", "SENT"}

	if err := inbox(t, env, "a1").DoImport(crlf(simpleMail), true, false, "INBOX"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"import", "insert", "trash:m1"}; !slices.Equal(env.gm.Calls, want) {
		t.Fatalf("calls = %v, want %v", env.gm.Calls, want)
	}

	orig, copy := env.gm.Messages["m1"], env.gm.Messages["m2"]
	if string(orig.Raw) != string(crlf(simpleMail)) {
		t.Error("original must be imported unmodified")
	}
	if copy.ThreadID != orig.ThreadID {
		t.Errorf("thread = %s, want %s", copy.ThreadID, orig.ThreadID)
	}
	want := []string{"INBOX", "UNREAD", "SPAM", "CATEGORY_PROMOTIONS", "Label_1"}
	if !slices.Equal(copy.Labels, want) {
		t.Errorf("labels = %v, want %v (SENT must be dropped)", copy.Labels, want)
	}

	h, _ := readHeader(copy.Raw)
	maxTok := env.token(t, "a1", "max@example.com")
	if got := addrList(t, h, "Reply-To"); got != `"Max Mustermann (via AOL1)" <`+maxTok+`>` {
		t.Errorf("Reply-To = %s", got)
	}
	if got := addrList(t, h, "To"); got != `"Paul (via AOL1)" <paul+a1.self@gmail.com>, "carl@example.com (via AOL1)" <`+env.token(t, "a1", "carl@example.com")+`>` {
		t.Errorf("To = %s", got)
	}
	if r, ok, _ := env.b.store.Token("a1", maxTok[len("paul+a1."):len("paul+a1.")+tokenLen]); !ok || r.Name != "Max Mustermann" {
		t.Errorf("token record = %+v, %v", r, ok)
	}
}

func TestInboundDeleteOriginal(t *testing.T) {
	env := newTestEnv(t, func(c *Config) { c.DeleteOriginal = true })
	if err := inbox(t, env, "a2").DoImport(crlf(simpleMail), false, true, "INBOX"); err != nil {
		t.Fatal(err)
	}
	if !env.gm.Messages["m1"].Deleted {
		t.Error("original must be deleted")
	}
}

func TestInboundInsertFailureDiscardsOriginal(t *testing.T) {
	env := newTestEnv(t, nil)
	boom := errors.New("insert failed")
	env.gm.InsertErr = boom

	err := inbox(t, env, "a1").DoImport(crlf(simpleMail), false, true, "INBOX")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if !env.gm.Messages["m1"].Trashed {
		t.Error("original must be trashed so the retry leaves no duplicate")
	}
}

func TestInboundFetchesLabelsWhenImportOmitsThem(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gm.ImportNoLabels = true
	env.gm.ImportLabels = []string{"SPAM"}
	if err := inbox(t, env, "a2").DoImport(crlf(simpleMail), false, true, "INBOX"); err != nil {
		t.Fatal(err)
	}
	if got := env.gm.Messages["m2"].Labels; !slices.Equal(got, []string{"INBOX", "UNREAD", "SPAM"}) {
		t.Errorf("labels = %v", got)
	}

	env.gm.LabelsErr = errors.New("get failed")
	if err := inbox(t, env, "a2").DoImport(crlf(simpleMail), false, true, "INBOX"); !errors.Is(err, env.gm.LabelsErr) {
		t.Fatalf("err = %v", err)
	}
	if !env.gm.Messages["m3"].Trashed {
		t.Error("original must be trashed when its labels cannot be read")
	}
}

func TestInboundRewriteFailureIsPermanent(t *testing.T) {
	env := newTestEnv(t, nil)
	err := inbox(t, env, "a1").DoImport(crlf("Subject: no from\n\nx\n"), false, true, "INBOX")
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v", err)
	}
	if len(env.gm.Calls) != 0 {
		t.Errorf("nothing may be imported, calls = %v", env.gm.Calls)
	}
}

func TestInboundDiscardFailureDoesNotReimport(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gm.TrashErr = errors.New("insufficient scope")
	if err := inbox(t, env, "a1").DoImport(crlf(simpleMail), false, true, "INBOX"); err != nil {
		t.Fatalf("rewritten copy is stored, want success, got %v", err)
	}
	if len(env.notes) != 1 {
		t.Errorf("want one notification, got %v", env.notes)
	}
	for _, id := range []string{"m1", "m2"} {
		if env.state(t, id) != stateIgnored {
			t.Errorf("%s must be excluded from the outbound relay", id)
		}
	}
}

func TestInboundStoreFailureIsRetryable(t *testing.T) {
	env := newTestEnv(t, nil)
	env.b.store.Close()
	err := inbox(t, env, "a1").DoImport(crlf(simpleMail), false, true, "INBOX")
	if err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want retryable", err)
	}
}

func TestInboxExposesNextForNotifications(t *testing.T) {
	env := newTestEnv(t, nil)
	next := &mocks.MockInbox{}
	in, _ := env.b.Inbox("a1", next)
	if in.Next != next {
		t.Fatal("Next must be the plain importer")
	}
	if _, err := env.b.Inbox("zz", next); err == nil {
		t.Fatal("unknown alias must fail")
	}
}
