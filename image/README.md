# The SD card image

A ready-to-flash SD card for a dongle: Raspberry Pi OS Lite (Bookworm, 64-bit) with the agent and the kiosk already
installed. Flash it, put the card in a Pi behind a TV, power it on, and the TV says *Scan to start*. Nobody has to
log in or run `install.sh`.

It's built by [pi-gen](https://github.com/RPi-Distro/pi-gen), the tool Raspberry Pi builds Raspberry Pi OS with.
pi-gen builds the OS in stages (0–2 make Raspberry Pi OS Lite) and this folder adds one more, `stage-jester`, which
runs [deploy/install.sh](../deploy/install.sh) inside the image as if it were a Pi set up by hand.

| File | Does |
| --- | --- |
| `config` | pi-gen's settings: hostname `jester`, user `jester`, SSH on, Wi-Fi country, time zone |
| `stage-jester/00-agent` | installs the agent, Chromium, Cage and the systemd units (`deploy/install.sh`) |
| `stage-jester/01-display` | drives HDMI even when the TV is off at boot; keeps boot text off the TV |
| `build.sh` | fetches pi-gen (a fixed Bookworm release), puts the stage and the agent in it, builds in Docker |
| `imager-list.sh` | writes the list that makes Raspberry Pi Imager offer the image with its settings |

## Getting an image

**From GitHub (the easy way).** Add a repository secret `JESTER_PASSWORD` (*Settings › Secrets and variables ›
Actions*): the password of the `jester` user, for SSH. Optionally add `JESTER_SSH_KEY` too, a public key
(`ssh-ed25519 …`) to log in with instead. Then either:

- *Actions › SD card image › Run workflow*: when it's done (it builds on GitHub's ARM runners, free for public
  repositories) the run has a `jester-image` download, or
- push a tag (`git tag v0.2.0 && git push --tags`): the same build, published as a GitHub release with the agent
  binary, the image and Imager's list (`.github/workflows/release.yml`).

**On your own machine.** Linux or WSL, with Docker and `qemu-user-static` (the Pi's programs run emulated, so
expect an hour or more):

```sh
make pi
JESTER_PASSWORD=… image/build.sh     # the image lands in image/pi-gen/deploy/
```

## Flashing it

With [Raspberry Pi Imager](https://www.raspberrypi.com/software/):

- **With Imager's settings (hostname, Wi-Fi, SSH…).** Imager only offers its *OS customisation* for images it finds
  in a list that says the image takes them, not for a file picked with *Use custom*. Each release publishes that
  list; the newest is always at `https://github.com/<owner>/<repo>/releases/latest/download/jester-imager.json`.
  Open Imager with it, e.g. `rpi-imager --repo <that address>` (newer Imagers also take it in their settings), pick
  *Pavilion visual dongle*, and *Edit settings* works as for Raspberry Pi OS. The list and image have to be
  downloadable without logging in, so this needs a public repository (or the two files hosted somewhere public).
- **Without them.** *Choose OS › Use custom*, pick the `.img.xz`, and say *No* to customisation. The card has the
  defaults from `config`.

What the settings do on a dongle:

| Imager setting | Effect |
| --- | --- |
| Hostname | the Pi's name on the network (`jester.local` otherwise): handy with several dongles on one network |
| Username and password | replaces the `jester` user's |
| Wi-Fi (name, password, country) | sets the Wi-Fi country, and fills that network in on the phone's setup page. It isn't joined until setup on the phone, so the Pavilion address and code still come from there. Imager saves the password as a key, so the page says it was set in Imager and keeps it unless a new one is typed |
| Locale | time zone (the TV's clock) and keyboard |
| SSH | on or off, password or key |

## Defaults, and changing them

In `config`. SSH is on with the `jester` user and `JESTER_PASSWORD`: every card from one build has the same
password, so use a long one, or set `JESTER_SSH_KEY` and add `PUBKEY_ONLY_SSH=1` to `config` for keys only. The Wi-Fi
country (`WPA_COUNTRY`) must be right for where the dongle is used; Imager's Wi-Fi settings change it per card.

The OS version is the `PIGEN_TAG` line in `build.sh` (pi-gen's tags are named after Raspberry Pi OS releases).
Moving to Trixie (Raspberry Pi OS 13) needs more than that line: Imager customises Trixie with cloud-init rather than
`firstrun.sh` (`init_format` in `imager-list.sh`), and the Wi-Fi Imager saves turns up differently.

## Not there yet

- Updating dongles that are already out: the agent is built on its own for each release
  (`jester-agent-linux-arm64`) so it can fetch that later, but nothing fetches it yet.
- A read-only root file system, so pulling the power never damages the card.
