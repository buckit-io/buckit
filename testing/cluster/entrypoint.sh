#!/bin/bash
# Container entrypoint: create loopback XFS drives, then exec systemd.
set -e

DRIVES=${DRIVES:-4}
DRIVE_SIZE=${DRIVE_SIZE:-1G}
DATA_DIR=${DATA_DIR:-/data}
SSH_ROOT_PASSWORD=${SSH_ROOT_PASSWORD:-buckitadmin}

if command -v chpasswd >/dev/null 2>&1; then
	echo "root:${SSH_ROOT_PASSWORD}" | chpasswd
fi

# Ensure loop device support
modprobe loop 2>/dev/null || true

# Detach any orphaned loop devices pointing to deleted backing files (e.g. from
# prior container runs on Docker Desktop, whose loop attachments leak into the
# host VM kernel even after the container's volume is removed).
if command -v losetup >/dev/null 2>&1; then
	losetup -a 2>/dev/null | awk -F: '/\(deleted\)/{print $1}' | while read -r stale; do
		losetup -d "$stale" 2>/dev/null || true
	done
fi

attach_loop() {
	# Allocate next free loop device, materializing its /dev node inside the
	# container if it doesn't already exist (the kernel may return an index
	# above the ones present in our /dev namespace).
	local img="$1"
	local loop num
	loop=$(losetup -f) || return 1
	num=${loop##/dev/loop}
	[ -e "$loop" ] || mknod "$loop" b 7 "$num" 2>/dev/null || true
	losetup "$loop" "$img" || return 1
	echo "$loop"
}

for i in $(seq 0 $((DRIVES - 1))); do
	img="/var/lib/buckit-drives/drive${i}.img"
	mnt="${DATA_DIR}/drive${i}"
	mkdir -p "$mnt"
	if [ ! -f "$img" ]; then
		fallocate -l "$DRIVE_SIZE" "$img"
		mkfs.xfs -f "$img"
	fi
	loop=$(attach_loop "$img")
	mount "$loop" "$mnt"
done

exec /sbin/init
