#!/bin/sh
# amp-bridge installer.
#
#   curl -fsSL https://raw.githubusercontent.com/oliver-kriska/amp-claude/main/install.sh | sh
#
# Downloads the latest release for this platform, verifies its SHA-256 against
# the published checksums, and installs to ~/.local/bin. Override with
# AMP_BRIDGE_PREFIX=/usr/local, or AMP_BRIDGE_VERSION=v0.2.0 to pin a release.
set -eu

REPO="oliver-kriska/amp-claude"
PREFIX="${AMP_BRIDGE_PREFIX:-$HOME/.local}"
BINDIR="$PREFIX/bin"

die() { printf 'amp-bridge: %s\n' "$1" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac
case "$os" in
  darwin|linux) ;;
  *) die "unsupported OS: $os (build from source: https://github.com/$REPO)" ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO- "$1"; }
else
  die "neither curl nor wget is available"
fi

version="${AMP_BRIDGE_VERSION:-}"
if [ -z "$version" ]; then
  version=$(fetch "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$version" ] || die "could not determine the latest release"
fi

tarball="amp-bridge_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

printf 'downloading %s %s (%s/%s)\n' amp-bridge "$version" "$os" "$arch"
fetch "$base/$tarball" > "$tmp/$tarball" || die "download failed"
fetch "$base/checksums.txt" > "$tmp/checksums.txt" || die "checksums unavailable"

# Verify before unpacking, not after. A corrupted or substituted archive should
# never reach the point of writing an executable onto the PATH.
want=$(sed -n "s/^\([0-9a-f]*\)  *$tarball\$/\1/p" "$tmp/checksums.txt")
[ -n "$want" ] || die "no checksum published for $tarball"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$tarball" | cut -d' ' -f1)
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "$tmp/$tarball" | cut -d' ' -f1)
else
  die "no sha256sum or shasum available to verify the download"
fi
[ "$got" = "$want" ] || die "checksum mismatch: expected $want, got $got"

tar -xzf "$tmp/$tarball" -C "$tmp"
mkdir -p "$BINDIR"
# Unlink first: overwriting a Mach-O in place invalidates its macOS signature,
# and the kernel then kills it at every later exec.
rm -f "$BINDIR/amp-bridge"
install -m 0755 "$tmp/amp-bridge" "$BINDIR/amp-bridge"

printf 'installed %s\n\n' "$BINDIR/amp-bridge"
case ":$PATH:" in
  *":$BINDIR:"*) ;;
  *) printf 'note: %s is not on your PATH — add it to your shell profile\n\n' "$BINDIR" ;;
esac

printf 'Next:\n'
printf '  amp-bridge init --global    # register for every project, plus the skill\n'
printf '  claude --dangerously-load-development-channels server:amp-bridge\n'
printf '  amp-bridge doctor\n'
