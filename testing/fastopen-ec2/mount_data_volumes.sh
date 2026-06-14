#!/usr/bin/env bash
set -euo pipefail

DATA_DEVICES=${DATA_DEVICES:-/dev/nvme1n1,/dev/nvme2n1}
MOUNT_POINTS=${MOUNT_POINTS:-/mnt/data1,/mnt/data2}
FILESYSTEM=${FILESYSTEM:-xfs}
LABEL_PREFIX=${LABEL_PREFIX:-buckitdata}
FSTAB=${FSTAB:-1}

IFS=',' read -r -a data_devices <<<"$DATA_DEVICES"
IFS=',' read -r -a mount_points <<<"$MOUNT_POINTS"

if [[ ${#data_devices[@]} -ne ${#mount_points[@]} ]]; then
  echo "DATA_DEVICES and MOUNT_POINTS must have the same length" >&2
  exit 1
fi

ensure_tool() {
  local tool=$1
  if command -v "$tool" >/dev/null 2>&1; then
    return 0
  fi
  if command -v dnf >/dev/null 2>&1; then
    sudo dnf install -y xfsprogs util-linux
    return 0
  fi
  if command -v yum >/dev/null 2>&1; then
    sudo yum install -y xfsprogs util-linux
    return 0
  fi
  echo "missing required tool: $tool" >&2
  exit 1
}

ensure_filesystem() {
  local device=$1
  local label=$2
  local fs_type
  fs_type=$(sudo blkid -o value -s TYPE "$device" 2>/dev/null || true)
  if [[ -n "$fs_type" ]]; then
    return 0
  fi
  case "$FILESYSTEM" in
    xfs)
      ensure_tool mkfs.xfs
      sudo mkfs.xfs -f -L "$label" "$device"
      ;;
    ext4)
      ensure_tool mkfs.ext4
      sudo mkfs.ext4 -F -L "$label" "$device"
      ;;
    *)
      echo "unsupported filesystem: $FILESYSTEM" >&2
      exit 1
      ;;
  esac
}

ensure_fstab() {
  local uuid=$1
  local mount_point=$2
  local fs_type=$3
  local entry="UUID=${uuid} ${mount_point} ${fs_type} defaults,nofail 0 2"
  if grep -Fq "UUID=${uuid} " /etc/fstab; then
    return 0
  fi
  printf '%s\n' "$entry" | sudo tee -a /etc/fstab >/dev/null
}

for idx in "${!data_devices[@]}"; do
  device=${data_devices[$idx]}
  mount_point=${mount_points[$idx]}
  label="${LABEL_PREFIX}$((idx + 1))"

  if [[ ! -b "$device" ]]; then
    echo "missing block device: $device" >&2
    exit 1
  fi

  ensure_filesystem "$device" "$label"
  sudo mkdir -p "$mount_point"

  fs_type=$(sudo blkid -o value -s TYPE "$device")
  uuid=$(sudo blkid -o value -s UUID "$device")

  if [[ "$FSTAB" == "1" ]]; then
    ensure_fstab "$uuid" "$mount_point" "$fs_type"
  fi

  if mountpoint -q "$mount_point"; then
    mounted_device=$(findmnt -n -o SOURCE --target "$mount_point")
    if [[ "$mounted_device" == "$device" || "$mounted_device" == "UUID=$uuid" ]]; then
      continue
    fi
    echo "mount point already used by another device: $mount_point -> $mounted_device" >&2
    exit 1
  fi

  sudo mount "$device" "$mount_point"
done

printf 'devices=%s\n' "$(IFS=,; echo "${data_devices[*]}")"
printf 'mount_points=%s\n' "$(IFS=,; echo "${mount_points[*]}")"
