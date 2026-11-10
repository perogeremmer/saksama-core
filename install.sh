#!/bin/sh
# Install Saksama without a Go toolchain.
#
#   curl -fsSL https://raw.githubusercontent.com/perogeremmer/saksama-core/master/install.sh | sh
#
# What it does, in order: work out the platform, download the archive for it,
# verify the checksum against the release's checksums.txt, and put the binary
# where a shell will find it. It refuses to install anything it cannot verify,
# because a migration tool that installs itself from an unverified download is
# not a safety tool.
#
# Environment:
#   SAKSAMA_VERSION       tag to install, e.g. 0.4.0 (default: the latest release)
#   SAKSAMA_INSTALL_DIR   where to put the binary (default: /usr/local/bin when
#                         writable, else $HOME/.local/bin)
#   SAKSAMA_BASE_URL      override where the archive and checksums come from, for an
#                         internal mirror or an offline test (curl also accepts
#                         file://, which is how CI exercises the failure paths)
set -eu

REPO=perogeremmer/saksama-core

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

need uname
need tar
if command -v curl >/dev/null 2>&1; then
	# stderr stays out of the way: a 404 from a missing archive is reported by the
	# message below, not by curl's own one-liner above it.
	fetch() { curl -fsSL "$1" 2>/dev/null; }
	fetch_to() { curl -fsSL -o "$2" "$1" 2>/dev/null; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO- "$1"; }
	fetch_to() { wget -qO "$2" "$1"; }
else
	die "curl or wget is required"
fi

# --- platform -----------------------------------------------------------------

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
linux) ;;
darwin)
	die "macOS is not a supported platform. Saksama runs the copy on the machine next to the database, and outside Linux the engine's identity cannot be confirmed through /proc - cancel and retire would have to trust a pid that may have been recycled. Run it on the database host, or build from source if you accept that."
	;;
*) die "unsupported operating system: $os (Saksama runs on Linux)" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture: $arch (releases exist for amd64 and arm64)" ;;
esac

# --- version ------------------------------------------------------------------

version=${SAKSAMA_VERSION:-}
if [ -z "$version" ]; then
	# The releases API, unauthenticated: it is one request, and the tag is the
	# only field needed. SAKSAMA_VERSION exists for the case where that request
	# fails (rate limit, proxy, air-gapped mirror).
	version=$(fetch "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -1)
	[ -n "$version" ] || die "cannot work out the latest version; set SAKSAMA_VERSION"
fi
version=${version#v}

base=${SAKSAMA_BASE_URL:-https://github.com/$REPO/releases/download/v$version}
archive="saksama_${version}_${os}_${arch}.tar.gz"

# --- install ------------------------------------------------------------------

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "saksama $version ($os/$arch)"
fetch_to "$base/$archive" "$tmp/$archive" ||
	die "no release archive at $base/$archive (is that version right for this platform?)"
fetch_to "$base/checksums.txt" "$tmp/checksums.txt" ||
	die "the release published no checksums.txt; refusing to install an unverified binary"

# Verify before unpacking: an archive nobody checked is an archive whose contents
# nobody chose.
if command -v sha256sum >/dev/null 2>&1; then
	sum() { sha256sum "$@"; }
elif command -v shasum >/dev/null 2>&1; then
	sum() { shasum -a 256 "$@"; }
else
	die "sha256sum or shasum is required to verify the download"
fi

want=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
got=$(cd "$tmp" && sum "$archive" | awk '{print $1}')
[ "$want" = "$got" ] || die "checksum mismatch for $archive
  expected $want
  got      $got"
say "checksum ok"

tar xzf "$tmp/$archive" -C "$tmp"
[ -f "$tmp/saksama" ] || die "the archive did not contain a saksama binary"

dir=${SAKSAMA_INSTALL_DIR:-}
if [ -z "$dir" ]; then
	if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		dir=/usr/local/bin
	else
		dir=$HOME/.local/bin
	fi
fi
mkdir -p "$dir"

if command -v install >/dev/null 2>&1; then
	install -m 0755 "$tmp/saksama" "$dir/saksama"
else
	cp "$tmp/saksama" "$dir/saksama"
	chmod 0755 "$dir/saksama"
fi

say "installed $dir/saksama"
"$dir/saksama" version

case ":$PATH:" in
*":$dir:"*) ;;
*)
	say ""
	say "note: $dir is not on PATH. Add it with:"
	say "  export PATH=\"$dir:\$PATH\""
	;;
esac
