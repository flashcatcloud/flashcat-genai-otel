package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/config"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/state"
)

// Retry queue limits. Variables so tests can shrink them.
var (
	// retryBudget is the total time one hook call spends replaying pending
	// events after its own event.
	retryBudget = 2 * time.Second
	// retryMinSlice is the least time left for starting another replay.
	retryMinSlice = 250 * time.Millisecond
	// retryLockWait bounds the wait for a session another process holds.
	retryLockWait = 500 * time.Millisecond
	// pendingMaxAge and pendingMaxAttempts drop entries that keep failing.
	pendingMaxAge      = 7 * 24 * time.Hour
	pendingMaxAttempts = 50
	// retryBackoff is the pause after the n-th failed delivery of an entry
	// (n >= 2: the first replay of a live failure is immediate). It stops a
	// long outage from using up the attempts while every hook call (Cursor
	// sends many with nothing to deliver) would try again.
	retryBackoff = func(failures int) time.Duration {
		d := 15 * time.Second
		for i := 2; i < failures && d < 15*time.Minute; i++ {
			d *= 2
		}
		if d > 15*time.Minute {
			d = 15 * time.Minute
		}
		return d
	}
)

// bumpPending records one more failed replay of an entry.
func bumpPending(store state.Store, pe state.Pending) {
	pe.Attempts++
	pe.NextAttempt = time.Now().Add(retryBackoff(pe.Attempts))
	if err := store.SavePending(pe); err != nil {
		log.Printf("pending: update %s entry: %v", pe.Tool, err)
	}
}

// retryPending replays pending events oldest first within retryBudget,
// through the same path as a live event (parser, state lock, delivery). It
// stops at the first failed delivery. It returns a note for the log line.
func retryPending(cfg config.Config, store state.Store, now time.Time) string {
	entries, corrupt := store.ListPending()
	if len(entries) == 0 && corrupt == 0 {
		return ""
	}
	deadline := time.Now().Add(retryBudget)
	var sent, failed, dropped, waiting int
	dropped = corrupt
	for _, pe := range entries {
		if now.Sub(pe.Received) > pendingMaxAge || pe.Attempts >= pendingMaxAttempts {
			_ = store.RemovePending(pe.Tool, pe.Key)
			log.Printf("pending: dropped a %s event received %s after %d failed deliveries", pe.Tool, pe.Received.Format(time.RFC3339), pe.Attempts)
			dropped++
			continue
		}
		newParser, ok := parsers[pe.Tool]
		var pl hookPayload
		if !ok || json.Unmarshal(pe.Payload, &pl) != nil {
			_ = store.RemovePending(pe.Tool, pe.Key)
			log.Printf("pending: dropped an unusable %s entry", pe.Tool)
			dropped++
			continue
		}
		if pe.NextAttempt.After(now) {
			waiting++
			continue
		}
		left := time.Until(deadline)
		if left < retryMinSlice {
			waiting++
			continue
		}
		lockWait := retryLockWait
		if left < lockWait {
			lockWait = left
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		pe := pe
		out := handleEvent(ctx, pe.Tool, newParser(), pl, pe.Payload, cfg, eventOpts{now: pe.Received, lockTimeout: lockWait, replay: &pe})
		cancel()
		switch {
		case out.lockBusy:
			waiting++
		case out.sendErr != nil:
			failed++
			log.Printf("pending: retry of a %s event failed: %v", pe.Tool, out.sendErr)
		case out.err != nil:
			failed++
			log.Printf("pending: retry of a %s event: %v", pe.Tool, out.err)
			continue
		default:
			sent += out.turns
		}
		if out.sendErr != nil {
			break // delivery is failing again; the rest waits for the next call
		}
	}
	var parts []string
	if sent > 0 {
		parts = append(parts, fmt.Sprintf("sent %d turns", sent))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("failed %d", failed))
	}
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("dropped %d", dropped))
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("waiting %d", waiting))
	}
	if len(parts) == 0 {
		return ""
	}
	return "pending: " + strings.Join(parts, ", ")
}
