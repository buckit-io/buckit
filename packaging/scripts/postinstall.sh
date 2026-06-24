#!/bin/sh
set -e

if ! getent group buckit >/dev/null 2>&1; then
	groupadd --system buckit
fi

if ! getent passwd buckit >/dev/null 2>&1; then
	useradd --system \
		--gid buckit \
		--no-create-home \
		--home-dir /var/lib/buckit \
		--shell /sbin/nologin \
		--comment "Buckit Object Storage" \
		buckit
fi

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
	# Keep XFS retry-on-error disabled on the data drives. Safe to enable
	# even before buckit.service is started: the unit is a no-op on hosts
	# without XFS. Idempotent across upgrades.
	systemctl enable --now buckit-xfs-retry.timer >/dev/null 2>&1 || true
fi

exit 0
