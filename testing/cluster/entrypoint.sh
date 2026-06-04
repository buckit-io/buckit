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

mkdir -p /etc/buckit
{
	printf 'MINIO_ROOT_USER=%q\n' "${MINIO_ROOT_USER:-buckitadmin}"
	printf 'MINIO_ROOT_PASSWORD=%q\n' "${MINIO_ROOT_PASSWORD:-buckitadmin}"
	printf 'MINIO_CONFIG_ENV_FILE=\n'
	printf 'BUCKIT_FAST_GET=%q\n' "${BUCKIT_FAST_GET:-0}"
	printf 'BUCKIT_ENDPOINTS=%q\n' "${BUCKIT_ENDPOINTS:-}"
} >/etc/buckit/buckit.env
chmod 0600 /etc/buckit/buckit.env

# Ensure loop device support
modprobe loop 2>/dev/null || true

# Pre-populate /dev/loop0.../dev/loop63 so that losetup --find --show can open
# the device node immediately after the kernel atomically allocates its index.
# Without this a freshly-started container may be missing higher-numbered nodes.
for _n in $(seq 0 63); do
	[ -e "/dev/loop${_n}" ] || mknod "/dev/loop${_n}" b 7 "${_n}" 2>/dev/null || true
done
unset _n

# Detach any orphaned loop devices pointing to deleted backing files (e.g. from
# prior container runs on Docker Desktop, whose loop attachments leak into the
# host VM kernel even after the container's volume is removed).
if command -v losetup >/dev/null 2>&1; then
	losetup -a 2>/dev/null | awk -F: '/\(deleted\)/{print $1}' | while read -r stale; do
		losetup -d "$stale" 2>/dev/null || true
	done
fi

attach_loop() {
	# losetup --find --show atomically selects the next free loop device and
	# attaches the image in one kernel operation (LOOP_CTL_GET_FREE +
	# LOOP_SET_FD).  The old two-step approach — losetup -f to read the next
	# free device, then a separate losetup <dev> <img> to attach — has a TOCTOU
	# race: when N nodes start concurrently they all observe the same "next
	# free" number before any of them has attached, so N-1 of them hit
	# "Device or resource busy".  The atomic form closes that window entirely.
	local img="$1"
	local loop num
	loop=$(losetup --find --show "$img") || return 1
	# Materialise the /dev node if the kernel assigned an index above the
	# pre-populated range inside this container's /dev namespace.
	num=${loop##/dev/loop}
	[ -e "$loop" ] || mknod "$loop" b 7 "$num" 2>/dev/null || true
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
