#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
	# On full removal (rpm passes "0", dpkg passes "remove"/"purge"), stop
	# and disable the XFS retry timer so it doesn't keep firing against a
	# script that no longer exists. On upgrade, leave it running.
	case "${1:-}" in
	0 | remove | purge)
		systemctl disable --now buckit-xfs-retry.timer >/dev/null 2>&1 || true
		;;
	esac
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

# Intentionally do NOT delete the buckit user/group on removal.
# It may own data on attached storage; orphaning files with a
# numeric UID is worse than leaving an unused system user.
# Operators who want full cleanup can `userdel buckit` manually.

exit 0
