#!/bin/bash

function minio_local_host() {
	if [ -n "${MINIO_LOCALHOST:-}" ]; then
		echo "${MINIO_LOCALHOST}"
		return 0
	fi

	if [ -f /.dockerenv ] || [ -n "${ACT:-}" ]; then
		local host_ip
		host_ip=$(hostname -i 2>/dev/null | awk '{print $1}')
		if [ -n "${host_ip}" ]; then
			echo "${host_ip}"
			return 0
		fi
	fi

	echo "127.0.0.1"
}
