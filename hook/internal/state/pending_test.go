package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingStore(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	now := time.Now()
	if p, err := st.LoadPending("claude", "a"); p != nil || err != nil {
		t.Fatalf("missing entry: %v %v", p, err)
	}
	for i, k := range []string{"newer", "older"} {
		p := Pending{Tool: "codex", Key: k, Received: now.Add(-time.Duration(i) * time.Hour), Attempts: i + 1, Payload: json.RawMessage(`{"session_id":"` + k + `"}`)}
		if err := st.SavePending(p); err != nil {
			t.Fatal(err)
		}
	}
	path := st.PendingPath("codex", "older")
	if filepath.Base(path) != "codex-"+Key("older")+".json" || filepath.Base(filepath.Dir(path)) != "pending" {
		t.Fatalf("path %s", path)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	os.WriteFile(filepath.Join(filepath.Dir(path), "broken.json"), []byte("{"), 0o600)
	if st.PendingCount() != 3 {
		t.Fatalf("count %d", st.PendingCount())
	}
	list, corrupt := st.ListPending()
	if corrupt != 1 || len(list) != 2 || list[0].Key != "older" || list[1].Key != "newer" || list[0].Attempts != 2 {
		t.Fatalf("list %+v corrupt %d", list, corrupt)
	}
	if st.PendingCount() != 2 {
		t.Fatal("corrupt entry not removed")
	}
	if err := st.RemovePending("codex", "older"); err != nil || st.RemovePending("codex", "older") != nil {
		t.Fatal("remove")
	}
	if p, _ := st.LoadPending("codex", "newer"); p == nil || string(p.Payload) != `{"session_id":"newer"}` {
		t.Fatalf("load %+v", p)
	}
}
