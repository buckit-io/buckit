#!/bin/sh
# install-binary.sh — standalone-binary installer helper for buckit.
#
# Downloads the buckit binary for this host (Linux or macOS) for the latest
# stable release to a predictable filename (buckit), verifies its published
# SHA-256 checksum, and leaves it in place so you can run it directly. It does
# NOT install it onto your PATH, and it does NOT register a service.
#
# On Linux, for a package-managed install with a systemd service, use
# install-linux.sh instead, which downloads the .deb/.rpm/.apk for this host.
#
# Also published as install-linux-binary.sh and install-mac.sh, the per-platform
# names this script replaced. Those URLs serve this same script.
#
# Usage:
#   curl -fsSL https://buckit-io.github.io/buckit/install-binary.sh | sh
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

# Set by resolve_release.
TAG=""
POINTER_SHA=""

err() {
	echo "install-binary.sh: $*" >&2
	exit 1
}

info() {
	echo "==> $*"
}

# detect_platform sets OS and ARCH to the Go-style tuple naming the published
# release assets, and validates the architecture against what is published for
# that platform: Linux ships amd64 and arm64, macOS ships Apple Silicon only.
detect_platform() {
	uname_s="$(uname -s)"
	case "$uname_s" in
	Linux) OS="linux" ;;
	Darwin) OS="darwin" ;;
	*) err "this installer is for Linux and macOS (detected '$uname_s'). On Windows use install-windows.ps1" ;;
	esac

	uname_m="$(uname -m)"
	case "$OS:$uname_m" in
	linux:x86_64 | linux:amd64) ARCH="amd64" ;;
	linux:aarch64 | linux:arm64) ARCH="arm64" ;;
	darwin:arm64 | darwin:aarch64) ARCH="arm64" ;;
	darwin:x86_64 | darwin:amd64) err "unsupported architecture '$uname_m'. Only Apple Silicon (arm64) builds are published for macOS." ;;
	*) err "unsupported architecture '$uname_m' on $OS." ;;
	esac
}

# validate_tag rejects anything that is not a plain release identifier. Both
# the pinned BUCKIT_VERSION and the pointer-resolved tag go through this, so a
# value like '../../evil' cannot reach the download URL as path traversal.
validate_tag() {
	case "$1" in
	RELEASE.?*) ;;
	*) err "unexpected release tag '$1' (expected RELEASE.*)" ;;
	esac
	# Only the characters real release tags use. Rejects '/', whitespace,
	# control characters, and URL delimiters such as '?' and '#'.
	case "$1" in
	*[!A-Za-z0-9._-]*) err "release tag '$1' contains unsupported characters" ;;
	esac
}

# normalize_sha lowercases a hex digest and requires exactly 64 hex characters,
# so a truncated or malformed checksum record can never be compared as if it
# were a valid digest. Echoes the normalized digest.
normalize_sha() {
	_sha="$(printf '%s' "$1" | tr 'ABCDEF' 'abcdef')"
	case "$_sha" in
	"" | *[!0-9a-f]*) err "malformed sha256 digest: '$1'" ;;
	esac
	[ "${#_sha}" -eq 64 ] || err "malformed sha256 digest: '$1'"
	printf '%s' "$_sha"
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

# sha256_of FILE -> normalized hex digest on stdout. The hash utility runs on
# its own rather than inside a pipeline, so a failure surfaces instead of being
# masked by the exit status of a downstream parser.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		_hash_out="$(sha256sum "$1")" || err "sha256sum failed on $1"
	elif command -v shasum >/dev/null 2>&1; then
		_hash_out="$(shasum -a 256 "$1")" || err "shasum failed on $1"
	else
		err "need sha256sum or shasum to verify the download"
	fi
	normalize_sha "$(printf '%s\n' "$_hash_out" | awk 'NR==1{print $1}')"
}

# resolve_release sets TAG, and POINTER_SHA when the release was resolved from
# the gh-pages pointer. A pinned BUCKIT_VERSION leaves POINTER_SHA empty: the
# pointer only ever describes the latest release, so it cannot vouch for an
# arbitrary pinned version.
resolve_release() {
	if [ -n "${BUCKIT_VERSION:-}" ]; then
		validate_tag "$BUCKIT_VERSION"
		TAG="$BUCKIT_VERSION"
		POINTER_SHA=""
		return
	fi

	pointer_url="$PAGES_BASE/server/buckit/release/$OS-$ARCH/buckit.sha256sum"
	# Pointer format: "<sha256>  buckit.<tag>"
	pointer="$(fetch "$pointer_url")" || err "could not fetch release pointer at $pointer_url"

	name="$(printf '%s\n' "$pointer" | awk 'NR==1{print $2}')"
	case "$name" in
	buckit.*) ;;
	*) err "unexpected release pointer payload: $pointer" ;;
	esac

	TAG="${name#buckit.}"
	validate_tag "$TAG"
	POINTER_SHA="$(normalize_sha "$(printf '%s\n' "$pointer" | awk 'NR==1{print $1}')")"
}

main() {
	detect_platform
	info "platform: $OS-$ARCH"

	resolve_release
	[ -n "$TAG" ] || err "could not resolve a release tag"
	info "release: $TAG"

	asset="buckit-$OS-$ARCH.$TAG"
	download_url="$RELEASE_BASE/$TAG/$asset"

	dldir="${BUCKIT_DOWNLOAD_DIR:-.}"
	mkdir -p "$dldir"
	binfile="$dldir/buckit"

	# Refuse to run when the destination is a directory. 'mv' would move the
	# temp file inside it and the script would report success while leaving
	# nothing runnable at the path it prints.
	[ ! -d "$binfile" ] || err "$binfile is a directory — remove it or set BUCKIT_DOWNLOAD_DIR"

	# Download to a temporary sibling and only move it into the predictable
	# path after the checksum verifies, so a failed or interrupted download
	# can never clobber an existing good binary or leave a partial/unverified
	# file at the path the printed command references.
	tmpfile="$(mktemp "$dldir/.buckit.XXXXXX")" || err "could not create temp file in $dldir"
	trap 'rm -f "$tmpfile"' EXIT

	info "downloading $asset"
	fetch_to "$download_url" "$tmpfile" || err "download failed: $download_url"

	info "fetching published checksum"
	# Capture the payload first: piping the fetch straight into a parser would
	# hide a failed transfer behind the parser's exit status.
	checksum_payload="$(fetch "$download_url.sha256sum")" ||
		err "could not fetch checksum at $download_url.sha256sum"
	release_sha="$(normalize_sha "$(printf '%s\n' "$checksum_payload" | awk 'NR==1{print $1}')")"

	# The binary and the checksum beside it come from the same origin, so that
	# digest alone only proves the download was not corrupted in transit. The
	# gh-pages pointer publishes the same digest from a separate origin;
	# when it is available, require the two to agree before trusting either.
	if [ -n "$POINTER_SHA" ]; then
		if [ "$POINTER_SHA" != "$release_sha" ]; then
			err "published digests disagree (pages $POINTER_SHA, release $release_sha) — refusing to continue"
		fi
		want_sha="$POINTER_SHA"
		info "sha256 cross-checked against the release pointer"
	else
		want_sha="$release_sha"
	fi

	got_sha="$(sha256_of "$tmpfile")"
	if [ "$got_sha" != "$want_sha" ]; then
		err "checksum mismatch (expected $want_sha, got $got_sha) — refusing to continue"
	fi
	info "sha256 verified"

	chmod 0755 "$tmpfile"

	# Clear the quarantine attribute so Gatekeeper doesn't block the unsigned
	# binary on first run. macOS only; harmless to skip elsewhere.
	if [ "$OS" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
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
