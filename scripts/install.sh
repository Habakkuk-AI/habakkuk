#!/bin/sh
# Install Habakkuk from its GitHub releases.
#
#   curl -fsSL https://habakkuk.ai/install.sh | sh
#   curl -fsSL https://habakkuk.ai/install.sh | sh -s -- --system
#
# The same script straight from the repository:
#   curl -fsSL https://raw.githubusercontent.com/habakkuk-ai/habakkuk/main/scripts/install.sh | sh
#
# Options:
#   --version TAG   install this release, e.g. v0.3.0 (default: the latest)
#   --dir DIR       install into DIR (default: ~/.local/bin)
#   --system        install into /usr/local/bin, with sudo if needed
#
# The same can be set with HABAKKUK_VERSION, HABAKKUK_INSTALL_DIR and
# HABAKKUK_SYSTEM=1. HABAKKUK_DOWNLOAD_URL points at a mirror holding the
# release files.
set -eu

repo="habakkuk-ai/habakkuk"
version="${HABAKKUK_VERSION:-latest}"
dir="${HABAKKUK_INSTALL_DIR:-}"
system="${HABAKKUK_SYSTEM:-0}"

say() { printf '%s\n' "$*"; }
fail() {
	printf 'habakkuk install: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Install Habakkuk from its GitHub releases.

Usage: install.sh [--version TAG] [--dir DIR] [--system]

  --version TAG   install this release, e.g. v0.3.0 (default: the latest)
  --dir DIR       install into DIR (default: ~/.local/bin)
  --system        install into /usr/local/bin, with sudo if needed
EOF
	exit 0
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		[ $# -ge 2 ] || fail "--version needs a tag"
		version="$2"
		shift 2
		;;
	--dir)
		[ $# -ge 2 ] || fail "--dir needs a directory"
		dir="$2"
		shift 2
		;;
	--system)
		system=1
		shift
		;;
	-h | --help) usage ;;
	*) fail "unknown option: $1 (try --help)" ;;
	esac
done

if [ -z "$dir" ]; then
	if [ "$system" = 1 ]; then
		dir=/usr/local/bin
	else
		[ -n "${HOME:-}" ] || fail "HOME is not set; pass --dir"
		dir="$HOME/.local/bin"
	fi
fi

os=$(uname -s)
arch=$(uname -m)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
MINGW* | MSYS* | CYGWIN*) fail "on Windows, install from PowerShell instead: irm https://habakkuk.ai/install.ps1 | iex" ;;
*) fail "no release for $os; build from source with: go install github.com/$repo/cmd/habakkuk@latest" ;;
esac
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "no release for $arch; build from source with: go install github.com/$repo/cmd/habakkuk@latest" ;;
esac
# A shell running under Rosetta reports x86_64 on Apple silicon.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
	arch=arm64
fi

if [ -n "${HABAKKUK_DOWNLOAD_URL:-}" ]; then
	base="$HABAKKUK_DOWNLOAD_URL"
elif [ "$version" = latest ]; then
	base="https://github.com/$repo/releases/latest/download"
else
	base="https://github.com/$repo/releases/download/$version"
fi
archive="habakkuk_${os}_${arch}.tar.gz"

download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		fail "curl or wget is required"
	fi
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$1" | awk '{print $NF}'
	else
		fail "sha256sum, shasum or openssl is required to verify the download"
	fi
}

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t habakkuk)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM

say "Downloading Habakkuk ($version, $os/$arch)..."
download "$base/$archive" "$tmp/$archive" || fail "could not download $base/$archive"
download "$base/SHA256SUMS" "$tmp/SHA256SUMS" || fail "could not download $base/SHA256SUMS"

want=$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1}' "$tmp/SHA256SUMS")
[ -n "$want" ] || fail "SHA256SUMS has no entry for $archive"
got=$(sha256 "$tmp/$archive")
[ "$want" = "$got" ] || fail "checksum mismatch for $archive (want $want, got $got); not installing"
say "Checksum verified."

mkdir "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x"
[ -f "$tmp/x/habakkuk" ] || fail "$archive does not contain the habakkuk binary"
chmod 0755 "$tmp/x/habakkuk"

use_sudo=0
as_owner() {
	if [ "$use_sudo" = 1 ]; then sudo "$@"; else "$@"; fi
}
if ! mkdir -p "$dir" 2>/dev/null || [ ! -w "$dir" ]; then
	if [ "$system" = 1 ] && [ "$(id -u)" != 0 ] && command -v sudo >/dev/null 2>&1; then
		use_sudo=1
		say "Installing to $dir needs sudo."
		as_owner mkdir -p "$dir"
	else
		fail "cannot write to $dir; pass --dir, or --system to use sudo"
	fi
fi
# Copy beside the target, then rename, so a running habakkuk is never half
# overwritten.
as_owner cp "$tmp/x/habakkuk" "$dir/.habakkuk.new"
as_owner mv -f "$dir/.habakkuk.new" "$dir/habakkuk"

say "Installed $("$dir/habakkuk" version 2>/dev/null || echo habakkuk) to $dir/habakkuk"
case ":${PATH:-}:" in
*":$dir:"*) ;;
*)
	say ""
	say "$dir is not on your PATH. Add it, for example:"
	say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile && . ~/.profile"
	;;
esac
cat <<'EOF'

Next steps:
  habakkuk init                                        # writes ~/.habakkuk/config.yaml
  echo 'ANTHROPIC_API_KEY=...' >> ~/.habakkuk/.env      # or another provider's key
  habakkuk doctor                                      # checks config, keys and channels
  habakkuk chat
EOF
