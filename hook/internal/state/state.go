// Package state stores each session's opaque parser state under the state
// directory, guarded by a file lock (CONTRACT §2).
package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/config"
	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/model"
)

// DefaultLockTimeout is how long Acquire waits for another hook process
// working on the same session.
const DefaultLockTimeout = 5 * time.Second

// Store is the state directory.
type Store struct {
	Dir         string
	LockTimeout time.Duration
}

// Session is a locked state file. Call Close to release the lock.
type Session struct {
	path string
	lock *flock.Flock
	prev []byte
}

// Key returns the file name stem: sha256(session or transcript path)[:16] in hex.
func Key(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])[:16]
}

// Path returns <dir>/<tool>/<key>.json.
func (s Store) Path(tool, id string) string {
	return filepath.Join(s.Dir, tool, Key(id)+".json")
}

// Acquire locks the state of one session (<key>.lock next to the state file)
// and loads it. id is the session id, or the transcript path when the
// payload has no session id.
func (s Store) Acquire(tool, id string) (*Session, error) {
	if id == "" {
		return nil, errors.New("state: empty session id")
	}
	p := s.Path(tool, id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	timeout := s.LockTimeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	lk := flock.New(p[:len(p)-len(".json")] + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ok, err := lk.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil || !ok {
		if err == nil {
			err = context.DeadlineExceeded
		}
		return nil, fmt.Errorf("state: lock %s: %w", filepath.Base(p), err)
	}
	b, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = lk.Unlock()
		return nil, err
	}
	return &Session{path: p, lock: lk, prev: b}, nil
}

// Prev is the stored state, nil when there is none.
func (s *Session) Prev() []byte { return s.prev }

// Save replaces the state atomically; empty data deletes it.
func (s *Session) Save(data []byte) error {
	if len(data) == 0 {
		err := os.Remove(s.path)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		s.prev = nil
		return err
	}
	if err := config.WriteFileAtomic(s.path, data, 0o600); err != nil {
		return err
	}
	s.prev = append([]byte(nil), data...)
	return nil
}

// Apply persists a parser result according to the delivery outcome
// (model.CollectResult): StateOnSuccess after delivery or when there were no
// turns; on delivery failure StateOnFailure, where nil keeps the previous
// state so the same turns are produced again.
func (s *Session) Apply(res model.CollectResult, deliveryErr error) error {
	if len(res.Turns) == 0 || deliveryErr == nil {
		return s.Save(res.StateOnSuccess)
	}
	if res.StateOnFailure == nil {
		return nil
	}
	return s.Save(res.StateOnFailure)
}

// Close releases the lock.
func (s *Session) Close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	return s.lock.Unlock()
}
