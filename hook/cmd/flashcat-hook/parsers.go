package main

import (
	"github.com/flashcatcloud/flashcat-otel/hook/internal/claude"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/codex"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/cursor"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

// parsers maps the CLI tool name to its parser constructor.
var parsers = map[string]func() model.Parser{
	"claude": claude.New,
	"codex":  codex.New,
	"cursor": cursor.New,
}
