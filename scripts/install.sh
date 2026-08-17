#!/bin/sh
# Install Sennit from its GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/rave-soft/sennit/main/scripts/install.sh | sh
#
# Environment:
#   SENNIT_VERSION  version to install (default: the latest release)
#   SENNIT_BIN_DIR  where to put the binary (default: ~/.local/bin)
#
# POSIX sh on purpose: this runs on whatever shell a machine has before
# Sennit is on it.
set -eu

REPO="rave-soft/sennit"
BIN_DIR="${SENNIT_BIN_DIR:-$HOME/.local/bin}"

die() {
	echo "install: $*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

# download URL FILE — curl or wget, whichever the machine has.
download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		die "curl or wget is required"
	fi
}

need tar
need uname

os="$(uname -s)"
case "$os" in
Linux) os="Linux" ;;
Darwin) os="Darwin" ;;
*) die "unsupported OS: $os (Windows: download the .zip from https://github.com/$REPO/releases)" ;;
esac

arch="$(uname -m)"
case "$arch" in
x86_64 | amd64) arch="x86_64" ;;
arm64 | aarch64) arch="arm64" ;;
*) die "unsupported architecture: $arch" ;;
esac

version="${SENNIT_VERSION:-}"
if [ -z "$version" ]; then
	# The redirect on /releases/latest names the tag, so the version can be
	# resolved without a GitHub API call (which is rate-limited per IP and
	# would fail for anyone behind a busy NAT).
	if command -v curl >/dev/null 2>&1; then
		version="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's#.*/tag/##')"
	else
		version="$(wget -qS --max-redirect=0 "https://github.com/$REPO/releases/latest" 2>&1 | sed -n 's#.*Location:.*/tag/##p' | tr -d '\r')"
	fi
	[ -n "$version" ] || die "could not determine the latest version; set SENNIT_VERSION"
fi
# Tags carry a leading v; archive names do not.
bare="${version#v}"

archive="sennit_${bare}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "Downloading sennit $version ($os/$arch)..."
download "$base/$archive" "$tmp/$archive" || die "no release asset named $archive"

# Verify against the release's checksums file. A failed download that still
# unpacks is worse than no install at all.
if download "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
	expected="$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')"
	if [ -n "$expected" ]; then
		if command -v sha256sum >/dev/null 2>&1; then
			actual="$(sha256sum "$tmp/$archive" | awk '{print $1}')"
		elif command -v shasum >/dev/null 2>&1; then
			actual="$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')"
		else
			actual=""
			echo "warning: no sha256 tool found; skipping checksum verification" >&2
		fi
		[ -z "$actual" ] || [ "$actual" = "$expected" ] || die "checksum mismatch for $archive"
	fi
else
	echo "warning: checksums.txt could not be fetched; skipping verification" >&2
fi

tar -xzf "$tmp/$archive" -C "$tmp"
binary="$(find "$tmp" -type f -name sennit -perm -u+x | head -n 1)"
[ -n "$binary" ] || die "the archive contains no sennit binary"

mkdir -p "$BIN_DIR"
install -m 0755 "$binary" "$BIN_DIR/sennit" 2>/dev/null ||
	{ cp "$binary" "$BIN_DIR/sennit" && chmod 0755 "$BIN_DIR/sennit"; }

echo "Installed $BIN_DIR/sennit"

if [ "$os" = "Darwin" ]; then
	# Unsigned, un-notarized binaries are quarantined by Gatekeeper, and
	# the first run fails with "cannot be opened because the developer
	# cannot be verified". Clearing the attribute here is the same thing
	# the user would be told to do, done once at install time.
	xattr -d com.apple.quarantine "$BIN_DIR/sennit" 2>/dev/null || true
fi

case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*) echo "Note: $BIN_DIR is not on your PATH. Add it to your shell profile." ;;
esac

"$BIN_DIR/sennit" --version 2>/dev/null || true
