// Package flashcatgenai is the Go SDK for Flashcat AI agent observability. It provides one-line initialization,
// export to Flashcat, semantic APIs for agents, tools, workflows, and sessions, plus masking, truncation, and diagnostics.
//
//	sdk, err := flashcatgenai.Init(ctx, flashcatgenai.WithServiceName("kefu-agent"))
//	if err != nil { /* Only invalid explicit options return an error. */ }
//	defer sdk.Shutdown(context.Background())
//
// Model call spans come from instrumentation submodules (go/instrumentation/openai and go/instrumentation/eino)
// or built-in model SDK instrumentation (Anthropic Go SDK). This package connects them to the same export pipeline.
//
// See spec/contract.md for the cross-language contract.
package flashcatgenai
