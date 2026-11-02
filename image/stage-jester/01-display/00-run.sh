#!/bin/bash -e
# The TV may be off, or on another input, when the dongle starts. Without a screen at boot the kiosk gets none until
# a reboot, so drive HDMI at 1080p regardless. And keep boot messages and the console cursor off the TV. psi=1: the
# kernel's memory pressure figures, which tell the agent a slow browser is starved rather than hung.
sed -i '1 s/$/ video=HDMI-A-1:1920x1080@60D quiet loglevel=3 logo.nologo consoleblank=0 vt.global_cursor_default=0 psi=1/' \
	"${ROOTFS_DIR}/boot/firmware/cmdline.txt"
