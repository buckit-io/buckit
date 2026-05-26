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
STATE_DIR="$SCRIPT_DIR/.state"

usage() {
	cat <<EOF
Usage: $0 <command> [options]

Commands:
  create   Create a new test cluster
  expand   Add nodes to an existing cluster
  destroy  Tear down the cluster

Options:
  -n, --nodes NUM          Number of nodes (default: $NODES)
  -d, --drives NUM         Drives per node (default: $DRIVES)
  -s, --drive-size SIZE    Size per drive, e.g. 1G, 500M (default: $DRIVE_SIZE)
  -m, --memory SIZE        RAM limit per container, e.g. 2G, 512M (default: $MEMORY)
  -i, --image IMAGE        Base Docker image (default: $IMAGE)
  --ssh-base-port PORT     First host port mapped to container SSH (default: $SSH_BASE_PORT)
  --ssh-password PASS      Root password for SSH in test containers (default: $SSH_PASSWORD)
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
		-N | --name)
			CLUSTER_NAME="$2"
			shift 2
			;;
		-h | --help) usage ;;
		*) shift ;;
		esac
	done
}

generate_compose() {
	local start_node="$1"
	local num_nodes="$2"
	local total_nodes="$3"
	local compose_file="$4"

	# Build the server endpoint string for all nodes
	local endpoints=""
	for i in $(seq 1 "$total_nodes"); do
		endpoints+="http://node${i}:9000/data/drive{0...$((DRIVES - 1))} "
	done
	endpoints="${endpoints% }"

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

	# Generate Dockerfile
	generate_dockerfile "$IMAGE" "$SCRIPT_DIR/Dockerfile"

	# Generate docker-compose.yml
	generate_compose 1 "$NODES" "$NODES" "$SCRIPT_DIR/docker-compose.yml"

	save_state

	# Build and start
	docker compose -f "$SCRIPT_DIR/docker-compose.yml" up -d --build

	echo ""
	echo "Cluster '${CLUSTER_NAME}' is starting."
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

	# Regenerate everything with new total
	NODES=$new_total
	generate_dockerfile "$IMAGE" "$SCRIPT_DIR/Dockerfile"
	generate_compose 1 "$new_total" "$new_total" "$SCRIPT_DIR/docker-compose.yml"

	save_state

	# Recreate with new config
	docker compose -f "$SCRIPT_DIR/docker-compose.yml" up -d --build

	echo ""
	echo "Cluster expanded to ${new_total} nodes."
}

cmd_destroy() {
	if [ -f "$STATE_DIR/config" ]; then
		source "$STATE_DIR/config"
	fi
	parse_args "$@"

	echo "Destroying cluster '${CLUSTER_NAME}'..."

	if [ -f "$SCRIPT_DIR/docker-compose.yml" ]; then
		docker compose -f "$SCRIPT_DIR/docker-compose.yml" down -v --remove-orphans 2>/dev/null || true
	fi

	rm -f "$SCRIPT_DIR/Dockerfile" "$SCRIPT_DIR/docker-compose.yml"
	rm -rf "$STATE_DIR"

	echo "Cluster destroyed."
}

# Main
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
*) usage ;;
esac
