#!/bin/sh
# Sets up a Raspberry Pi (Raspberry Pi OS Trixie Lite, 64-bit) as a Pavilion visual dongle.
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
apt-get install -y -qq network-manager dnsmasq-base curl fonts-dejavu-core
# The kiosk's browser: cog (WPE WebKit), which draws straight onto the display. dbus-daemon is for the D-Bus session
# the agent calls it over; the GStreamer packages are what WebKit plays a screen's sound with.
apt-get install -y -qq cog dbus-daemon gstreamer1.0-plugins-good gstreamer1.0-alsa gstreamer1.0-libav
[ -x /usr/bin/cog ] && [ -x /usr/bin/cogctl ] || { echo "No cog: the kiosk can't run"; exit 1; }

echo "Kiosk user…"
id kiosk >/dev/null 2>&1 || useradd -m -G video,render,input,audio -s /usr/sbin/nologin kiosk
# The display and input devices for cog, and sound: screens play music and order callouts through the TV.
usermod -aG video,render,input,audio kiosk

echo "Memory…"
# A Pi Zero 2 W leaves Linux about 416 MB. Swapping to the SD card stalls everything for seconds at a time (and wears
# the card out): what doesn't fit is swapped to compressed memory (zram) instead, and never to the card.
ZRAM=
if dpkg-query -W -f='${Status}' rpi-swap 2>/dev/null | grep -q "install ok installed"; then
	# Raspberry Pi OS Trixie: rpi-swap sets zram up, by default with a file on the card behind it. Without the file.
	install -d /etc/rpi/swap.conf.d
	cat > /etc/rpi/swap.conf.d/90-jester.conf <<EOF
[Main]
Mechanism=zram
EOF
else
	apt-get install -y -qq zram-tools
	cat > /etc/default/zramswap <<EOF
PERCENT=60
PRIORITY=100
EOF
	apt-get purge -y -qq dphys-swapfile 2>/dev/null || true
	ZRAM=zramswap.service
fi
cat > /etc/sysctl.d/90-jester-zram.conf <<EOF
vm.swappiness=100
vm.page-cluster=0
EOF
# Services a dongle has no use for (cellular modems, Bluetooth, mounting USB disks, batteries, hotkeys): each is
# memory the browser needs. Masked, since some start whenever another program asks for them.
for s in ModemManager bluetooth hciuart udisks2 upower triggerhappy; do
	systemctl disable $NOW "$s.service" 2>/dev/null || true
	systemctl mask "$s.service" 2>/dev/null || true
done

echo "Wi-Fi…"
# The Pi's Wi-Fi chip dozes between packets to save power, so what Pavilion sends (a screen just picked, a command)
# can wait or get lost on the way in. A dongle is always plugged in: keep the radio awake. This is the file
# raspi-config's own "Wi-Fi power saving: off" writes.
install -d /etc/NetworkManager/conf.d
cat > /etc/NetworkManager/conf.d/jester-wifi-powersave-off.conf <<EOF
[connection]
wifi.powersave = 2
EOF

echo "Agent…"
install -m 0755 "$BIN" /usr/local/bin/jester-agent
install -d -m 0700 /var/lib/jester
install -d /etc/NetworkManager/dnsmasq-shared.d
install -m 0644 deploy/captive-dnsmasq.conf /etc/NetworkManager/dnsmasq-shared.d/jester-captive.conf
install -m 0644 deploy/jester-agent.service /etc/systemd/system/jester-agent.service
install -m 0644 deploy/jester-kiosk.service /etc/systemd/system/jester-kiosk.service
install -m 0755 deploy/jester-kiosk deploy/jester-cogctl /usr/local/bin/
# The kiosk's settings for this dongle (see the file), kept when the agent is installed again.
[ -f /etc/default/jester-kiosk ] || install -m 0644 deploy/jester-kiosk.default /etc/default/jester-kiosk

# Wi-Fi must be allowed (Raspberry Pi OS keeps it blocked until a country is set): set yours with raspi-config.
rfkill unblock wifi || true

[ -z "$NOW" ] || systemctl daemon-reload
systemctl disable $NOW getty@tty1.service 2>/dev/null || true
# shellcheck disable=SC2086 # ZRAM is empty or one unit
systemctl enable $NOW NetworkManager.service $ZRAM jester-agent.service jester-kiosk.service
systemctl set-default graphical.target
echo "Done. The TV should show “Scan to start”."
