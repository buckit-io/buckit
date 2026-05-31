#!/bin/bash
# Container entrypoint for the MinIO migration test container.
#
# Steps performed at startup:
#   1. Set up loopback-backed XFS drives (same technique as the Buckit cluster).
#   2. Set ownership of all drive mount points to minio-user.
#   3. Write /etc/default/minio from environment variables (per MinIO RHEL
#      deployment guide).
#   4. Hand off to /sbin/init (systemd), which starts minio.service.
#
# Environment variables (set via docker compose):
#   DRIVES              Number of drives to create  (default: 4)
#   DRIVE_SIZE          Size of each drive image    (default: 1G)
#   MINIO_ROOT_USER     MinIO root / admin username (default: minioadmin)
#   MINIO_ROOT_PASSWORD MinIO root / admin password (default: minioadmin)
#   SSH_ROOT_PASSWORD   Root password for SSH access (default: minioadmin)
set -e

DRIVES=${DRIVES:-4}
DRIVE_SIZE=${DRIVE_SIZE:-1G}
MINIO_ROOT_USER=${MINIO_ROOT_USER:-minioadmin}
MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD:-minioadmin}
SSH_ROOT_PASSWORD=${SSH_ROOT_PASSWORD:-minioadmin}

# ---------------------------------------------------------------------------
# Set the root password so SSH password authentication works.
# ---------------------------------------------------------------------------
if command -v chpasswd >/dev/null 2>&1; then
	echo "root:${SSH_ROOT_PASSWORD}" | chpasswd
fi

# ---------------------------------------------------------------------------
# Loop device helpers (identical to the Buckit cluster entrypoint)
# ---------------------------------------------------------------------------

# Ensure loop device support is loaded in the host kernel.
modprobe loop 2>/dev/null || true

# Pre-populate /dev/loop0.../dev/loop63 so that losetup --find --show can open
# the device node immediately after the kernel atomically allocates its index.
# Without this a freshly-started container may be missing higher-numbered nodes.
for _n in $(seq 0 63); do
	[ -e "/dev/loop${_n}" ] || mknod "/dev/loop${_n}" b 7 "${_n}" 2>/dev/null || true
done
unset _n

# Detach any orphaned loop devices pointing to deleted backing files (e.g.
# from prior container runs on Docker Desktop, whose loop attachments leak
# into the host VM kernel after the container volume is removed).
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

# ---------------------------------------------------------------------------
# Create / mount XFS loopback drives
# ---------------------------------------------------------------------------

for i in $(seq 0 $((DRIVES - 1))); do
	img="/var/lib/minio-drives/drive${i}.img"
	mnt="/mnt/data/drive${i}"
	mkdir -p "$mnt"
	if [ ! -f "$img" ]; then
		fallocate -l "$DRIVE_SIZE" "$img"
		mkfs.xfs -f "$img"
	fi
	loop=$(attach_loop "$img")
	mount "$loop" "$mnt"
done

# Set ownership so the minio service (running as minio-user) can write data.
chown -R minio-user:minio-user /mnt/data

# ---------------------------------------------------------------------------
# Write /etc/default/minio  (per MinIO RHEL deployment guide)
# ---------------------------------------------------------------------------
# MINIO_VOLUMES is normally pre-computed by cluster.sh and injected via the
# compose environment — covering both single-node local paths and distributed
# http://minio{1...N}:9000/... URLs.  Fall back to local-path computation
# only when the variable is absent (e.g. a manual `docker run`).
if [ -z "${MINIO_VOLUMES:-}" ]; then
	if [ "$DRIVES" -eq 1 ]; then
		MINIO_VOLUMES="/mnt/data/drive0"
	else
		MINIO_VOLUMES="/mnt/data/drive{0...$((DRIVES - 1))}"
	fi
fi

cat >/etc/default/minio <<EOF
# MinIO environment file — written by minio-entrypoint.sh at container start.
# See: https://buckit-io.github.io/docs/operations/deployments/baremetal-deploy-minio-on-redhat-linux.html

MINIO_VOLUMES="${MINIO_VOLUMES}"
MINIO_OPTS="--console-address :9001"
MINIO_ROOT_USER=${MINIO_ROOT_USER}
MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD}
EOF

# ---------------------------------------------------------------------------
# Hand off to systemd — it will start minio.service via the wants symlink.
# ---------------------------------------------------------------------------
exec /sbin/init
