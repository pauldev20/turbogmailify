package bridge

import "testing"

func (e *testEnv) state(t *testing.T, id string) string {
	t.Helper()
	r, _, err := e.b.store.Outbound(id)
	if err != nil {
		t.Fatal(err)
	}
	return r.State
}
