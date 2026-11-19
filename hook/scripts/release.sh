#!/bin/sh
# Build a flashcat-hook release into hook/dist/hook-v<version>/ (CONTRACT §6):
# six binaries, install.sh, install.ps1 and SHA256SUMS.
#
#   scripts/release.sh 0.1.0
#
# FLASHCAT_HOOK_BASE_URL overrides the default download base written into the
# installers (a base WITHOUT the version; the installers append /hook-v<version>).
set -eu

VERSION=${1:-}
if [ -z "$VERSION" ]; then
	echo "usage: $0 <version>   e.g. $0 0.1.0" >&2
	exit 2
fi
VERSION=${VERSION#hook-v}
VERSION=${VERSION#v}
case $VERSION in
*[!0-9A-Za-z.+-]*)
	echo "invalid version: $VERSION" >&2
	exit 2
	;;
esac

BASE_URL=${FLASHCAT_HOOK_BASE_URL:-https://github.com/flashcatcloud/flashcat-otel/releases/download}
BASE_URL=${BASE_URL%/}

cd "$(dirname "$0")/.."
OUT=dist/hook-v$VERSION
rm -rf "$OUT"
mkdir -p "$OUT"

for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
	os=${target%/*}
	arch=${target#*/}
	ext=
	[ "$os" = windows ] && ext=.exe
	name=flashcat-hook_${os}_${arch}${ext}
	echo "build $name"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$VERSION" \
		-o "$OUT/$name" ./cmd/flashcat-hook
done

for f in install.sh install.ps1; do
	sed -e "s|@VERSION@|$VERSION|g" -e "s|@BASE_URL@|$BASE_URL|g" "scripts/$f.tmpl" >"$OUT/$f"
done
chmod +x "$OUT/install.sh"

if command -v sha256sum >/dev/null 2>&1; then
	sum() { sha256sum "$@"; }
else
	sum() { shasum -a 256 "$@"; }
fi
(cd "$OUT" && sum flashcat-hook_* install.sh install.ps1 >SHA256SUMS)

echo "release written to hook/$OUT"
ls -l "$OUT"
