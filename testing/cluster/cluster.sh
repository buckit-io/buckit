#!/bin/bash
# Buckit/MinIO test cluster manager using Docker Compose.
# Creates containers with systemd, loopback XFS drives, and networking.
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/dockerfile-gen.sh"

# Defaults
CLUSTER_NAME="buckit-test"
NODES=4
DRIVES=4
DRIVE_SIZE="1G"
MEMORY="256M"
IMAGE="ubuntu:24.04"
BASE_PORT=9000
SSH_BASE_PORT=2201
SSH_PASSWORD="buckitadmin"
BUCKIT_FAST_GET="${BUCKIT_FAST_GET:-0}"
STATE_DIR="$SCRIPT_DIR/.state"

usage() {
	cat <<EOF
Usage: $0 <command> [options]

Commands:
  create        Create a new Buckit test cluster
  expand        Add nodes to an existing cluster
  destroy       Tear down the cluster
  minio start   Start a standalone MinIO container (migration source)
  minio stop    Stop and remove the MinIO migration container

Options (create / expand):
  -n, --nodes NUM          Number of nodes (default: $NODES)
  -d, --drives NUM         Drives per node (default: $DRIVES)
  -s, --drive-size SIZE    Size per drive, e.g. 1G, 500M (default: $DRIVE_SIZE)
  -m, --memory SIZE        RAM limit per container, e.g. 2G, 512M (default: $MEMORY)
  -i, --image IMAGE        Base Docker image (default: $IMAGE)
  --ssh-base-port PORT     First host port mapped to container SSH (default: $SSH_BASE_PORT)
  --ssh-password PASS      Root password for SSH in test containers (default: $SSH_PASSWORD)
  --fast-get VALUE         BUCKIT_FAST_GET value for Buckit nodes (default: $BUCKIT_FAST_GET)
  -N, --name NAME          Cluster name (default: $CLUSTER_NAME)
  -h, --help               Show this help

Supported images (must have systemd):
  Debian/Ubuntu:   ubuntu:24.04, ubuntu:22.04, debian:12, debian:11
  RHEL-family:     rockylinux:9, almalinux:9, fedora:40, amazonlinux:2023
                   quay.io/centos/centos:stream9
  Not supported:   Alpine (no systemd)
EOF
	exit 0
}

parse_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
		-n | --nodes)
			NODES="$2"
			shift 2
			;;
		-d | --drives)
			DRIVES="$2"
			shift 2
			;;
		-s | --drive-size)
			DRIVE_SIZE="$2"
			shift 2
			;;
		-m | --memory)
			MEMORY="$2"
			shift 2
			;;
		-i | --image)
			IMAGE="$2"
			shift 2
			;;
		--ssh-base-port)
			SSH_BASE_PORT="$2"
			shift 2
			;;
		--ssh-password)
			SSH_PASSWORD="$2"
			shift 2
			;;
		--fast-get)
			BUCKIT_FAST_GET="$2"
			shift 2
			;;
		-N | --name)
			CLUSTER_NAME="$2"
			shift 2
			;;
		-h | --help) usage ;;
		*) shift ;;
		esac
	done
}

build_buckit_binary() {
	local repo_root goos goarch

	repo_root="$(cd "$SCRIPT_DIR/../.." && pwd)"
	goos="${GOOS:-linux}"
	goarch="${GOARCH:-$(go env GOHOSTARCH)}"

	echo "Building Buckit binary for ${goos}/${goarch}..."
	(
		cd "$repo_root"
		CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -tags kqueue -trimpath \
			--ldflags "$(go run buildscripts/gen-ldflags.go)" \
			-o "$SCRIPT_DIR/buckit"
	)
}

generate_compose() {
	local start_node="$1"
	local num_nodes="$2"
	local total_nodes="$3"
	local compose_file="$4"

	# Build the server endpoint string as a SINGLE server pool spanning every
	# node, i.e. one erasure set across all drives. This makes SinglePool()==true
	# so GET dispatches straight to the set and the single-trip fast path can
	# actually bypass xl.meta. Emitting one arg per node instead would create one
	# pool per node, and multi-pool GET resolves the owning pool via an xl.meta
	# read (getLatestObjectInfoWithIdx) before the fast path ever runs — defeating
	# the whole experiment.
	local endpoints
	if [ "$total_nodes" -gt 1 ]; then
		endpoints="http://node{1...${total_nodes}}:9000/data/drive{0...$((DRIVES - 1))}"
	else
		endpoints="http://node1:9000/data/drive{0...$((DRIVES - 1))}"
	fi

	# Start or append compose file
	if [ "$start_node" -eq 1 ]; then
		cat >"$compose_file" <<EOF
name: ${CLUSTER_NAME}

services:
EOF
	fi

	for i in $(seq "$start_node" "$((start_node + num_nodes - 1))"); do
		local api_port=$((BASE_PORT + (i - 1) * 2))
		local console_port=$((api_port + 1))
		local ssh_port=$((SSH_BASE_PORT + i - 1))

		cat >>"$compose_file" <<EOF
  node${i}:
    build:
      context: .
      dockerfile: Dockerfile
    container_name: ${CLUSTER_NAME}-node${i}
    hostname: node${i}
    privileged: true
    mem_limit: ${MEMORY}
    dns:
      - 8.8.8.8
      - 1.1.1.1
    environment:
      - DRIVES=${DRIVES}
      - DRIVE_SIZE=${DRIVE_SIZE}
      - MINIO_ROOT_USER=buckitadmin
      - MINIO_ROOT_PASSWORD=buckitadmin
      - BUCKIT_ENDPOINTS=${endpoints}
      - BUCKIT_FAST_GET=${BUCKIT_FAST_GET}
      - SSH_ROOT_PASSWORD=${SSH_PASSWORD}
    volumes:
      - node${i}-drives:/var/lib/buckit-drives
    tmpfs:
      - /run
      - /run/lock
    ports:
      - "${api_port}:9000"
      - "${console_port}:9001"
      - "${ssh_port}:22"
    networks:
      - ${CLUSTER_NAME}-net

EOF
	done

	# Write volumes and network (rewrite tail section)
	# Remove old volumes/networks section if appending
	if [ "$start_node" -eq 1 ]; then
		cat >>"$compose_file" <<EOF
volumes:
EOF
		for i in $(seq 1 "$total_nodes"); do
			echo "  node${i}-drives:" >>"$compose_file"
		done

		cat >>"$compose_file" <<EOF

networks:
  ${CLUSTER_NAME}-net:
    driver: bridge
EOF
	fi
}

save_state() {
	mkdir -p "$STATE_DIR"
	cat >"$STATE_DIR/config" <<EOF
CLUSTER_NAME=${CLUSTER_NAME}
NODES=${NODES}
DRIVES=${DRIVES}
DRIVE_SIZE=${DRIVE_SIZE}
MEMORY=${MEMORY}
IMAGE=${IMAGE}
SSH_BASE_PORT=${SSH_BASE_PORT}
SSH_PASSWORD=${SSH_PASSWORD}
BUCKIT_FAST_GET=${BUCKIT_FAST_GET}
EOF
}

load_state() {
	if [ -f "$STATE_DIR/config" ]; then
		source "$STATE_DIR/config"
	else
		echo "Error: no existing cluster found. Run 'create' first."
		exit 1
	fi
}

cmd_create() {
	parse_args "$@"

	echo "Creating cluster '${CLUSTER_NAME}': ${NODES} nodes, ${DRIVES} drives/node (${DRIVE_SIZE} each), image: ${IMAGE}"

	build_buckit_binary

	# Generate Dockerfile
	generate_dockerfile "$IMAGE" "$SCRIPT_DIR/Dockerfile"

	# Generate docker-compose.yml
	generate_compose 1 "$NODES" "$NODES" "$SCRIPT_DIR/docker-compose.yml"

	save_state

	# Build the image once, then start nodes one by one.
	# Sequential startup prevents concurrent losetup calls from racing over the
	# same free loop device (TOCTOU: "Device or resource busy").
	docker compose -p "$CLUSTER_NAME" -f "$SCRIPT_DIR/docker-compose.yml" build
	for i in $(seq 1 "$NODES"); do
		echo "  Starting node${i}..."
		docker compose -p "$CLUSTER_NAME" -f "$SCRIPT_DIR/docker-compose.yml" up -d "node${i}"
	done

	echo ""
	echo "Cluster '${CLUSTER_NAME}' is up."
	echo "  Nodes: ${NODES}"
	echo "  API ports: $((BASE_PORT))-$((BASE_PORT + (NODES - 1) * 2)) (even ports)"
	echo "  Console ports: $((BASE_PORT + 1))-$((BASE_PORT + (NODES - 1) * 2 + 1)) (odd ports)"
	echo "  SSH ports: ${SSH_BASE_PORT}-$((SSH_BASE_PORT + NODES - 1))"
}

cmd_expand() {
	load_state
	local old_nodes=$NODES

	parse_args "$@"
	local add_nodes=$NODES
	local new_total=$((old_nodes + add_nodes))

	echo "Expanding cluster '${CLUSTER_NAME}': adding ${add_nodes} nodes (total: ${new_total})"

	build_buckit_binary

	# Regenerate everything with new total
	NODES=$new_total
	generate_dockerfile "$IMAGE" "$SCRIPT_DIR/Dockerfile"
	generate_compose 1 "$new_total" "$new_total" "$SCRIPT_DIR/docker-compose.yml"

	save_state

	# Build the image once, then start only the new nodes one by one.
	docker compose -p "$CLUSTER_NAME" -f "$SCRIPT_DIR/docker-compose.yml" build
	for i in $(seq $((old_nodes + 1)) "$new_total"); do
		echo "  Starting node${i}..."
		docker compose -p "$CLUSTER_NAME" -f "$SCRIPT_DIR/docker-compose.yml" up -d "node${i}"
	done

	echo ""
	echo "Cluster expanded to ${new_total} nodes."
}

cmd_destroy() {
	if [ -f "$STATE_DIR/config" ]; then
		source "$STATE_DIR/config"
	fi
	parse_args "$@"

	echo "Destroying Buckit cluster '${CLUSTER_NAME}'..."
	if [ -f "$SCRIPT_DIR/docker-compose.yml" ]; then
		docker compose -p "$CLUSTER_NAME" -f "$SCRIPT_DIR/docker-compose.yml" down -v 2>/dev/null || true
	fi
	rm -f "$SCRIPT_DIR/Dockerfile" "$SCRIPT_DIR/docker-compose.yml" "$SCRIPT_DIR/buckit"
	rm -rf "$STATE_DIR"
	echo "Buckit cluster destroyed."

	teardown_minio
}

# ---------------------------------------------------------------------------
# MinIO migration-test container
# ---------------------------------------------------------------------------

# Defaults for the standalone MinIO container (ports chosen above 19000 to
# avoid clashing with a simultaneously-running Buckit cluster).
MINIO_NODES=1
MINIO_DRIVES=4
MINIO_DRIVE_SIZE="1G"
MINIO_API_PORT=19000
MINIO_CONSOLE_PORT=19001
MINIO_SSH_PORT=2299
MINIO_ROOT_USER="minioadmin"
MINIO_ROOT_PASSWORD="minioadmin"
MINIO_SSH_PASSWORD="minioadmin"
MINIO_CONTAINER_NAME="minio-migration"
MINIO_STATE_DIR="$SCRIPT_DIR/.state-minio"

minio_usage() {
	cat <<EOF
Usage: $0 minio <subcommand> [options]

Subcommands:
  start   Build and start MinIO (single node or distributed cluster)
  stop    Stop and remove all MinIO containers (destroys drive data)

Options (start):
  -n, --nodes NUM      Number of MinIO nodes   (default: $MINIO_NODES)
  --drives NUM         Drives per node          (default: $MINIO_DRIVES)
  --drive-size SIZE    Size per drive           (default: $MINIO_DRIVE_SIZE)
  --api-port PORT      First host API port      (default: $MINIO_API_PORT)
  --console-port PORT  First host console port  (default: $MINIO_CONSOLE_PORT)
  --ssh-port PORT      First host SSH port      (default: $MINIO_SSH_PORT)
  --root-user USER     MINIO_ROOT_USER          (default: $MINIO_ROOT_USER)
  --root-password PASS MINIO_ROOT_PASSWORD      (default: $MINIO_ROOT_PASSWORD)
  --ssh-password PASS  Root SSH password        (default: $MINIO_SSH_PASSWORD)
  --name NAME          Cluster name prefix      (default: $MINIO_CONTAINER_NAME)
  -h, --help           Show this help

With --nodes 1 (default) a single standalone MinIO container is started.
With --nodes N>1 a distributed MinIO cluster is created: each node gets its
own loopback XFS drives and they all share MINIO_VOLUMES set to
  http://minio{1...N}:9000/mnt/data/drive{0...D-1}
MinIO recommends a minimum of 4 nodes for erasure-coded distributed mode.

Ports are allocated sequentially from the base values:
  Node i  API  = --api-port  + (i-1)*2
          Console = --api-port + (i-1)*2 + 1
          SSH  = --ssh-port + (i-1)

The container(s) run Rocky Linux 9 with MinIO installed from the official
ARM64 RPM.  Drive images live in named Docker volumes (one per node) and are
removed by 'minio stop'.

To connect the cluster to a running Buckit network for migration testing:
  docker network connect ${CLUSTER_NAME}-net ${MINIO_CONTAINER_NAME}-node1
  docker network connect ${CLUSTER_NAME}-net ${MINIO_CONTAINER_NAME}-node2
  ...  (single-node: docker network connect ${CLUSTER_NAME}-net ${MINIO_CONTAINER_NAME})
EOF
	exit 0
}

parse_minio_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
		-n | --nodes)
			MINIO_NODES="$2"
			shift 2
			;;
		--drives)
			MINIO_DRIVES="$2"
			shift 2
			;;
		--drive-size)
			MINIO_DRIVE_SIZE="$2"
			shift 2
			;;
		--api-port)
			MINIO_API_PORT="$2"
			shift 2
			;;
		--console-port)
			MINIO_CONSOLE_PORT="$2"
			shift 2
			;;
		--root-user)
			MINIO_ROOT_USER="$2"
			shift 2
			;;
		--root-password)
			MINIO_ROOT_PASSWORD="$2"
			shift 2
			;;
		--ssh-port)
			MINIO_SSH_PORT="$2"
			shift 2
			;;
		--ssh-password)
			MINIO_SSH_PASSWORD="$2"
			shift 2
			;;
		--name)
			MINIO_CONTAINER_NAME="$2"
			shift 2
			;;
		-h | --help) minio_usage ;;
		*) shift ;;
		esac
	done
}

generate_minio_compose() {
	local compose_file="$1"

	# Pre-compute MINIO_VOLUMES so it can be injected into every node's
	# environment.  Single-node uses local paths; distributed uses HTTP URLs
	# with MinIO's brace-expansion syntax so all nodes agree on the topology.
	local minio_volumes
	if [ "$MINIO_NODES" -eq 1 ]; then
		if [ "$MINIO_DRIVES" -eq 1 ]; then
			minio_volumes="/mnt/data/drive0"
		else
			minio_volumes="/mnt/data/drive{0...$((MINIO_DRIVES - 1))}"
		fi
	else
		if [ "$MINIO_DRIVES" -eq 1 ]; then
			minio_volumes="http://minio{1...${MINIO_NODES}}:9000/mnt/data/drive0"
		else
			minio_volumes="http://minio{1...${MINIO_NODES}}:9000/mnt/data/drive{0...$((MINIO_DRIVES - 1))}"
		fi
	fi

	# Write compose header
	cat >"$compose_file" <<EOF
name: ${MINIO_CONTAINER_NAME}

services:
EOF

	# One service block per node
	for i in $(seq 1 "$MINIO_NODES"); do
		local api_port=$((MINIO_API_PORT + (i - 1) * 2))
		local ssh_port=$((MINIO_SSH_PORT + i - 1))
		local console_port
		# Single-node honours --console-port; multi-node derives it from api+1.
		if [ "$MINIO_NODES" -eq 1 ]; then
			console_port=$MINIO_CONSOLE_PORT
		else
			console_port=$((api_port + 1))
		fi

		# Single-node keeps the bare service name for backward compatibility.
		local svc container_name
		if [ "$MINIO_NODES" -eq 1 ]; then
			svc="minio"
			container_name="${MINIO_CONTAINER_NAME}"
		else
			svc="minio${i}"
			container_name="${MINIO_CONTAINER_NAME}-node${i}"
		fi

		cat >>"$compose_file" <<EOF
  ${svc}:
    build:
      context: .
      dockerfile: Dockerfile.minio
    container_name: ${container_name}
    hostname: ${svc}
    privileged: true
    dns:
      - 8.8.8.8
      - 1.1.1.1
    environment:
      - DRIVES=${MINIO_DRIVES}
      - DRIVE_SIZE=${MINIO_DRIVE_SIZE}
      - MINIO_VOLUMES=${minio_volumes}
      - MINIO_ROOT_USER=${MINIO_ROOT_USER}
      - MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD}
      - SSH_ROOT_PASSWORD=${MINIO_SSH_PASSWORD}
    volumes:
      - ${svc}-drives:/var/lib/minio-drives
    tmpfs:
      - /run
      - /run/lock
    ports:
      - "${api_port}:9000"
      - "${console_port}:9001"
      - "${ssh_port}:22"
    networks:
      - ${MINIO_CONTAINER_NAME}-net

EOF
	done

	# Volumes section — one named volume per node
	cat >>"$compose_file" <<EOF
volumes:
EOF
	for i in $(seq 1 "$MINIO_NODES"); do
		local svc
		[ "$MINIO_NODES" -eq 1 ] && svc="minio" || svc="minio${i}"
		echo "  ${svc}-drives:" >>"$compose_file"
	done

	# Shared network — all nodes must be able to resolve each other by hostname
	cat >>"$compose_file" <<EOF

networks:
  ${MINIO_CONTAINER_NAME}-net:
    driver: bridge
EOF
}

save_minio_state() {
	mkdir -p "$MINIO_STATE_DIR"
	cat >"$MINIO_STATE_DIR/config" <<EOF
MINIO_CONTAINER_NAME=${MINIO_CONTAINER_NAME}
MINIO_NODES=${MINIO_NODES}
MINIO_DRIVES=${MINIO_DRIVES}
MINIO_DRIVE_SIZE=${MINIO_DRIVE_SIZE}
MINIO_API_PORT=${MINIO_API_PORT}
MINIO_CONSOLE_PORT=${MINIO_CONSOLE_PORT}
MINIO_SSH_PORT=${MINIO_SSH_PORT}
MINIO_ROOT_USER=${MINIO_ROOT_USER}
MINIO_ROOT_PASSWORD=${MINIO_ROOT_PASSWORD}
MINIO_SSH_PASSWORD=${MINIO_SSH_PASSWORD}
EOF
}

teardown_minio() {
	# Load saved MinIO state if available so we use the right project name.
	if [ -f "$MINIO_STATE_DIR/config" ]; then
		source "$MINIO_STATE_DIR/config"
	fi
	if [ -f "$SCRIPT_DIR/docker-compose.minio.yml" ]; then
		echo "Destroying MinIO cluster '${MINIO_CONTAINER_NAME}'..."
		docker compose -p "$MINIO_CONTAINER_NAME" -f "$SCRIPT_DIR/docker-compose.minio.yml" down -v 2>/dev/null || true
		echo "MinIO cluster destroyed."
	fi
	rm -f "$SCRIPT_DIR/docker-compose.minio.yml"
	rm -rf "$MINIO_STATE_DIR"
}

cmd_minio() {
	local subcmd="${1:-}"
	[ -n "$subcmd" ] && shift

	case "$subcmd" in
	start)
		parse_minio_args "$@"

		local label
		[ "$MINIO_NODES" -eq 1 ] && label="container" || label="cluster (${MINIO_NODES} nodes)"
		echo "Starting MinIO ${label} '${MINIO_CONTAINER_NAME}'..."
		echo "  Nodes:     ${MINIO_NODES}"
		echo "  Drives:    ${MINIO_DRIVES} x ${MINIO_DRIVE_SIZE} per node"
		echo "  API base:  ${MINIO_API_PORT}  (+2 per node)"
		echo "  SSH base:  ${MINIO_SSH_PORT}  (+1 per node)"

		generate_minio_compose "$SCRIPT_DIR/docker-compose.minio.yml"
		save_minio_state

		# Build the image once, then start nodes one by one.
		# Sequential startup prevents concurrent losetup calls from racing over
		# the same free loop device (TOCTOU: "Device or resource busy").
		docker compose -p "$MINIO_CONTAINER_NAME" -f "$SCRIPT_DIR/docker-compose.minio.yml" build
		for i in $(seq 1 "$MINIO_NODES"); do
			local svc
			[ "$MINIO_NODES" -eq 1 ] && svc="minio" || svc="minio${i}"
			echo "  Starting ${svc}..."
			docker compose -p "$MINIO_CONTAINER_NAME" -f "$SCRIPT_DIR/docker-compose.minio.yml" up -d "$svc"
		done

		echo ""
		if [ "$MINIO_NODES" -eq 1 ]; then
			echo "MinIO container '${MINIO_CONTAINER_NAME}' is starting."
			echo ""
			echo "  API:     http://localhost:${MINIO_API_PORT}"
			echo "  Console: http://localhost:${MINIO_CONSOLE_PORT}"
			echo "  SSH:     ssh root@localhost -p ${MINIO_SSH_PORT}  (password: ${MINIO_SSH_PASSWORD})"
			echo "  User:    ${MINIO_ROOT_USER} / ${MINIO_ROOT_PASSWORD}"
			echo ""
			echo "Tip: to attach to a running Buckit cluster network:"
			echo "  docker network connect ${CLUSTER_NAME}-net ${MINIO_CONTAINER_NAME}"
		else
			echo "MinIO cluster '${MINIO_CONTAINER_NAME}' is starting (${MINIO_NODES} nodes)."
			echo ""
			echo "  User: ${MINIO_ROOT_USER}  Pass: ${MINIO_ROOT_PASSWORD}  SSH pass: ${MINIO_SSH_PASSWORD}"
			echo ""
			printf "  %-8s  %-28s  %-30s  %s\n" "Node" "API" "Console" "SSH"
			printf "  %-8s  %-28s  %-30s  %s\n" "----" "---" "-------" "---"
			for i in $(seq 1 "$MINIO_NODES"); do
				local api_port=$((MINIO_API_PORT + (i - 1) * 2))
				local console_port=$((api_port + 1))
				local ssh_port=$((MINIO_SSH_PORT + i - 1))
				printf "  node%-4d  http://localhost:%-10d  http://localhost:%-12d  ssh root@localhost -p %d\n" \
					"$i" "$api_port" "$console_port" "$ssh_port"
			done
			echo ""
			echo "Tip: to attach each node to a running Buckit cluster network:"
			for i in $(seq 1 "$MINIO_NODES"); do
				echo "  docker network connect ${CLUSTER_NAME}-net ${MINIO_CONTAINER_NAME}-node${i}"
			done
		fi
		;;

	stop)
		parse_minio_args "$@"
		teardown_minio
		;;

	*) minio_usage ;;
	esac
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
case "${1:-}" in
create)
	shift
	cmd_create "$@"
	;;
expand)
	shift
	cmd_expand "$@"
	;;
destroy)
	shift
	cmd_destroy "$@"
	;;
minio)
	shift
	cmd_minio "$@"
	;;
*) usage ;;
esac
