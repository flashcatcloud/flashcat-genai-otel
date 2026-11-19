package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

func TestPathAndRoundTrip(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	p := st.Path("claude", "session-1")
	if filepath.Dir(p) != filepath.Join(st.Dir, "claude") || len(filepath.Base(p)) != 16+len(".json") || Key("session-1") != filepath.Base(p)[:16] {
		t.Fatalf("path %s", p)
	}
	s, err := st.Acquire("claude", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Prev() != nil {
		t.Fatal("fresh state not nil")
	}
	if err := s.Save([]byte(`{"offset":10}`)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v %v", fi.Mode(), err)
	}
	if _, err := os.Stat(p[:len(p)-5] + ".lock"); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
	s2, _ := st.Acquire("claude", "session-1")
	defer s2.Close()
	if string(s2.Prev()) != `{"offset":10}` {
		t.Fatalf("prev = %s", s2.Prev())
	}
	if _, err := st.Acquire("claude", ""); err == nil {
		t.Fatal("empty id accepted")
	}
}

func TestLockExcludes(t *testing.T) {
	st := Store{Dir: t.TempDir(), LockTimeout: 100 * time.Millisecond}
	a, err := st.Acquire("codex", "s")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := st.Acquire("codex", "s"); err == nil {
		t.Fatal("second Acquire got the lock")
	} else if time.Since(start) < 90*time.Millisecond {
		t.Fatalf("gave up after %v", time.Since(start))
	}
	// Other sessions are independent.
	other, err := st.Acquire("codex", "other")
	if err != nil {
		t.Fatal(err)
	}
	other.Close()

	done := make(chan error, 1)
	go func() {
		s, err := Store{Dir: st.Dir, LockTimeout: 2 * time.Second}.Acquire("codex", "s")
		if err == nil {
			s.Close()
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	a.Close()
	if err := <-done; err != nil {
		t.Fatalf("waiter did not get the lock after release: %v", err)
	}
}

func TestApplySemantics(t *testing.T) {
	turn := []model.Turn{{TurnKey: "k"}}
	fail := errors.New("send failed")
	cases := []struct {
		name string
		res  model.CollectResult
		err  error
		want string // "" = no file
	}{
		{"no turns saves success state", model.CollectResult{StateOnSuccess: []byte("S"), StateOnFailure: []byte("F")}, nil, "S"},
		{"no turns ignores delivery error", model.CollectResult{StateOnSuccess: []byte("S")}, fail, "S"},
		{"delivered saves success state", model.CollectResult{Turns: turn, StateOnSuccess: []byte("S"), StateOnFailure: []byte("F")}, nil, "S"},
		{"failure keeps previous when nil", model.CollectResult{Turns: turn, StateOnSuccess: []byte("S")}, fail, "PREV"},
		{"failure saves failure state", model.CollectResult{Turns: turn, StateOnSuccess: []byte("S"), StateOnFailure: []byte("F")}, fail, "F"},
		{"empty success state clears", model.CollectResult{StateOnSuccess: nil}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := Store{Dir: t.TempDir()}
			s, _ := st.Acquire("cursor", "s")
			if err := s.Save([]byte("PREV")); err != nil {
				t.Fatal(err)
			}
			if err := s.Apply(c.res, c.err); err != nil {
				t.Fatal(err)
			}
			s.Close()
			b, err := os.ReadFile(st.Path("cursor", "s"))
			if c.want == "" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("state should be deleted, got %q", b)
				}
				return
			}
			if string(b) != c.want {
				t.Fatalf("state = %q, want %q", b, c.want)
			}
		})
	}
}
