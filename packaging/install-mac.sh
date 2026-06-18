#!/bin/sh
# install-mac.sh — macOS installer helper for buckit.
#
# Downloads the macOS (Apple Silicon) buckit binary for the latest stable
# release to a predictable filename (buckit), verifies its published SHA-256
# checksum, clears the macOS quarantine attribute, and leaves it in place so
# you can run it directly. It does NOT install it onto your PATH for you.
#
# Usage:
#   curl -fsSL https://buckit-io.github.io/buckit/install-mac.sh | sh
#   ./buckit --help   # run it from where it was downloaded
#
# Environment overrides:
#   BUCKIT_PAGES_BASE     gh-pages base URL
#                         (default: https://buckit-io.github.io/buckit)
#   BUCKIT_RELEASE_BASE   release download base
#                         (default: https://github.com/buckit-io/buckit/releases/download)
#   BUCKIT_VERSION        pin a release tag (e.g. RELEASE.2026-05-11T17-20-40Z)
#                         instead of resolving the latest stable release
#   BUCKIT_DOWNLOAD_DIR   directory to download the binary into
#                         (default: the current directory)

set -eu

PAGES_BASE="${BUCKIT_PAGES_BASE:-https://buckit-io.github.io/buckit}"
RELEASE_BASE="${BUCKIT_RELEASE_BASE:-https://github.com/buckit-io/buckit/releases/download}"

err() {
	echo "install-mac.sh: $*" >&2
	exit 1
}

info() {
	echo "==> $*"
}

# detect_platform requires macOS on Apple Silicon — the only published darwin
# build — and sets ARCH accordingly.
detect_platform() {
	os="$(uname -s)"
	[ "$os" = "Darwin" ] || err "this installer is for macOS (detected '$os'). On Linux use install-linux.sh; on Windows use install-windows.ps1"

	arch="$(uname -m)"
	case "$arch" in
	arm64 | aarch64) ARCH="arm64" ;;
	*) err "unsupported architecture '$arch'. Only Apple Silicon (arm64) builds are published." ;;
	esac
}

# fetch URL -> stdout
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO- "$1"
	else
		err "need curl or wget to download"
	fi
}

# fetch_to URL FILE
fetch_to() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		err "need curl or wget to download"
	fi
}

# sha256_of FILE -> hex digest on stdout
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		err "need sha256sum or shasum to verify the download"
	fi
}

# resolve_tag echoes the release tag to install, either the pinned
# BUCKIT_VERSION or the latest stable tag from the gh-pages pointer.
resolve_tag() {
	if [ -n "${BUCKIT_VERSION:-}" ]; then
		echo "$BUCKIT_VERSION"
		return
	fi

	pointer_url="$PAGES_BASE/server/buckit/release/darwin-$ARCH/buckit.sha256sum"
	# Pointer format: "<sha256>  buckit.<tag>"
	pointer="$(fetch "$pointer_url")" || err "could not fetch release pointer at $pointer_url"
	name="$(echo "$pointer" | awk '{print $2}')"
	tag="${name#buckit.}"
	case "$tag" in
	RELEASE.*) ;;
	*) err "unexpected release pointer payload: $pointer" ;;
	esac
	echo "$tag"
}

main() {
	detect_platform
	info "platform: darwin-$ARCH"

	tag="$(resolve_tag)"
	[ -n "$tag" ] || err "could not resolve a release tag"
	info "release: $tag"

	asset="buckit-darwin-$ARCH.$tag"
	download_url="$RELEASE_BASE/$tag/$asset"

	dldir="${BUCKIT_DOWNLOAD_DIR:-.}"
	mkdir -p "$dldir"
	binfile="$dldir/buckit"

	# Download to a temporary sibling and only move it into the predictable
	# path after the checksum verifies, so a failed or interrupted download
	# can never clobber an existing good binary or leave a partial/unverified
	# file at the path the printed command references.
	tmpfile="$(mktemp "$dldir/.buckit.XXXXXX")" || err "could not create temp file in $dldir"
	trap 'rm -f "$tmpfile"' EXIT

	info "downloading $asset"
	fetch_to "$download_url" "$tmpfile" || err "download failed: $download_url"

	info "fetching published checksum"
	want_sha="$(fetch "$download_url.sha256sum" | awk '{print $1}')" ||
		err "could not fetch checksum at $download_url.sha256sum"
	[ -n "$want_sha" ] || err "release checksum is empty"

	got_sha="$(sha256_of "$tmpfile")"
	if [ "$got_sha" != "$want_sha" ]; then
		err "checksum mismatch (expected $want_sha, got $got_sha) — refusing to continue"
	fi
	info "sha256 verified"

	chmod 0755 "$tmpfile"
	# Clear the quarantine attribute so Gatekeeper doesn't block the unsigned
	# binary on first run.
	if command -v xattr >/dev/null 2>&1; then
		xattr -d com.apple.quarantine "$tmpfile" 2>/dev/null || true
	fi

	mv -f "$tmpfile" "$binfile"
	trap - EXIT

	echo
	echo "Downloaded and verified:"
	echo "  $binfile"
	echo
	echo "Run it from here:"
	echo "  \"$binfile\""
	echo
	echo "(Move it onto your PATH to call 'buckit' from anywhere.)"
	echo
}

main "$@"
