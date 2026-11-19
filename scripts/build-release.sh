#!/usr/bin/env bash
# Build all GitHub Release artifacts locally or from .github/workflows/release.yml.
# Usage: scripts/build-release.sh <version, e.g. 0.1.0> [output directory, defaults to dist/]
# Outputs include Python wheels and sdists, an npm tarball, Java JARs and POMs, a Maven repository ZIP, and SHA256SUMS.
# Package versions must match the requested version. Git tags determine the Go module versions.
set -euo pipefail

VERSION=${1:?用法：scripts/build-release.sh <版本号> [输出目录]}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$(mkdir -p "${2:-$ROOT/dist}" && cd "${2:-$ROOT/dist}" && pwd)

fail() { echo "build-release: $*" >&2; exit 1; }

# Verify package versions.
py_ver() { sed -n 's/^version = "\(.*\)"/\1/p' "$1" | head -1; }
for f in python/packages/flashcat-otel/pyproject.toml python/instrumentation/flashcat-instrumentation-dashscope/pyproject.toml python/instrumentation/flashcat-instrumentation-mcp/pyproject.toml; do
  [[ "$(py_ver "$ROOT/$f")" == "$VERSION" ]] || fail "Expected $f version $VERSION, found $(py_ver "$ROOT/$f")"
done
js_ver=$(node -p "require('$ROOT/js/package.json').version")
[[ "$js_ver" == "$VERSION" ]] || fail "Expected js/package.json version $VERSION, found $js_ver"
java_ver=$(sed -n 's/^ *version = "\(.*\)"/\1/p' "$ROOT/java/build.gradle.kts" | head -1)
[[ "$java_ver" == "$VERSION" ]] || fail "Expected java/build.gradle.kts version $VERSION, found $java_ver"

# Build a wheel and an sdist for each of the three Python packages.
(cd "$ROOT/python" && uv build --package flashcat-otel --out-dir "$OUT" && uv build --package flashcat-instrumentation-dashscope --out-dir "$OUT" \
  && uv build --package flashcat-instrumentation-mcp --out-dir "$OUT")

# Use npm pack to create flashcat-otel-<version>.tgz.
(cd "$ROOT/js" && npm ci --no-audit --no-fund && npm run build && npm pack --pack-destination "$OUT")

# Package both Java JARs and their POMs. Zip the local Maven repository for file-based use or import into Nexus or Artifactory.
M2=$(mktemp -d)
(cd "$ROOT/java" && ./gradlew --no-daemon -q assemble publishToMavenLocal -Dmaven.repo.local="$M2")
for a in flashcat-otel flashcat-otel-agent-extension; do
  d="$M2/cloud/flashcat/$a/$VERSION"
  [[ -f "$d/$a-$VERSION.jar" ]] || fail "Build did not produce $a-$VERSION.jar"
  cp "$d/$a-$VERSION.jar" "$d/$a-$VERSION.pom" "$OUT/"
done
(cd "$M2" && zip -qr "$OUT/flashcat-otel-java-maven-repo-$VERSION.zip" cloud/flashcat)
rm -rf "$M2"

(cd "$OUT" && rm -f SHA256SUMS && shasum -a 256 -- * > SHA256SUMS && cat SHA256SUMS)
