#!/bin/sh
# Fix the data directory's owner as root, then run as the unprivileged user.
set -e
if [ "$(id -u)" = "0" ]; then
  chown -R app:app "${DATA_DIR:-/data}" 2>/dev/null || true
  exec su-exec app kaku "$@"
fi
exec kaku "$@"
