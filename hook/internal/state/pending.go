package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/config"
)

// Pending is a hook event whose delivery failed, kept so that any later hook
// call (of any session) can replay it: one-shot sessions such as `codex exec`
// or `claude -p` have no next event of their own. There is one entry per
// tool and session; a newer failure overwrites the payload.
type Pending struct {
	Tool        string          `json:"tool"`
	Key         string          `json:"key"` // the state key: session id or transcript path
	Received    time.Time       `json:"received"`
	Attempts    int             `json:"attempts"` // failed deliveries so far
	NextAttempt time.Time       `json:"next_attempt,omitempty"`
	Payload     json.RawMessage `json:"payload"`
}

func (s Store) pendingDir() string { return filepath.Join(s.Dir, "pending") }

// PendingPath is <dir>/pending/<tool>-<sha256(key)[:16]>.json.
func (s Store) PendingPath(tool, key string) string {
	return filepath.Join(s.pendingDir(), tool+"-"+Key(key)+".json")
}

// SavePending writes an entry atomically (mode 0600).
func (s Store) SavePending(p Pending) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(s.PendingPath(p.Tool, p.Key), b, 0o600)
}

// LoadPending returns the entry of a session, nil when there is none.
func (s Store) LoadPending(tool, key string) (*Pending, error) {
	b, err := os.ReadFile(s.PendingPath(tool, key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// RemovePending deletes the entry of a session; a missing entry is fine.
func (s Store) RemovePending(tool, key string) error {
	err := os.Remove(s.PendingPath(tool, key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ListPending returns all entries, oldest received first. Unreadable entries
// are deleted and counted in corrupt.
func (s Store) ListPending() (entries []Pending, corrupt int) {
	files, _ := filepath.Glob(filepath.Join(s.pendingDir(), "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var p Pending
		if json.Unmarshal(b, &p) != nil || p.Tool == "" || p.Key == "" || len(p.Payload) == 0 {
			_ = os.Remove(f)
			corrupt++
			continue
		}
		entries = append(entries, p)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].Received.Equal(entries[j].Received) {
			return entries[i].Received.Before(entries[j].Received)
		}
		return entries[i].Tool+entries[i].Key < entries[j].Tool+entries[j].Key
	})
	return entries, corrupt
}

// PendingCount is the number of entries waiting for a retry.
func (s Store) PendingCount() int {
	files, _ := filepath.Glob(filepath.Join(s.pendingDir(), "*.json"))
	n := 0
	for _, f := range files {
		if !strings.HasPrefix(filepath.Base(f), ".") {
			n++
		}
	}
	return n
}
