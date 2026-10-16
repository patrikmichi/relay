#!/bin/sh
# Creates the unprivileged service user and the log directory.
set -e
if ! getent group zuano-connector >/dev/null 2>&1; then
  groupadd --system zuano-connector
fi
if ! getent passwd zuano-connector >/dev/null 2>&1; then
  useradd --system --gid zuano-connector --home-dir /var/lib/zuano-connector --no-create-home --shell /usr/sbin/nologin zuano-connector
fi
install -d -o zuano-connector -g zuano-connector -m 0750 /var/lib/zuano-connector /var/log/zuano-connector
