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

echo "Kiosk user…"
id kiosk >/dev/null 2>&1 || useradd -m -G video,render,input -s /usr/sbin/nologin kiosk
KIOSK_UID=$(id -u kiosk)

echo "Agent…"
install -m 0755 "$BIN" /usr/local/bin/jester-agent
install -d -m 0700 /var/lib/jester
install -d /etc/NetworkManager/dnsmasq-shared.d
install -m 0644 deploy/captive-dnsmasq.conf /etc/NetworkManager/dnsmasq-shared.d/jester-captive.conf
sed "s/@KIOSK_UID@/$KIOSK_UID/" deploy/jester-agent.service > /etc/systemd/system/jester-agent.service
install -m 0644 deploy/jester-kiosk.service /etc/systemd/system/jester-kiosk.service

# Wi-Fi must be allowed (Bookworm keeps it blocked until a country is set): set yours with raspi-config.
rfkill unblock wifi || true

[ -z "$NOW" ] || systemctl daemon-reload
systemctl disable $NOW getty@tty1.service 2>/dev/null || true
systemctl enable $NOW NetworkManager.service jester-agent.service jester-kiosk.service
systemctl set-default graphical.target
echo "Done. The TV should show “Scan to start”."
