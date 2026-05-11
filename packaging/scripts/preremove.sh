#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop buckit.service >/dev/null 2>&1 || true
fi

exit 0
