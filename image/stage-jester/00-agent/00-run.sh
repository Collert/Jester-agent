#!/bin/bash -e
# The agent, the kiosk and their systemd units, installed by the same script as on a Pi set up by hand. build.sh put
# the agent and deploy/ in files/.
install -d "${ROOTFS_DIR}/tmp/jester"
cp -r files/. "${ROOTFS_DIR}/tmp/jester/"
on_chroot << CHROOT
sh /tmp/jester/deploy/install.sh /tmp/jester/bin/jester-agent-linux-arm64
rm -rf /tmp/jester
CHROOT
