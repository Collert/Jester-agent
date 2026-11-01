#!/bin/bash -e
# Writes the list Raspberry Pi Imager reads to offer the dongle's image with its OS customisation settings (hostname,
# user, Wi-Fi and country, SSH, time zone). Imager only offers them for images whose list says how they take them:
# "systemd" is Raspberry Pi OS Bookworm's way (a firstrun.sh on the boot partition, run once on the first boot).
#
#   image/imager-list.sh 2026-11-01-jester.img.xz https://…/2026-11-01-jester.img.xz > jester-imager.json
img=$1
url=$2
cat <<JSON
{
  "os_list": [
    {
      "name": "Pavilion visual dongle",
      "description": "Raspberry Pi OS Lite (Bookworm, 64-bit) set up as a Pavilion visual dongle. For the Pi 3, 4, 5 and Zero 2 W.",
      "url": "$url",
      "release_date": "$(date -u +%Y-%m-%d)",
      "extract_size": $(xz -dc "$img" | wc -c),
      "extract_sha256": "$(xz -dc "$img" | sha256sum | cut -d' ' -f1)",
      "image_download_size": $(stat -c %s "$img"),
      "image_download_sha256": "$(sha256sum "$img" | cut -d' ' -f1)",
      "init_format": "systemd"
    }
  ]
}
JSON
