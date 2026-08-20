#!/bin/sh
# clav installer.
#
#   curl -fsSL https://raw.githubusercontent.com/mattkje/clav/main/install.sh | sh
#
# Downloads the release binary for this machine from GitHub, checks it against
# the release's checksums, and installs it. Nothing else is touched.
#
# Environment:
#   CLAV_VERSION      version to install (default: the latest release)
#   CLAV_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   CLAV_BASE_URL     where to download from (default: the GitHub release)

set -eu

REPO="mattkje/clav"
BINARY="clav"
INSTALL_DIR="${CLAV_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf 'clav install: %s\n' "$*" >&2; exit 1; }

# --- what are we running on? -------------------------------------------------

detect_platform() {
	os=$(uname -s)
	arch=$(uname -m)
	case "$os" in
		Darwin) os=darwin ;;
		Linux) os=linux ;;
		*) die "unsupported operating system: $os (clav ships macOS and Linux builds)" ;;
	esac
	case "$arch" in
		x86_64 | amd64) arch=amd64 ;;
		arm64 | aarch64) arch=arm64 ;;
		*) die "unsupported architecture: $arch" ;;
	esac
	PLATFORM="${os}-${arch}"
}

# --- fetching ----------------------------------------------------------------

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
	fetch_stdout() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
	fetch_stdout() { wget -qO- "$1"; }
else
	die "neither curl nor wget is installed"
fi

latest_version() {
	# The redirect target of /releases/latest ends in the tag, which avoids
	# depending on the API's rate limit or on a JSON parser being present.
	fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p' | head -n 1
}

checksum_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		printf ''
	fi
}

# --- install -----------------------------------------------------------------

main() {
	detect_platform

	version="${CLAV_VERSION:-}"
	if [ -z "$version" ]; then
		version=$(latest_version) || true
		[ -n "$version" ] || die "cannot work out the latest version; set CLAV_VERSION=v1.2.3"
	fi

	asset="${BINARY}-${PLATFORM}"
	base="${CLAV_BASE_URL:-https://github.com/$REPO/releases/download/$version}"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	say "clav $version for $PLATFORM"
	fetch "$base/$asset" "$tmp/$BINARY" || die "cannot download $base/$asset"

	# Verify against the checksums published with the release. A release
	# without them is a reason to stop, not a reason to shrug.
	if fetch "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
		want=$(sed -n "s/^\([0-9a-f]*\)  *$asset\$/\1/p" "$tmp/checksums.txt" | head -n 1)
		got=$(checksum_of "$tmp/$BINARY")
		if [ -z "$got" ]; then
			say "  (no sha256 tool found; skipping checksum verification)"
		elif [ -z "$want" ]; then
			die "checksums.txt does not list $asset"
		elif [ "$want" != "$got" ]; then
			die "checksum mismatch for $asset: expected $want, got $got"
		else
			say "  checksum ok"
		fi
	else
		die "cannot download checksums for $version"
	fi

	chmod +x "$tmp/$BINARY"

	mkdir -p "$INSTALL_DIR" 2>/dev/null || true
	if [ -w "$INSTALL_DIR" ]; then
		mv "$tmp/$BINARY" "$INSTALL_DIR/$BINARY"
	elif command -v sudo >/dev/null 2>&1; then
		say "  $INSTALL_DIR needs root; using sudo"
		sudo mkdir -p "$INSTALL_DIR"
		sudo mv "$tmp/$BINARY" "$INSTALL_DIR/$BINARY"
	else
		die "cannot write to $INSTALL_DIR; set CLAV_INSTALL_DIR to somewhere you own"
	fi

	say "  installed $INSTALL_DIR/$BINARY"

	# clav shells out to git for everything it does.
	command -v git >/dev/null 2>&1 || say "  note: clav needs git, and git was not found on your PATH"

	case ":$PATH:" in
		*":$INSTALL_DIR:"*) ;;
		*)
			say ""
			say "$INSTALL_DIR is not on your PATH. Add it:"
			say "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.zshrc && exec zsh"
			;;
	esac

	say ""
	"$INSTALL_DIR/$BINARY" --version 2>/dev/null || true
	say "Run 'clav --help' to get started."
}

main "$@"
