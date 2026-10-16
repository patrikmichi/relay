#!/bin/sh
# Stops the service before its binary goes away. The identity and the config
# are kept, so a reinstall picks up where the agent left off.
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop zuano-connector.service >/dev/null 2>&1 || true
  systemctl disable zuano-connector.service >/dev/null 2>&1 || true
fi
