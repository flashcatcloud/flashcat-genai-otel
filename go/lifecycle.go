package flashcatotel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v5"
)

// exportLifecycle applies the shutdown deadline to background exports as well. The OTel batch processor drains its queue
// with context.Background, so the contexts passed to ForceFlush and Shutdown are insufficient on their own.
type exportLifecycle struct {
	mu       sync.Mutex
	shutdown context.Context
	changed  chan struct{}
	active   map[*exportCall]struct{}
	bounds   map[*exportBound]struct{}
}

type exportCall struct {
	cancel context.CancelFunc
	stops  []func() bool
}

type exportBound struct{ ctx context.Context }

// errExitDeadline marks exports abandoned at the shutdown or explicit Flush deadline.
var errExitDeadline = errors.New("flashcat-otel: export abandoned at the shutdown deadline")

// bounded reports whether a shutdown or explicit Flush deadline is active.
func (l *exportLifecycle) bounded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.shutdown != nil || len(l.bounds) > 0
}

func newExportLifecycle() *exportLifecycle {
	return &exportLifecycle{changed: make(chan struct{}), active: map[*exportCall]struct{}{}, bounds: map[*exportBound]struct{}{}}
}

func (l *exportLifecycle) beginShutdown(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shutdown != nil {
		return
	}
	l.shutdown = ctx
	close(l.changed)
	l.changed = make(chan struct{})
	for call := range l.active {
		call.stops = append(call.stops, context.AfterFunc(ctx, call.cancel))
	}
}

// limit applies the explicit Flush deadline to the PeriodicReader's background exports.
// The returned function restores normal operation.
func (l *exportLifecycle) limit(ctx context.Context) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)
	bound := &exportBound{ctx: ctx}
	l.mu.Lock()
	l.bounds[bound] = struct{}{}
	close(l.changed)
	l.changed = make(chan struct{})
	for call := range l.active {
		call.stops = append(call.stops, context.AfterFunc(ctx, call.cancel))
	}
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.bounds, bound)
		l.mu.Unlock()
		cancel()
	}
}

func (l *exportLifecycle) exportContext(ctx context.Context) (context.Context, context.CancelFunc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	deadline, _ := ctx.Deadline()
	var bounds []context.Context
	if l.shutdown != nil {
		bounds = append(bounds, l.shutdown)
	}
	for bound := range l.bounds {
		bounds = append(bounds, bound.ctx)
	}
	for _, bound := range bounds {
		if d, ok := bound.Deadline(); ok && (deadline.IsZero() || d.Before(deadline)) {
			deadline = d
		}
	}
	var cancel context.CancelFunc
	if deadline.IsZero() {
		ctx, cancel = context.WithCancel(ctx)
	} else {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	}
	call := &exportCall{cancel: cancel}
	for _, bound := range bounds {
		call.stops = append(call.stops, context.AfterFunc(bound, cancel))
		if bound.Err() != nil {
			cancel()
		}
	}
	l.active[call] = struct{}{}
	return ctx, func() {
		l.mu.Lock()
		delete(l.active, call)
		for _, stop := range call.stops {
			stop()
		}
		l.mu.Unlock()
		call.cancel()
	}
}

// retryState reads the deadline and change signal together so the caller never misses a wakeup when Flush or Shutdown starts.
func (l *exportLifecycle) retryState(ctx context.Context) (bool, time.Time, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	deadline, _ := ctx.Deadline()
	limit := func(bound context.Context) {
		if d, ok := bound.Deadline(); ok && (deadline.IsZero() || d.Before(deadline)) {
			deadline = d
		}
	}
	if l.shutdown != nil {
		limit(l.shutdown)
	}
	for bound := range l.bounds {
		limit(bound.ctx)
	}
	return l.shutdown != nil || len(l.bounds) > 0, deadline, l.changed
}

// export retries up to twice during shutdown or explicit Flush, with backoffs of 0.2 and 0.4 seconds.
// The OTLP exporter's own retries are disabled. The counting layer still records only the final result for each batch.
func (l *exportLifecycle) export(ctx context.Context, timeout, initial, maxInterval time.Duration, fn func(context.Context) error) error {
	// The caller and lifecycle set the overall deadline. Each request also has its own export timeout.
	ctx, cancel := l.exportContext(ctx)
	defer cancel()
	normalDeadline := time.Now().Add(timeout)
	rec, _ := ctx.Value(callRecordKey{}).(*callRecord)
	if rec == nil {
		rec = &callRecord{}
		ctx = context.WithValue(ctx, callRecordKey{}, rec)
	}
	backoff := &backoff.ExponentialBackOff{
		InitialInterval: initial, RandomizationFactor: backoff.DefaultRandomizationFactor,
		Multiplier: backoff.DefaultMultiplier, MaxInterval: maxInterval,
	}
	backoff.Reset()
	limited, retries := false, 0
	var lastErr error
	var elapsed time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, lastErr)
		}
		bounded, deadline, _ := l.retryState(ctx)
		limited = limited || bounded
		requestDeadline := normalDeadline
		if limited {
			requestDeadline = time.Now().Add(timeout)
		}
		if !deadline.IsZero() && deadline.Before(requestDeadline) {
			requestDeadline = deadline
		}
		if !time.Now().Before(requestDeadline) {
			return errors.Join(context.DeadlineExceeded, lastErr)
		}
		if lastErr != nil && limited {
			if retries == 2 {
				return lastErr
			}
			if !deadline.IsZero() && time.Until(deadline) <= elapsed {
				return errors.Join(errExitDeadline, lastErr)
			}
			retries++
		}
		rec.status.Store(0)
		rec.retryAfter.Store(0)
		requestCtx, requestCancel := context.WithDeadline(ctx, requestDeadline)
		started := time.Now()
		err := fn(requestCtx)
		elapsed = time.Since(started)
		requestCancel()
		if err == nil || !retryableExport(err, int(rec.status.Load())) {
			return err
		}
		lastErr = err
		normalDelay := backoff.NextBackOff()
		for {
			bounded, deadline, changed := l.retryState(ctx)
			limited = limited || bounded
			delay := normalDelay
			if limited {
				if retries == 2 {
					return err
				}
				delay = (200 * time.Millisecond) << retries
			}
			delay = max(delay, time.Duration(rec.retryAfter.Load()))
			// Budget for another request using the previous request's duration. The actual request is still limited by the remaining time.
			if limited && !deadline.IsZero() && time.Until(deadline) <= delay+elapsed {
				return errors.Join(errExitDeadline, err)
			}
			if !limited {
				delay = min(delay, max(time.Until(normalDeadline), 0))
			}
			timer := time.NewTimer(delay)
			select {
			case <-changed:
				timer.Stop()
				continue
			case <-ctx.Done():
				timer.Stop()
				return errors.Join(ctx.Err(), err)
			case <-timer.C:
			}
			// Prefer the bounded retry policy when the deadline change and timer are both ready.
			select {
			case <-changed:
				continue
			default:
			}
			break
		}
	}
}

func retryableExport(err error, status int) bool {
	switch status {
	case 429, 502, 503, 504:
		return true
	case 0:
		for _, target := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE} {
			if errors.Is(err, target) {
				return true
			}
		}
		if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
			return true
		}
		// Wrapping with fmt.Errorf or errors.Join hides url.Error's Temporary method.
		switch wrapped := err.(type) {
		case interface{ Unwrap() error }:
			return retryableExport(wrapped.Unwrap(), 0)
		case interface{ Unwrap() []error }:
			for _, inner := range wrapped.Unwrap() {
				if retryableExport(inner, 0) {
					return true
				}
			}
		}
	}
	return false
}
