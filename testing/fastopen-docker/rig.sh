#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$ROOT_DIR/../.." && pwd)
CLUSTER_DIR="$REPO_ROOT/testing/cluster"
CLUSTER_SH="$CLUSTER_DIR/cluster.sh"

CLUSTER_NAME=${CLUSTER_NAME:-buckit-fastopen}
NODES=${NODES:-4}
DRIVES=${DRIVES:-4}
DRIVE_SIZE=${DRIVE_SIZE:-1G}
MEMORY=${MEMORY:-256M}
CPUS=${CPUS:-1.0}
IMAGE=${IMAGE:-ubuntu:24.04}
API_PORT=${API_PORT:-9000}
SSH_BASE_PORT=${SSH_BASE_PORT:-2201}

usage() {
  cat <<'EOF'
usage: testing/fastopen-docker/rig.sh <command>

commands:
  start-off    destroy any existing rig and recreate with BUCKIT_FAST_GET=0
  start-on     destroy any existing rig and recreate with BUCKIT_FAST_GET=1
  destroy      tear down the docker rig and remove volumes
  status       print docker container status for the rig
  endpoint     print the public S3 endpoint for node1
EOF
}

require_cluster_sh() {
  if [[ ! -x "$CLUSTER_SH" ]]; then
    echo "missing executable: $CLUSTER_SH" >&2
    exit 1
  fi
}

create_cluster() {
  local fast_get=$1
  require_cluster_sh
  (
    cd "$CLUSTER_DIR"
    "$CLUSTER_SH" destroy --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
    "$CLUSTER_SH" create \
      --name "$CLUSTER_NAME" \
      --nodes "$NODES" \
      --drives "$DRIVES" \
      --drive-size "$DRIVE_SIZE" \
      --memory "$MEMORY" \
      --cpus "$CPUS" \
      --image "$IMAGE" \
      --ssh-base-port "$SSH_BASE_PORT" \
      --fast-get "$fast_get"
  )
  printf 'cluster_name=%s\n' "$CLUSTER_NAME"
  printf 'fast_get=%s\n' "$fast_get"
  printf 'endpoint=http://127.0.0.1:%s\n' "$API_PORT"
  printf 'topology=%sx%s\n' "$NODES" "$DRIVES"
}

destroy_cluster() {
  require_cluster_sh
  (
    cd "$CLUSTER_DIR"
    "$CLUSTER_SH" destroy --name "$CLUSTER_NAME"
  )
}

status_cluster() {
  if ! command -v docker >/dev/null 2>&1; then
    echo "docker not found" >&2
    exit 1
  fi
  docker ps -a \
    --filter "name=${CLUSTER_NAME}-node" \
    --format 'name={{.Names}} status={{.Status}} ports={{.Ports}}'
}

case "${1:-}" in
  start-off)
    create_cluster 0
    ;;
  start-on)
    create_cluster 1
    ;;
  destroy)
    destroy_cluster
    ;;
  status)
    status_cluster
    ;;
  endpoint)
    printf 'http://127.0.0.1:%s\n' "$API_PORT"
    ;;
  *)
    usage
    exit 1
    ;;
esac
