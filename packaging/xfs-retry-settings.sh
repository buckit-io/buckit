#!/bin/bash
# Disable XFS retry-on-error for Buckit's data drives only.
#
# Buckit handles XFS errors itself, so the kernel default ("retry forever")
# only adds latency. This sets max_retries=0 (fail immediately) for the EIO,
# ENOSPC, and default error classes — but ONLY on the block devices backing
# the mounts in MINIO_VOLUMES, never the root fs or unrelated XFS volumes.
#
# The data volumes are read from the running server's actual environment, so
# the env-file location doesn't matter and there's nothing to guess. If the
# server isn't running there's nothing to protect, so the script exits. The
# sysfs values reset on every mount (boot, drive hot-swap), so
# buckit-xfs-retry.timer re-runs this periodically.
set -u
set -f

[ -d /sys/fs/xfs ] || exit 0
command -v systemctl >/dev/null 2>&1 || exit 0
command -v findmnt >/dev/null 2>&1 || exit 0

# Find the running Buckit server (minio.service covers a host mid-migration)
# and read MINIO_VOLUMES from its process environment.
volumes=""
for unit in buckit.service minio.service; do
	pid="$(systemctl show "$unit" -p MainPID --value 2>/dev/null || true)"
	[ -n "$pid" ] && [ "$pid" -gt 0 ] 2>/dev/null || continue
	[ -r "/proc/$pid/environ" ] || continue
	volumes="$(tr '\0' '\n' <"/proc/$pid/environ" | sed -n 's/^MINIO_VOLUMES=//p' | tail -n1)"
	break
done
[ -n "$volumes" ] || exit 0 # server not running / no volumes — nothing to do

declare -A devs=()

# Expand one MINIO_VOLUMES token's Buckit ellipses into concrete paths
# WITHOUT eval, so a hostile value in the (root) process environment can't
# execute command substitutions. Supports one or more {start...end} groups
# of non-negative, optionally zero-padded decimal integers. Any token with
# an unsupported construct (hex, steps, reversed ranges, stray/nested braces,
# non-integers) yields nothing — fail closed rather than resolve a wrong path.
expand_token() {
	local tok="$1" pre rest grp a b a_n b_n width i v
	case "$tok" in
	*'{'*'...'*'}'*) ;;        # has an ellipsis group → parse below
	*'{'* | *'}'*) return 0 ;; # stray brace, no ellipsis → reject
	*)
		printf '%s\n' "$tok"
		return 0
		;;
	esac
	pre="${tok%%\{*}"  # before first '{'
	rest="${tok#*\{}"  # after  first '{'
	grp="${rest%%\}*}" # 'start...end'
	rest="${rest#*\}}" # remainder, may hold further groups
	case "$grp" in *...*)
		a="${grp%%...*}"
		b="${grp##*...}"
		;;
	*) return 0 ;; esac
	case "$a$b" in "" | *[!0-9]*) return 0 ;; esac # both must be all-digit, non-empty
	width=0
	case "$a" in 0?*) [ "${#a}" -gt "$width" ] && width="${#a}" ;; esac
	case "$b" in 0?*) [ "${#b}" -gt "$width" ] && width="${#b}" ;; esac
	a_n=$((10#$a))
	b_n=$((10#$b))                    # base-10 (tolerate leading zeros)
	[ "$a_n" -le "$b_n" ] || return 0 # reversed range → reject
	i="$a_n"
	while [ "$i" -le "$b_n" ]; do
		if [ "$width" -gt 0 ]; then v="$(printf "%0${width}d" "$i")"; else v="$i"; fi
		expand_token "${pre}${v}${rest}" # recurse for any remaining groups
		i=$((i + 1))
	done
}

# Resolve a data path to the kernel name of the block device backing its
# mount (as it appears under /sys/fs/xfs, e.g. sdb1, nvme0n1, dm-3). Never
# tunes "/", and never climbs to arbitrary ancestors, so an unmounted data
# drive can't cause the root fs to be modified.
device_for_path() {
	local p="$1" q parent target src
	[ "$p" = / ] && return 0
	parent="${p%/*}"
	[ -n "$parent" ] || parent=/
	if [ -e "$p" ]; then
		# The path exists, so findmnt --target returns the mount it lives on
		# by definition (any nesting depth). Tuning that fs is correct as long
		# as it isn't the root fs.
		q="$p"
		target="$(findmnt -fn -o TARGET --target "$q" 2>/dev/null)" || return 0
		[ -n "$target" ] && [ "$target" != / ] || return 0
	else
		# Data subdir not created yet (e.g. fresh hot-swap). Fall back to the
		# immediate parent, but only act when the parent is itself the
		# mountpoint — so we never tune an unrelated ancestor fs.
		[ -e "$parent" ] || return 0
		q="$parent"
		target="$(findmnt -fn -o TARGET --target "$q" 2>/dev/null)" || return 0
		[ -n "$target" ] && [ "$target" != / ] || return 0
		[ "$target" = "$parent" ] || return 0
	fi
	src="$(findmnt -fn -o SOURCE --target "$q" 2>/dev/null)" || return 0
	[ -n "$src" ] || return 0
	src="$(readlink -f "$src" 2>/dev/null || printf '%s' "$src")"
	devs["${src##*/}"]=1
}

# MINIO_VOLUMES is whitespace-separated, matching how the buckit server
# parses it; paths containing spaces are not supported.
for tok in $volumes; do
	# Drop any scheme://host[:port], keep the local path component.
	case "$tok" in
	*://*)
		rest="${tok#*://}"
		case "$rest" in */*) tok="/${rest#*/}" ;; *) continue ;; esac
		;;
	esac
	# Expand ellipses in the current shell so device_for_path can populate devs.
	expanded="$(expand_token "$tok")"
	while IFS= read -r path; do
		[ -n "$path" ] || continue
		case "$path" in /*) device_for_path "$path" ;; esac
	done <<<"$expanded"
done

[ "${#devs[@]}" -gt 0 ] || exit 0

# Expected no-ops (server down, no XFS, drive not mounted, unsupported
# pattern) stay silent and exit 0 above. Here the device exists and we mean
# to write, so a genuine write failure is logged to stderr (captured by the
# journal) and makes the unit exit non-zero — surfacing via `systemctl
# --failed` / `journalctl -u buckit-xfs-retry.service`. The timer retries
# every couple of minutes, so a transient failure self-clears.
failed=0
for name in "${!devs[@]}"; do
	d="/sys/fs/xfs/$name"
	[ -d "$d/error" ] || continue
	for class in EIO ENOSPC default; do
		f="$d/error/metadata/$class/max_retries"
		# Group the write so 2>/dev/null also covers a failed ">" open on a
		# read-only sysfs file (the redirection error precedes the command).
		if ! { echo 0 >"$f"; } 2>/dev/null; then
			printf 'buckit-xfs-retry: failed to set %s\n' "$f" >&2
			failed=1
		fi
	done
done

[ "$failed" -eq 0 ] || exit 1
exit 0
