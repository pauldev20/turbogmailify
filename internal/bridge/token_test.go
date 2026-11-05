package bridge

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func testTokens(t *testing.T) *Tokens {
	t.Helper()
	tk, err := newTokens(bytes.Repeat([]byte{7}, keyLen), "Paul@Gmail.com")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestTokenStableAndScoped(t *testing.T) {
	tk := testTokens(t)
	a := tk.Token("a1", "max@example.com")
	if a != tk.Token("a1", "MAX@Example.com") {
		t.Error("token must ignore address case")
	}
	if a == tk.Token("a2", "max@example.com") {
		t.Error("token must differ per alias")
	}
	if a == tk.Token("a1", "moritz@example.com") {
		t.Error("token must differ per address")
	}
	if !regexp.MustCompile(`^[a-z2-7]{10}$`).MatchString(a) {
		t.Errorf("token %q has wrong format", a)
	}
	other, _ := newTokens(bytes.Repeat([]byte{8}, keyLen), "paul@gmail.com")
	if a == other.Token("a1", "max@example.com") {
		t.Error("token must depend on the key")
	}
}

func TestTokenAddressRoundTrip(t *testing.T) {
	tk := testTokens(t)
	tok := tk.Token("a1", "max@example.com")
	addr := tk.Address("a1", tok)
	if addr != "paul+a1."+tok+"@gmail.com" {
		t.Fatalf("Address = %s", addr)
	}
	for _, in := range []string{addr, "PAUL+A1." + tok + "@GMAIL.COM", tk.Address("a1", selfToken)} {
		alias, got, ok := tk.Parse(in)
		if !ok || alias != "a1" || (got != tok && got != selfToken) {
			t.Errorf("Parse(%s) = %s, %s, %v", in, alias, got, ok)
		}
	}
	for _, in := range []string{
		"paul@gmail.com",
		"paul+a1@gmail.com",
		"paul+a1." + tok + "@gmail.co",
		"xpaul+a1." + tok + "@gmail.com",
		"paul+a1." + tok[:9] + "@gmail.com",
		"paul+toolongalias." + tok + "@gmail.com",
	} {
		if _, _, ok := tk.Parse(in); ok {
			t.Errorf("Parse(%s) should fail", in)
		}
	}
}

func TestKeyCreatedOnceWithPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.key")
	k1, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := loadOrCreateKey(path)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatalf("key changed or failed: %v", err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	os.WriteFile(path, []byte("short"), 0o600)
	if _, err := loadOrCreateKey(path); err == nil {
		t.Error("truncated key must be rejected")
	}
}

func TestStoreTokens(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	t0 := time.Unix(1000, 0).UTC()
	if err := s.PutToken(TokenRecord{Alias: "a1", Token: "tok", Address: "max@example.com", Name: "Max", FirstSeen: t0}); err != nil {
		t.Fatal(err)
	}
	// Later sighting without a name keeps FirstSeen and the old name.
	if err := s.PutToken(TokenRecord{Alias: "a1", Token: "tok", Address: "MAX@example.com", FirstSeen: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.Token("a1", "tok")
	if err != nil || !ok || r.Name != "Max" || !r.FirstSeen.Equal(t0) {
		t.Fatalf("Token = %+v, %v, %v", r, ok, err)
	}
	if _, ok, _ := s.Token("a2", "tok"); ok {
		t.Error("tokens must be scoped by alias")
	}
	err = s.PutToken(TokenRecord{Alias: "a1", Token: "tok", Address: "other@example.com"})
	if !errors.Is(err, ErrTokenCollision) {
		t.Errorf("want collision, got %v", err)
	}
}

func TestStoreHistoryOutboundAndSends(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if id, _ := s.HistoryID(); id != 0 {
		t.Errorf("initial history id = %d", id)
	}
	s.SetHistoryID(42)
	if id, _ := s.HistoryID(); id != 42 {
		t.Errorf("history id = %d", id)
	}

	s.SetOutbound("m1", OutboundRecord{State: stateSent, MessageID: "x@aol.com"})
	if r, ok, _ := s.Outbound("m1"); !ok || r.State != stateSent || r.MessageID != "x@aol.com" {
		t.Errorf("outbound = %+v %v", r, ok)
	}

	now := time.Unix(10_000, 0)
	reserve := func(at time.Time) bool {
		ok, err := s.ReserveSend(at, 2)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !reserve(now.Add(-2*time.Hour)) || !reserve(now.Add(-30*time.Minute)) {
		t.Fatal("reservations under the limit must succeed")
	}
	if !reserve(now) {
		t.Error("entries older than an hour must not count")
	}
	if reserve(now.Add(time.Minute)) {
		t.Error("third attempt within an hour must be refused")
	}
}
