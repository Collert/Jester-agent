#!/bin/sh
# Sets up a Raspberry Pi (Raspberry Pi OS Bookworm Lite, 64-bit) as a Pavilion visual dongle.
# Run as root from a checkout, after building the agent:  GOOS=linux GOARCH=arm64 make  (or copy bin/jester-agent).
# The SD card image (image/) runs it too, in a chroot while building: there everything is set up to start at boot,
# but nothing is running to start or stop now.
set -eu
cd "$(dirname "$0")/.."

NOW=--now
[ -d /run/systemd/system ] || NOW=

BIN=${1:-bin/jester-agent-linux-arm64}
[ -f "$BIN" ] || BIN=bin/jester-agent
[ -f "$BIN" ] || { echo "Build the agent first (make pi)"; exit 1; }

echo "Installing packages…"
apt-get update -qq
apt-get install -y -qq network-manager dnsmasq-base cage wlr-randr curl fonts-dejavu-core
apt-get install -y -qq chromium-browser 2>/dev/null || apt-get install -y -qq chromium
[ -x /usr/bin/chromium-browser ] || ln -sf /usr/bin/chromium /usr/bin/chromium-browser
# The kiosk runs this, not the chromium-browser script (see jester-kiosk.service).
[ -x /usr/lib/chromium/chromium ] || { echo "No /usr/lib/chromium/chromium: fix ExecStart in jester-kiosk.service"; exit 1; }

echo "Kiosk user…"
id kiosk >/dev/null 2>&1 || useradd -m -G video,render,input,audio -s /usr/sbin/nologin kiosk
usermod -aG audio kiosk # screens play sound (music, order callouts) through the TV
KIOSK_UID=$(id -u kiosk)

echo "Memory…"
# A Pi Zero 2 W leaves Linux about 416 MB and Chromium wants more. Swapping to the SD card stalls everything for
# seconds at a time (and wears the card out): swap to compressed memory (zram) instead.
apt-get install -y -qq zram-tools
cat > /etc/default/zramswap <<EOF
PERCENT=60
PRIORITY=100
EOF
cat > /etc/sysctl.d/90-jester-zram.conf <<EOF
vm.swappiness=100
vm.page-cluster=0
EOF
apt-get purge -y -qq dphys-swapfile 2>/dev/null || true
# Services a dongle has no use for (cellular modems, Bluetooth, mounting USB disks, batteries, hotkeys): each is
# memory the browser needs. Masked, since some start whenever another program asks for them.
for s in ModemManager bluetooth hciuart udisks2 upower triggerhappy; do
	systemctl disable $NOW "$s.service" 2>/dev/null || true
	systemctl mask "$s.service" 2>/dev/null || true
done

echo "Agent…"
install -m 0755 "$BIN" /usr/local/bin/jester-agent
install -d -m 0700 /var/lib/jester
install -d /etc/NetworkManager/dnsmasq-shared.d
install -m 0644 deploy/captive-dnsmasq.conf /etc/NetworkManager/dnsmasq-shared.d/jester-captive.conf
sed "s/@KIOSK_UID@/$KIOSK_UID/" deploy/jester-agent.service > /etc/systemd/system/jester-agent.service
install -m 0644 deploy/jester-kiosk.service /etc/systemd/system/jester-kiosk.service
# The kiosk only shows Pavilion's screens, and nobody can answer a permission prompt on it (local network, location,
# camera, …): Chromium's policies allow them all without asking.
install -d /etc/chromium/policies/managed
install -m 0644 deploy/chromium-policy.json /etc/chromium/policies/managed/jester.json

# Wi-Fi must be allowed (Bookworm keeps it blocked until a country is set): set yours with raspi-config.
rfkill unblock wifi || true

[ -z "$NOW" ] || systemctl daemon-reload
systemctl disable $NOW getty@tty1.service 2>/dev/null || true
systemctl enable $NOW NetworkManager.service zramswap.service jester-agent.service jester-kiosk.service
systemctl set-default graphical.target
echo "Done. The TV should show “Scan to start”."
