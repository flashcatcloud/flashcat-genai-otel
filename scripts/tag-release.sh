#!/usr/bin/env bash
# Tag the current commit and push v<version> to trigger the release workflow, along with tags for all four Go modules.
# Prefix Go module tags with their directory. go get github.com/flashcatcloud/flashcat-otel/go@v0.1.0 resolves to go/v0.1.0.
# Usage: scripts/tag-release.sh <version, e.g. 0.1.0>
set -euo pipefail
VERSION=${1:?用法：scripts/tag-release.sh <版本号>}
[[ -z "$(git status --porcelain)" ]] || { echo "The working tree has uncommitted changes" >&2; exit 1; }
GO_TAGS=("go/v$VERSION" "go/instrumentation/openai/v$VERSION" "go/instrumentation/eino/v$VERSION" "go/instrumentation/mcp/v$VERSION")
for t in "${GO_TAGS[@]}" "v$VERSION"; do git tag -a "$t" -m "flashcat-otel $VERSION"; done
# Push v<version> separately so the release workflow runs. GitHub skips push events when more than three tags are pushed at once.
git push origin "${GO_TAGS[@]}"
git push origin "v$VERSION"
