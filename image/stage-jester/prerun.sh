#!/bin/bash -e
# Starts from Raspberry Pi OS Lite, as stage2 left it.
if [ ! -d "${ROOTFS_DIR}" ]; then
	copy_previous
fi
