#!/usr/bin/env sh
# Installs the latest amuxify release binary for this OS and CPU.
# Verifies the SHA-256 checksum published with the release before installing.
#   curl -fsSL https://raw.githubusercontent.com/amuxify/amuxify/main/install.sh | sh
#   PREFIX=$HOME/.local sh install.sh
#   VERSION=0.2.0 sh install.sh
set -eu
REPO="amuxify/amuxify"
PREFIX="${PREFIX:-/usr/local}"
BIN="$PREFIX/bin"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  armv7l|armv7) arch=armv7 ;;
  *) echo "install.sh: unsupported CPU $arch" >&2; exit 1 ;;
esac
case "$os" in linux|darwin) ;; *) echo "install.sh: unsupported OS $os (Windows: download the zip from the releases page)" >&2; exit 1 ;; esac
if [ -z "${VERSION:-}" ]; then
  VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$VERSION" ] || { echo "install.sh: could not determine latest version" >&2; exit 1; }
fi
name="amuxify_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/v$VERSION"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM
echo "Downloading amuxify $VERSION for $os/$arch"
curl -fsSL -o "$tmp/$name" "$base/$name"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
want="$(grep " $name\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
[ -n "$want" ] || { echo "install.sh: no checksum for $name" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$tmp/$name" | cut -d' ' -f1)"; else got="$(shasum -a 256 "$tmp/$name" | cut -d' ' -f1)"; fi
[ "$want" = "$got" ] || { echo "install.sh: checksum mismatch for $name" >&2; exit 1; }
tar -xzf "$tmp/$name" -C "$tmp"
mkdir -p "$BIN"
install -m 755 "$tmp/amuxify" "$BIN/amuxify"
echo "Installed $BIN/amuxify"
"$BIN/amuxify" version
echo "Next: $BIN/amuxify doctor"
