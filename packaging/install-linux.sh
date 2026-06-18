#!/bin/sh
# install-linux.sh — Linux native-package installer helper for buckit.
#
# Detects this host's package manager (dnf/yum/zypper, apt/apt-get/dpkg, or
# apk), downloads the matching .rpm/.deb/.apk for the latest stable release,
# and verifies its published SHA-256 checksum.
#
# Modes:
#   (default)   Print the install command and stop. Does NOT run the package
#               manager — the final install (which needs root) is left to you
#               to review and run.
#   --install   Non-interactive: after a successful download and checksum
#               verification, run the detected install command automatically.
#               Intended for docs and automation.
#
# Usage:
#   # Print-only (default):
#   curl -fsSL https://buckit-io.github.io/buckit/install-linux.sh | sh
#
#   # Download, verify, and install automatically:
#   curl -fsSL https://buckit-io.github.io/buckit/install-linux.sh | sh -s -- --install
#
# Environment overrides:
#   BUCKIT_PAGES_BASE     gh-pages base URL
#                         (default: https://buckit-io.github.io/buckit)
#   BUCKIT_RELEASE_BASE   release download base
#                         (default: https://github.com/buckit-io/buckit/releases/download)
#   BUCKIT_VERSION        pin a release tag (e.g. RELEASE.2026-05-11T17-20-40Z)
#                         instead of resolving the latest stable release
#   BUCKIT_DOWNLOAD_DIR   directory to download the package into
#                         (default: a fresh temp directory)

set -eu

PAGES_BASE="${BUCKIT_PAGES_BASE:-https://buckit-io.github.io/buckit}"
RELEASE_BASE="${BUCKIT_RELEASE_BASE:-https://github.com/buckit-io/buckit/releases/download}"

err() {
	echo "install-linux.sh: $*" >&2
	exit 1
}

info() {
	echo "==> $*"
}

usage() {
	cat <<'EOF'
Usage: install-linux.sh [--install] [-h|--help]

  (default)   download + verify the package, then print the install command
  --install   download + verify, then run the install command automatically
EOF
}

# run_install PKGFILE runs the detected install command on PKGFILE.
# INSTALL_CMD holds fixed command words (none contain spaces or globs), so the
# unquoted expansion below intentionally word-splits it into separate
# arguments while PKGFILE stays a single quoted argument. This keeps execution
# reliable in POSIX sh without resorting to eval.
run_install() {
	# shellcheck disable=SC2086
	$INSTALL_CMD "$1"
}

# detect_arch sets ARCH to the Go-style arch tuple (amd64 / arm64) and
# requires a Linux host — native packages are Linux-only.
detect_arch() {
	os="$(uname -s)"
	[ "$os" = "Linux" ] || err "native packages are Linux-only (detected '$os'). For macOS/Windows download a binary from $RELEASE_BASE"

	arch="$(uname -m)"
	case "$arch" in
	x86_64 | amd64) ARCH="amd64" ;;
	arm64 | aarch64) ARCH="arm64" ;;
	*) err "unsupported architecture '$arch'. Supported: amd64, arm64." ;;
	esac
}

# detect_pkg sets PKG (rpm|deb|apk) and INSTALL_CMD (the command prefix used
# to install a local package file) based on the available package manager.
detect_pkg() {
	if command -v dnf >/dev/null 2>&1; then
		PKG="rpm"
		INSTALL_CMD="sudo dnf install"
	elif command -v yum >/dev/null 2>&1; then
		PKG="rpm"
		INSTALL_CMD="sudo yum install"
	elif command -v zypper >/dev/null 2>&1; then
		PKG="rpm"
		INSTALL_CMD="sudo zypper install"
	elif command -v apt >/dev/null 2>&1; then
		PKG="deb"
		INSTALL_CMD="sudo apt install"
	elif command -v apt-get >/dev/null 2>&1; then
		PKG="deb"
		INSTALL_CMD="sudo apt-get install"
	elif command -v apk >/dev/null 2>&1; then
		PKG="apk"
		INSTALL_CMD="sudo apk add --allow-untrusted"
	elif command -v rpm >/dev/null 2>&1; then
		PKG="rpm"
		INSTALL_CMD="sudo rpm -i"
	elif command -v dpkg >/dev/null 2>&1; then
		PKG="deb"
		INSTALL_CMD="sudo dpkg -i"
	else
		err "no supported package manager found (need dnf/yum/zypper, apt/dpkg, or apk)"
	fi
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

	pointer_url="$PAGES_BASE/server/buckit/release/linux-$ARCH/buckit.sha256sum"
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

# pkg_version TAG -> nfpm package version, mirroring the release workflow:
# RELEASE.2026-05-11T17-20-40Z[.rcN] -> 20260511172040.0.0
pkg_version() {
	echo "$1" | sed 's/^RELEASE\.//' | sed 's/\.rc[0-9]*$//' | tr -d '\-:TZ' | sed 's/$/.0.0/'
}

main() {
	install_mode=0
	while [ $# -gt 0 ]; do
		case "$1" in
		--install) install_mode=1 ;;
		-h | --help)
			usage
			exit 0
			;;
		*) err "unknown argument: $1 (try --help)" ;;
		esac
		shift
	done

	detect_arch
	detect_pkg
	info "platform: linux-$ARCH ($PKG)"

	tag="$(resolve_tag)"
	[ -n "$tag" ] || err "could not resolve a release tag"
	info "release: $tag"

	pkgver="$(pkg_version "$tag")"

	# Build the package filename. nfpm maps amd64->x86_64 / arm64->aarch64
	# for rpm and apk; deb keeps the Debian arch names.
	case "$PKG" in
	rpm)
		case "$ARCH" in
		amd64) pkgarch="x86_64" ;;
		arm64) pkgarch="aarch64" ;;
		esac
		asset="buckit-${pkgver}.${pkgarch}.rpm"
		;;
	deb)
		asset="buckit_${pkgver}_${ARCH}.deb"
		;;
	apk)
		case "$ARCH" in
		amd64) pkgarch="x86_64" ;;
		arm64) pkgarch="aarch64" ;;
		esac
		asset="buckit_${pkgver}_${pkgarch}.apk"
		;;
	esac

	download_url="$RELEASE_BASE/$tag/$asset"

	if [ -n "${BUCKIT_DOWNLOAD_DIR:-}" ]; then
		dldir="$BUCKIT_DOWNLOAD_DIR"
		mkdir -p "$dldir"
	else
		dldir="$(mktemp -d "${TMPDIR:-/tmp}/buckit-pkg.XXXXXX")"
	fi
	pkgfile="$dldir/$asset"

	info "downloading $asset"
	fetch_to "$download_url" "$pkgfile" || err "download failed: $download_url"

	info "fetching published checksum"
	want_sha="$(fetch "$download_url.sha256sum" | awk '{print $1}')" ||
		err "could not fetch checksum at $download_url.sha256sum"
	[ -n "$want_sha" ] || err "release checksum is empty"

	got_sha="$(sha256_of "$pkgfile")"
	if [ "$got_sha" != "$want_sha" ]; then
		err "checksum mismatch (expected $want_sha, got $got_sha) — refusing to continue"
	fi
	info "sha256 verified"

	echo
	echo "Downloaded and verified:"
	echo "  $pkgfile"
	echo

	if [ "$install_mode" -eq 1 ]; then
		info "installing package"
		echo "  $INSTALL_CMD \"$pkgfile\""
		run_install "$pkgfile"
	else
		echo "To install, run:"
		echo "  $INSTALL_CMD \"$pkgfile\""
		echo
	fi
}

main "$@"
