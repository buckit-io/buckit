#!/usr/bin/env bash
set -euo pipefail

if [[ $# -eq 0 ]]; then
  echo "usage: $0 <presigned-url-1> [<presigned-url-2> ...]" >&2
  exit 1
fi

for url in "$@"; do
  sync
  echo 3 > /proc/sys/vm/drop_caches
  curl --http1.1 -sS -o /dev/null -w "%{time_starttransfer}\n" "$url"
done
