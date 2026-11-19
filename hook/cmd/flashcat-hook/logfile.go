package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/config"
)

// maxLogBytes is when hook.log rotates to hook.log.1 (CONTRACT §2).
const maxLogBytes = 5 << 20

func logPath() string { return filepath.Join(config.StateDir(), "hook.log") }

// openLog opens hook.log for appending, rotating it first when it is over
// 5 MB. It never fails: on error it returns io.Discard.
func openLog() io.WriteCloser {
	p := logPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nopCloser{io.Discard}
	}
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxLogBytes {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nopCloser{io.Discard}
	}
	return f
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// hookLog collects the details of one hook call and writes a summary line.
// Parsers report details (unknown record types…) through the standard log
// package, which is pointed at the same file.
type hookLog struct {
	w     io.WriteCloser
	start time.Time
}

func newHookLog() *hookLog {
	w := openLog()
	log.SetOutput(w)
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("  ")
	return &hookLog{w: w, start: time.Now()}
}

// summary writes one line: tool, event, turns, duration, error.
func (l *hookLog) summary(tool, event string, turns int, note string, err error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s tool=%s event=%s turns=%d dur=%s",
		time.Now().Format(time.RFC3339), orDash(tool), orDash(event), turns, time.Since(l.start).Round(time.Millisecond))
	if note != "" {
		fmt.Fprintf(&b, " note=%q", note)
	}
	if err != nil {
		fmt.Fprintf(&b, " error=%q", err.Error())
	}
	b.WriteByte('\n')
	_, _ = io.WriteString(l.w, b.String())
	log.SetOutput(io.Discard)
	_ = l.w.Close()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
