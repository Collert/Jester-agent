#!/bin/bash -e
# The agent, the kiosk and their systemd units, installed by the same script as on a Pi set up by hand. build.sh put
# the agent and deploy/ in files/. Not staged in /tmp: on_chroot mounts an empty tmpfs over the image's /tmp.
install -d "${ROOTFS_DIR}/opt/jester-install"
cp -r files/. "${ROOTFS_DIR}/opt/jester-install/"
on_chroot << CHROOT
sh /opt/jester-install/deploy/install.sh /opt/jester-install/bin/jester-agent-linux-arm64
rm -rf /opt/jester-install
CHROOT
