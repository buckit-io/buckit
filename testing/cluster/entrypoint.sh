#!/bin/bash
# Container entrypoint: create loopback XFS drives, then exec systemd.
set -e

DRIVES=${DRIVES:-4}
DRIVE_SIZE=${DRIVE_SIZE:-1G}
DATA_DIR=${DATA_DIR:-/data}
SSH_ROOT_PASSWORD=${SSH_ROOT_PASSWORD:-buckitadmin}
HDD_DELAY_MS=${HDD_DELAY_MS:-0}
NETEM_DELAY=${NETEM_DELAY:-0ms}
NETEM_JITTER=${NETEM_JITTER:-0ms}

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
	printf 'MINIO_STORAGE_CLASS_STANDARD=%q\n' "${MINIO_STORAGE_CLASS_STANDARD:-}"
	printf 'MINIO_PROMETHEUS_AUTH_TYPE=%q\n' "${MINIO_PROMETHEUS_AUTH_TYPE:-public}"
} >/etc/buckit/buckit.env
chmod 0600 /etc/buckit/buckit.env

# Ensure loop device support
modprobe loop 2>/dev/null || true
if [ "$HDD_DELAY_MS" -gt 0 ] 2>/dev/null; then
	modprobe dm_mod 2>/dev/null || true
	modprobe dm_delay 2>/dev/null || true
	if ! dmsetup targets 2>/dev/null | awk '{print $1}' | grep -qx 'delay'; then
		echo "HDD_DELAY_MS=${HDD_DELAY_MS} requested, but the Linux kernel in this container does not expose the device-mapper delay target." >&2
		echo "Run the Docker rig on a native Linux host with dm-delay support, or disable HDD_DELAY_MS." >&2
		exit 1
	fi
fi

if [ "$NETEM_DELAY" != "0ms" ] && [ "$NETEM_DELAY" != "0us" ] && [ "$NETEM_DELAY" != "0" ]; then
	modprobe sch_netem 2>/dev/null || true
	netem_args="delay ${NETEM_DELAY}"
	if [ "$NETEM_JITTER" != "0ms" ] && [ "$NETEM_JITTER" != "0us" ] && [ "$NETEM_JITTER" != "0" ]; then
		netem_args="${netem_args} ${NETEM_JITTER} distribution normal"
	fi
	if ! tc qdisc add dev eth0 root netem ${netem_args}; then
		echo "NETEM_DELAY=${NETEM_DELAY} NETEM_JITTER=${NETEM_JITTER} requested, but tc netem could not be applied on eth0." >&2
		exit 1
	fi
fi

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

create_delayed_device() {
	local backing="$1"
	local delay_ms="$2"
	local mapper_name="$3"
	local sectors table

	sectors=$(blockdev --getsz "$backing")
	if [ -z "$sectors" ]; then
		echo "failed to determine size for $backing" >&2
		return 1
	fi

	dmsetup remove "$mapper_name" 2>/dev/null || true
	table="0 ${sectors} delay ${backing} 0 ${delay_ms}"
	dmsetup create "$mapper_name" --table "$table"
	dmsetup mknodes "$mapper_name" 2>/dev/null || true
	for _try in $(seq 1 20); do
		if [ -e "/dev/mapper/${mapper_name}" ]; then
			break
		fi
		sleep 0.1
	done
	if [ ! -e "/dev/mapper/${mapper_name}" ]; then
		echo "failed to materialize /dev/mapper/${mapper_name}" >&2
		return 1
	fi
	echo "/dev/mapper/${mapper_name}"
}

for i in $(seq 0 $((DRIVES - 1))); do
	img="/var/lib/buckit-drives/drive${i}.img"
	mnt="${DATA_DIR}/drive${i}"
	mapper_name="buckit-delay-$(hostname)-${i}"
	mkdir -p "$mnt"
	is_new=0
	if [ ! -f "$img" ]; then
		fallocate -l "$DRIVE_SIZE" "$img"
		is_new=1
	fi
	loop=$(attach_loop "$img")
	device="$loop"
	if [ "$HDD_DELAY_MS" -gt 0 ] 2>/dev/null; then
		device=$(create_delayed_device "$loop" "$HDD_DELAY_MS" "$mapper_name")
	fi
	if [ "$is_new" -eq 1 ]; then
		mkfs.xfs -f "$device"
	fi
	mount "$device" "$mnt"
done

exec /sbin/init
