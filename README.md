# flashcat-otel

This monorepo contains the Flashcat OTel SDK for AI agent observability. Each of the four languages has its own directory and follows the same contract.

| Directory | Contents |
|---|---|
| `spec/` | Cross-language contract (`contract.md`) and conformance test cases (`fixtures/`) |
| `python/` | `flashcat-otel` (PyPI), DashScope and MCP instrumentation, and a demo traffic tool |
| `js/` | `@flashcat/otel` (npm), Node.js ≥ 22.15 |
| `go/` | `github.com/flashcatcloud/flashcat-otel/go`, with three submodules, `instrumentation/openai`, `instrumentation/eino`, and `instrumentation/mcp` |
| `java/` | `cloud.flashcat:flashcat-otel` and the OTel Java Agent extension JAR |
| `hook/` | `flashcat-hook` uses hooks in Claude Code, Codex, and Cursor to send coding tool sessions to Flashcat. It is released separately under the tag `hook-vX.Y.Z` |

See each language directory's README for usage. See [RELEASING.md](RELEASING.md) for Python publishing, GitHub Releases installation, and Go module tags.

```bash
cd python && uv sync && uv run pytest
cd js && npm install && npm run test:setup && npm test
cd go && go test -race ./...          # Run the tests separately in instrumentation/openai, instrumentation/eino, and instrumentation/mcp
cd java && ./gradlew build
cd hook && go test -race ./...
```

## License

[Apache License 2.0](LICENSE). `go/instrumentation/openai` and `python/instrumentation/flashcat-instrumentation-dashscope` are adapted from upstream Apache-2.0 code. See `NOTICE` and `UPSTREAM.md` in each directory.
