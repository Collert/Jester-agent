#!/bin/bash -e
# Builds the dongle's SD card image with pi-gen, Raspberry Pi's own image builder, in Docker. Needs Linux (or WSL)
# with Docker and qemu-user-static, and the agent built for the Pi first (make pi):
#
#   JESTER_PASSWORD=… [JESTER_SSH_KEY="ssh-ed25519 …"] image/build.sh
#
# The image lands in image/pi-gen/deploy/. GitHub Actions runs this too (.github/workflows/image.yml).
cd "$(dirname "$0")"

# Raspberry Pi OS Bookworm, 64-bit, as released on this date. Moving to a newer one is changing this line.
PIGEN_TAG=2026-10-06-raspios-bookworm-arm64
BIN=../bin/jester-agent-linux-arm64

[ -f "$BIN" ] || { echo "Build the agent first: make pi"; exit 1; }
[ -n "${JESTER_PASSWORD:-}" ] || { echo "Set JESTER_PASSWORD: the password of the jester user (for SSH)"; exit 1; }

rm -rf pi-gen
git clone --quiet --depth 1 --branch "$PIGEN_TAG" https://github.com/RPi-Distro/pi-gen pi-gen

# The stage and what it installs go inside pi-gen: its Docker build only sees that folder.
cp -r stage-jester pi-gen/
chmod +x pi-gen/stage-jester/prerun.sh pi-gen/stage-jester/*/*.sh # pi-gen skips scripts that aren't executable
files=pi-gen/stage-jester/00-agent/files
mkdir -p "$files/bin"
cp "$BIN" "$files/bin/"
cp -r ../deploy "$files/"

{
	cat config
	printf 'FIRST_USER_PASS=%q\n' "$JESTER_PASSWORD"
	if [ -n "${JESTER_SSH_KEY:-}" ]; then printf 'PUBKEY_SSH_FIRST_USER=%q\n' "$JESTER_SSH_KEY"; fi
} > pi-gen/config

cd pi-gen
./build-docker.sh
ls -lh deploy/
