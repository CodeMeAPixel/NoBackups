#!/bin/sh
set -eu

REPO=CodeMeAPixel/NoBackups
VERSION=${NOBACKUPS_VERSION:-latest}
SOURCE=${NOBACKUPS_SOURCE:-auto}

say() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root, e.g. sudo sh $0"
[ "$(uname -s)" = Linux ] || die "NoBackups runs on Linux servers"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

find_go() {
	for g in "$(command -v go 2>/dev/null || true)" /usr/local/go/bin/go /usr/lib/go/bin/go /snap/bin/go; do
		[ -n "$g" ] && [ -x "$g" ] && { echo "$g"; return 0; }
	done
	if [ -n "${SUDO_USER:-}" ]; then
		h=$(getent passwd "$SUDO_USER" | cut -d: -f6)
		for g in "$h/go/bin/go" "$h/.local/go/bin/go" "$h/sdk/go/bin/go"; do
			[ -x "$g" ] && { echo "$g"; return 0; }
		done
	fi
	return 1
}

go_ok() {
	v=$("$1" env GOVERSION 2>/dev/null | sed 's/^go//')
	major=${v%%.*}; rest=${v#*.}; minor=${rest%%.*}
	[ "${major:-0}" -gt 1 ] || { [ "${major:-0}" -eq 1 ] && [ "${minor:-0}" -ge 24 ]; }
}

build_from_source() {
	src=$1 gobin=$2
	ver=$(git -c safe.directory='*' -C "$src" describe --tags --always --dirty 2>/dev/null || echo dev)
	say "building $ver from source with $("$gobin" version | cut -d' ' -f3)"
	(cd "$src" && CGO_ENABLED=0 GOFLAGS=-buildvcs=false "$gobin" build -trimpath \
		-ldflags "-s -w -X main.version=$ver" -o "$TMP/nobackups" ./cmd/nobackups)
}

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "need curl or wget to download NoBackups"
	fi
}

download_release() {
	case $(uname -m) in
		x86_64 | amd64) arch=amd64 ;;
		aarch64 | arm64) arch=arm64 ;;
		armv7* | armv8l) arch=armv7 ;;
		*) die "no release binary for $(uname -m); install Go 1.24+ and run this from a clone to build from source" ;;
	esac
	if [ -n "${NOBACKUPS_DOWNLOAD_URL:-}" ]; then
		base=${NOBACKUPS_DOWNLOAD_URL%/}
	elif [ "$VERSION" = latest ]; then
		base=https://github.com/$REPO/releases/latest/download
	else
		base=https://github.com/$REPO/releases/download/$VERSION
	fi
	file=nobackups-linux-$arch
	say "downloading $file ($VERSION)"
	fetch "$base/$file" "$TMP/$file" || die "download failed: $base/$file (has a release been published?)"
	fetch "$base/SHA256SUMS" "$TMP/SHA256SUMS" || die "download failed: $base/SHA256SUMS"
	(cd "$TMP" && grep " $file\$" SHA256SUMS | sha256sum -c - >/dev/null) || die "checksum mismatch for $file"
	say "checksum verified"
	mv "$TMP/$file" "$TMP/nobackups"
}

if [ $# -gt 0 ]; then
	[ -f "$1" ] || die "binary not found: $1"
	cp "$1" "$TMP/nobackups"
else
	src=""
	case $0 in
		*install.sh) d=$(cd "$(dirname "$0")" && pwd); [ -f "$d/go.mod" ] && [ -d "$d/cmd/nobackups" ] && src=$d ;;
	esac
	gobin=""
	if [ -n "$src" ] && [ "$SOURCE" != release ]; then
		if gobin=$(find_go) && go_ok "$gobin"; then
			build_from_source "$src" "$gobin" || {
				[ "$SOURCE" = build ] && die "build failed"
				say "build failed, falling back to the release binary"
				download_release
			}
		elif [ "$SOURCE" = build ]; then
			die "Go 1.24+ not found; install it from https://go.dev/dl/ or drop NOBACKUPS_SOURCE=build"
		else
			say "Go 1.24+ not found, using the release binary instead"
			download_release
		fi
	elif [ "$SOURCE" = build ]; then
		die "building from source needs a clone of the repository"
	else
		download_release
	fi
fi

chmod 0755 "$TMP/nobackups"
"$TMP/nobackups" install
