#!/bin/sh
# Reloads systemd and tells the administrator what is left to do. The service
# is not started: it needs an enrollment and a config first.
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload || true
fi
if [ ! -f /var/lib/zuano-connector/identity.json ]; then
  echo "zuano-connector is installed but not enrolled."
  echo "  sudo -u zuano-connector zuano-connector enroll --token nxt_... --url https://<gateway> --config /etc/zuano-connector/config.yaml --state-dir /var/lib/zuano-connector"
  echo "  sudo systemctl enable --now zuano-connector"
fi
