#!/usr/bin/env bash
# The agent installed for real on this machine (Linux with systemd, or macOS with launchd), the
# way a customer runs it: install, the features of this release, an update's merge step, uninstall.
# Run from the repository root with the binary built for this machine at ./zharp-collector.
#
# The API key and endpoint are dummies: nothing is sent anywhere (exports fail and are retried),
# which is fine — what's checked is that everything installs, starts, stays up and is removed.
set -euxo pipefail

BIN="$(pwd)/zharp-collector"
chmod +x "$BIN"
"$BIN" --version

# Install, unattended.
sudo "$BIN" install --api-key zh_ci_dummy --endpoint http://127.0.0.1:9/api/v1 -y
sleep 8
sudo "$BIN" status

if [ "$(uname)" = "Linux" ]; then
  CONFIG=/etc/zharp-collector/config.yaml
  HELPER=/usr/local/lib/zharp-collector/zharp-inspect

  systemctl is-active zharp-collector
  # The service runs the copy in /usr/local/bin, not this one (a home folder its user can't enter).
  systemctl show -p ExecStart zharp-collector | grep -q 'path=/usr/local/bin/zharp-collector'
  # This release's receivers are in the config install wrote.
  for receiver in hostmetrics/processes zharp_disk_usage zharp_network_usage; do
    sudo grep -q "^  $receiver:" "$CONFIG"
  done

  # The read-only helper: its two permissions, its lock-down, and both of its jobs, run as the
  # agent's user the way the receivers run it.
  sudo getcap "$HELPER" | grep -q 'cap_dac_read_search,cap_sys_ptrace=ep'
  test "$(stat -c '%U:%G %a' "$HELPER")" = "root:zharp-collector 750"
  # Every drive on a real machine; in a container (the local distro runs) / is an overlay, not a
  # drive, so a folder is scanned instead.
  if [ "$(findmnt -n -o FSTYPE /)" = overlay ]; then
    sudo -u zharp-collector "$HELPER" inspect disk --top 3 --timeout 60s /usr
  else
    sudo -u zharp-collector "$HELPER" inspect disk --top 3 --timeout 60s
  fi
  sudo -u zharp-collector "$HELPER" inspect sockets | grep -q '"privileged":true'
  if sudo -u zharp-collector "$HELPER" --config "$CONFIG" validate; then
    echo "the helper ran something other than inspect" >&2
    exit 1
  fi
  # Network by program, live, with traffic of its own.
  (for _ in 1 2 3 4 5; do curl -s -o /dev/null https://github.com || true; sleep 0.5; done) &
  sudo "$BIN" inspect network --seconds 4
  wait
fi

# An update's merge step (what the new binary runs after replacing itself), run on a current
# config: it must change nothing that breaks the service.
sudo "$BIN" update --apply --from v1.9.4
sleep 5
sudo "$BIN" status
if [ "$(uname)" = "Linux" ]; then
  systemctl is-active zharp-collector
  # Still once each.
  test "$(sudo grep -c '^  zharp_network_usage:' /etc/zharp-collector/config.yaml)" = 1
fi

echo y | sudo "$BIN" uninstall
if [ "$(uname)" = "Linux" ]; then
  ! systemctl is-active zharp-collector
  test ! -e /usr/local/lib/zharp-collector
fi
echo "smoke test passed on $(uname -sm)"
