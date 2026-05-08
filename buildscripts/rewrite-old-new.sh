#!/bin/bash -e

set -E
set -o pipefail
set -x

source "$(dirname "$0")/local-host.sh"

WORK_DIR="$PWD/.verify-$RANDOM"
MINIO_CONFIG_DIR="$WORK_DIR/.minio"
MINIO_OLD=("$PWD/minio.RELEASE.2020-10-28T08-16-50Z" --config-dir "$MINIO_CONFIG_DIR" server)
MINIO=("$PWD/minio" --config-dir "$MINIO_CONFIG_DIR" server)
MINIO_HOST="$(minio_local_host)"
MINIO_OLD_RELEASE_URL="https://dl.minio.io/server/minio/release/linux-amd64/archive/minio.RELEASE.2020-10-28T08-16-50Z"

if [ ! -x "$PWD/minio" ]; then
	echo "minio executable binary not found in current directory"
	exit 1
fi

function download_old_release() {
	if [ -f minio.RELEASE.2020-10-28T08-16-50Z ]; then
		header="$(od -An -t x1 -N4 minio.RELEASE.2020-10-28T08-16-50Z 2>/dev/null | tr -d '[:space:]')"
		if [ "$header" = "7f454c46" ]; then
			chmod a+x minio.RELEASE.2020-10-28T08-16-50Z
			return 0
		fi

		rm -f minio.RELEASE.2020-10-28T08-16-50Z
	fi

	curl --fail --silent --show-error -L -o minio.RELEASE.2020-10-28T08-16-50Z "${MINIO_OLD_RELEASE_URL}"
	chmod a+x minio.RELEASE.2020-10-28T08-16-50Z
}

function fail() {
	echo "server1 log:"
	if [ -f "${WORK_DIR}/server1.log" ]; then
		cat "${WORK_DIR}/server1.log"
	fi
	echo "FAILED"
	purge "$WORK_DIR"
	exit 1
}

function wait_for_minio() {
	if ! timeout 2m "${WORK_DIR}/mc" ready minio/; then
		fail
	fi
}

function verify_rewrite() {
	start_port=$1

	export MINIO_ACCESS_KEY=minio
	export MINIO_SECRET_KEY=minio123
	export MC_HOST_minio="http://minio:minio123@${MINIO_HOST}:${start_port}/"
	unset MINIO_KMS_AUTO_ENCRYPTION # do not auto-encrypt objects
	export MINIO_CI_CD=1

	MC_BUILD_DIR="mc-$RANDOM"
	if ! git clone --quiet https://github.com/minio/mc "$MC_BUILD_DIR"; then
		echo "failed to download https://github.com/minio/mc"
		purge "${MC_BUILD_DIR}"
		exit 1
	fi

	(cd "${MC_BUILD_DIR}" && go build -o "$WORK_DIR/mc")

	# remove mc source.
	purge "${MC_BUILD_DIR}"

	"${MINIO_OLD[@]}" --address ":$start_port" "${WORK_DIR}/xl{1...16}" >"${WORK_DIR}/server1.log" 2>&1 &
	pid=$!
	disown $pid

	wait_for_minio

	if ! ps -p ${pid} 1>&2 >/dev/null; then
		fail
	fi

	"${WORK_DIR}/mc" mb minio/healing-rewrite-bucket --quiet --with-lock
	"${WORK_DIR}/mc" cp \
		buildscripts/verify-build.sh \
		minio/healing-rewrite-bucket/ \
		--disable-multipart --quiet

	"${WORK_DIR}/mc" cp \
		buildscripts/verify-build.sh \
		minio/healing-rewrite-bucket/ \
		--disable-multipart --quiet

	"${WORK_DIR}/mc" cp \
		buildscripts/verify-build.sh \
		minio/healing-rewrite-bucket/ \
		--disable-multipart --quiet

	kill ${pid}
	sleep 3

	"${MINIO[@]}" --address ":$start_port" "${WORK_DIR}/xl{1...16}" >"${WORK_DIR}/server1.log" 2>&1 &
	pid=$!
	disown $pid

	wait_for_minio

	if ! ps -p ${pid} 1>&2 >/dev/null; then
		fail
	fi

	if ! ./s3-check-md5 \
		-debug \
		-versions \
		-access-key minio \
		-secret-key minio123 \
		-endpoint "http://${MINIO_HOST}:${start_port}/" 2>&1 | grep INTACT; then
		echo "server1 log:"
		cat "${WORK_DIR}/server1.log"
		echo "FAILED"
		mkdir -p inspects
		(
			cd inspects
			"${WORK_DIR}/mc" admin inspect minio/healing-rewrite-bucket/verify-build.sh/**
		)

		"${WORK_DIR}/mc" mb play/inspects
		"${WORK_DIR}/mc" mirror inspects play/inspects

		purge "$WORK_DIR"
		exit 1
	fi

	go run ./buildscripts/heal-manual.go "${MINIO_HOST}:${start_port}" "minio" "minio123"
	sleep 1

	if ! ./s3-check-md5 \
		-debug \
		-versions \
		-access-key minio \
		-secret-key minio123 \
		-endpoint http://${MINIO_HOST}:${start_port}/ 2>&1 | grep INTACT; then
		echo "server1 log:"
		cat "${WORK_DIR}/server1.log"
		echo "FAILED"
		mkdir -p inspects
		(
			cd inspects
			"${WORK_DIR}/mc" admin inspect minio/healing-rewrite-bucket/verify-build.sh/**
		)

		"${WORK_DIR}/mc" mb play/inspects
		"${WORK_DIR}/mc" mirror inspects play/inspects

		purge "$WORK_DIR"
		exit 1
	fi

	kill ${pid}
}

function main() {
	mkdir -p "$WORK_DIR" "$MINIO_CONFIG_DIR"
	download_old_release

	start_port=$(shuf -i 10000-65000 -n 1)

	verify_rewrite ${start_port}
}

function purge() {
	rm -rf "$1"
}

(main "$@")
rv=$?
purge "$WORK_DIR"
exit "$rv"
