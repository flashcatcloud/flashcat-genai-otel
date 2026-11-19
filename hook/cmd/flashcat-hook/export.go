package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/claude"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/config"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/otlp"
)

// cmdExport is a hidden developer command: parse a whole transcript and
// write the OTLP JSON that `hook` would send, without sending it.
//
//	flashcat-hook export claude --transcript <path> [--content none|no_tool_content|full] --out <file.json>
func cmdExport(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: flashcat-hook export <tool> --transcript <path> [--content ...] [--app ...] --out <file.json>")
		return 2
	}
	tool := args[0]
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	transcript := fs.String("transcript", "", "transcript path")
	content := fs.String("content", "none", "content level")
	app := fs.String("app", "coding-p0", "service.name")
	user := fs.String("user", "", "user id override")
	session := fs.String("session", "", "session id (default: transcript file name)")
	out := fs.String("out", "", "output file (- for stdout)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	newParser, ok := parsers[tool]
	if !ok || *transcript == "" || *out == "" {
		fmt.Fprintln(stderr, "export: need a known tool, --transcript and --out")
		return 2
	}
	if !config.ValidContent(*content) {
		fmt.Fprintln(stderr, "export: invalid --content")
		return 2
	}
	sid := *session
	if sid == "" {
		sid = strings.TrimSuffix(filepath.Base(*transcript), filepath.Ext(*transcript))
	}
	parser := newParser()
	if tool == "claude" {
		parser = claude.NewBackfill() // whole transcript, not just the latest turn
	}
	turns, err := collectAll(parser, model.HookEvent{
		Tool:           toolOf(tool),
		Name:           finalEventName(tool),
		SessionID:      sid,
		TranscriptPath: *transcript,
		Final:          true,
		Now:            time.Now(),
	})
	if err != nil {
		fmt.Fprintln(stderr, "export:", err)
		return 1
	}
	id := &config.Identity{ConfigUser: *user}
	resolveTurns(turns, id)
	td := otlp.Build(turns, resourceInfo(*app), otlp.Options{Content: model.Content(*content)})
	b, err := (&ptrace.JSONMarshaler{}).MarshalTraces(td)
	if err != nil {
		fmt.Fprintln(stderr, "export:", err)
		return 1
	}
	if *out == "-" {
		_, err = stdout.Write(b)
	} else {
		err = os.WriteFile(*out, b, 0o600)
	}
	if err != nil {
		fmt.Fprintln(stderr, "export:", err)
		return 1
	}
	fmt.Fprintf(stderr, "export: %d turns, %d spans\n", len(turns), td.SpanCount())
	return 0
}

// collectAll calls Collect until it stops producing turns (the parser caps
// the turns of one call).
func collectAll(p model.Parser, ev model.HookEvent) ([]model.Turn, error) {
	var all []model.Turn
	var st []byte
	for i := 0; i < 10000; i++ {
		res, err := p.Collect(ev, st)
		if err != nil {
			return all, err
		}
		all = append(all, res.Turns...)
		if len(res.Turns) == 0 {
			return all, nil
		}
		st = res.StateOnSuccess
	}
	return all, errors.New("too many iterations")
}

func finalEventName(tool string) string {
	switch tool {
	case "cursor":
		return "sessionEnd"
	case "codex":
		return "Stop"
	}
	return "SessionEnd"
}
