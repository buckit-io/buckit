#!/bin/sh
set -e

if ! getent group buckit >/dev/null 2>&1; then
    groupadd --system buckit
fi

if ! getent passwd buckit >/dev/null 2>&1; then
    useradd --system \
            --gid buckit \
            --no-create-home \
            --home-dir /var/lib/buckit \
            --shell /sbin/nologin \
            --comment "Buckit Object Storage" \
            buckit
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

exit 0
