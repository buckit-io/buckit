#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

# Intentionally do NOT delete the buckit user/group on removal.
# It may own data on attached storage; orphaning files with a
# numeric UID is worse than leaving an unused system user.
# Operators who want full cleanup can `userdel buckit` manually.

exit 0
