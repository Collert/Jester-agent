# jester-agent

The agent for **Pavilion visual dongles**: a Raspberry Pi behind a TV that shows one of
[Pavilion](https://github.com/collert/pavilion)'s screens (menu boards, pickup boards, signage). It's written in Go,
keeps its state in SQLite, and takes care of everything between "plugged in for the first time" and "showing the
menu board":

1. **Fresh out of the box**, the TV says *Scan to start* with a QR code. The dongle has turned on its own Wi-Fi
   (`Pavilion-Setup-7F3K`); the QR code joins a phone to it and the phone opens the setup page by itself (a captive
   portal at `http://10.42.0.1`).
2. **On the phone**: Pavilion's address, a pairing code from Pavilion (*Settings › Devices › Add visual dongle*), and
   the Wi-Fi the TV should use, picked from the networks the dongle can hear.
3. **On the TV**: each step as it happens (joining the Wi-Fi, getting an address, checking the code with Pavilion).
   If something goes wrong the TV says what (wrong password, code expired, address didn't answer), the setup Wi-Fi
   comes back on and the phone's details are kept, so it's a quick fix.
4. **Paired**: the dongle keeps a WebSocket open to Pavilion (no ports to open). Pavilion tells it which screen to
   show and which way the TV hangs, and can make it show its name, restart, or ask for a new Wi-Fi. Until a screen is
   assigned the TV says *No screen assigned* and where to pick one.

And afterwards, by itself:

- **The browser crashes or hangs**: it's started again (systemd), and the agent restarts it when it stops answering.
- **The screen doesn't load** (Pavilion down when the TV turns on): the TV shows the agent's page saying so and tries
  again every 30 seconds. Once a screen is up it keeps itself current and rides out Pavilion restarts on its own.
- **The connection to Pavilion dies quietly** (the Wi-Fi dropped for a moment, the router restarted): the agent asks
  Pavilion for a sign of life every 15 seconds and opens a new connection when none comes, so a screen picked in
  Pavilion doesn't wait on a connection that's no longer there.
- **The Wi-Fi goes away for 2 minutes**: the setup Wi-Fi comes back on and the TV shows a QR code to give it a new
  network. It stays paired, and keeps trying the old network every 5 minutes while nobody's on the setup page.
- **Unpaired in Pavilion**: back to *Scan to start*.
- **The agent itself crashes**: systemd starts it again; the state is in SQLite, so it carries on where it was.

The designs are the `Dongle*` and `DonglePortal*` artboards in the *Pavilion POS Register* canvas.

## How it fits together

```
                    ┌──────────── Raspberry Pi ─────────────────────────────┐
 phone ──Wi-Fi──▶   │ setup page :80 (only on the setup Wi-Fi)              │
                    │       │                                               │
                    │  jester-agent ── SQLite (/var/lib/jester/agent.db)    │
                    │   │  │   └── nmcli (NetworkManager): hotspot, join    │
                    │   │  └── TV pages 127.0.0.1:8080 ◀── Chromium (Cage)  │──HDMI──▶ TV
                    │   │          DevTools 127.0.0.1:9222 ──▲ (steering)   │
                    └───┼───────────────────────────────────────────────────┘
                        └── wss://pavilion/ws/dongle, POST /devices/dongles/pair
```

The kiosk browser only ever opens two things: the agent's page (`http://127.0.0.1:8080/`, which shows whatever the
agent's state calls for and updates itself over SSE) and, while showing, the Pavilion screen. The agent moves it
between them through Chromium's DevTools port.

| Package | Does |
| --- | --- |
| `cmd/jester-agent` | flags, wiring, the HTTP servers |
| `internal/agent` | the state machine: setup, pairing, the WebSocket session, Wi-Fi watching and rescue, steering the browser |
| `internal/ui` | the TV's pages and the phone's setup page (Go templates, embedded CSS/JS/fonts) |
| `internal/netmgr` | the network: `NM` (nmcli) on the Pi, `Fake` for laptops |
| `internal/pavilion` | pairing and the WebSocket client ([PROTOCOL.md](PROTOCOL.md)) |
| `internal/browser` | runs/restarts the browser, steers it over the DevTools protocol |
| `internal/store` | SQLite: the config and a log |
| `internal/device` | the dongle's ID, model, TV resolution; turning the picture (wlr-randr) |

## Try it on a laptop

```sh
make run          # or: go run ./cmd/jester-agent -fake-network -db ./dev.db -portal "" -cdp "" -listen 127.0.0.1:8080
```

- The TV: <http://127.0.0.1:8080/>
- The phone: <http://127.0.0.1:8080/portal/> (open it in a narrow window)
- What it's doing: <http://127.0.0.1:8080/state>

The fake network has a few networks in range: any password of 8+ characters works except `wrongpass`, and `Far-Away`
is never found. For Pavilion's address use your dev server, e.g. `http://127.0.0.1:8000`, and get a code from its
Devices page. Without a DevTools port the TV page takes itself to the screen (and can't come back: that needs the
real kiosk). `JESTER_FAKE_IMAGER_WIFI=Juniper-Staff:juniper2026` pretends Raspberry Pi Imager saved that Wi-Fi on
the card (a password of 64 hex digits is a key, as Imager saves it); it only counts on a fresh `dev.db`.

## Put it on a Pi

The simplest way is the ready-made SD card image: flash it and the Pi starts as a dongle. See
[image/README.md](image/README.md) for building and flashing it (GitHub Actions builds it).

By hand, on Raspberry Pi OS Bookworm Lite, 64-bit (Pi 3/4/5 or Zero 2 W). Set the Wi-Fi country with `raspi-config` first.

```sh
make pi                     # bin/jester-agent-linux-arm64 (no cgo needed)
scp -r bin deploy pi@dongle.local:jester/
ssh pi@dongle.local 'cd jester && sudo deploy/install.sh bin/jester-agent-linux-arm64'
```

`install.sh` installs NetworkManager, Cage, Chromium and wlr-randr, makes a `kiosk` user, and installs two systemd
units: `jester-agent` (root: it drives nmcli, serves port 80 and can reboot) and `jester-kiosk` (Cage + Chromium on
tty1 as `kiosk`). It also makes every DNS name on the setup Wi-Fi point at the dongle
(`deploy/captive-dnsmasq.conf`), which is what makes phones pop the setup page open.

Logs: `journalctl -u jester-agent -u jester-kiosk -f`. State: `curl 127.0.0.1:8080/state` on the Pi.

Cage starts in landscape every time, so the agent turns the picture to Pavilion's orientation (wlr-randr) for every
kiosk that starts: after boot, after a crash, after it restarted a hung browser. That takes Cage 0.1.5 or newer
(`cage -v`): Raspberry Pi OS has one, Debian Bookworm's own package is 0.1.4 and can't. `install.sh` warns about it
and the agent's log says so (`[display] Couldn't turn the picture: …`).

### When a dongle is slow

A Pi Zero 2 W has 416 MB for everything, and Chromium alone wants about that much for one page, so it lives on
compressed swap (zram). `curl -s 127.0.0.1:8080/state` on the Pi says how it's coping:

- `system`: memory left (`memory_available_mb`), how much is swapped out, `memory_pressure` (the percent of the last
  10 seconds everything stood still waiting for memory: over 10 and the Pi is starved, not slow), the load and the
  temperature (at 80 °C a Pi slows itself down).
- `browser`: Chromium's version and whether the graphics chip puts its pages on the TV (`gpu_compositing`:
  `enabled`) or the processor does (`disabled_software`). The agent's log says the same each time the browser
  starts.

`/etc/default/jester-kiosk` adds Chromium flags without building anything (`JESTER_CHROMIUM_FLAGS`, then
`sudo systemctl restart jester-kiosk`): for instance `--disable-gpu`, to compare the processor drawing with the
graphics chip. The TV's own pages have no animation that runs on and on: while one does, Chromium redraws the whole
TV 60 times a second, which a Pi Zero can't spare.

### Flags (or environment)

| Flag | Env | Default | |
| --- | --- | --- | --- |
| `-db` | `JESTER_DB` | `/var/lib/jester/agent.db` | the state |
| `-listen` | `JESTER_LISTEN` | `127.0.0.1:8080` | the TV's pages |
| `-portal` | `JESTER_PORTAL` | `:80` | the setup page (answers only phones on the setup Wi-Fi, only during setup) |
| `-wifi`, `-ethernet` | `JESTER_WIFI`, `JESTER_ETHERNET` | `wlan0`, `eth0` | interfaces (a cable can be picked instead of Wi-Fi) |
| `-cdp` | `JESTER_CDP` | `127.0.0.1:9222` | the browser's DevTools port |
| `-browser` | `JESTER_BROWSER` | — | a browser command line for the agent to run itself (instead of a systemd unit) |
| `-browser-restart` | `JESTER_BROWSER_RESTART` | — | restarts a browser something else runs |
| `-reboot` | `JESTER_REBOOT` | `systemctl reboot` | Pavilion's *Restart* |
| `-rotate` | `JESTER_ROTATE` | on | turn the picture with wlr-randr |
| `-id` | `JESTER_ID` | from the Pi's serial | the dongle's short ID |
| `-fake-network` | `JESTER_FAKE_NETWORK=1` | off | pretend (laptops) |
| `-dev-portal` | `JESTER_DEV_PORTAL=1` | with `-fake-network` | the setup page at `/portal/` on the TV's address too |

## Notes and limits (first draft)

- **One radio**: a Pi's Wi-Fi can't be an access point and join a network at once, so the phone's network list comes
  from a scan made just before the setup Wi-Fi comes on (*Scan* tries again, but most Pi radios see nothing new
  while they're an access point), and the phone drops off when the dongle goes to join. That's why the phone's last
  page says *Watch the TV*.
- The Wi-Fi password goes to `nmcli` on its command line (visible to `ps` on the Pi for a moment) and sits in the
  SQLite file (root-only, 0600) so the setup page can offer it again after a failure.
- Only WPA2/WPA3-Personal and open networks. No enterprise (802.1X) Wi-Fi or captive-portal guest Wi-Fi.
- Software updates over the WebSocket aren't there yet (`version` is reported, so Pavilion can show it).
- Fonts: Bricolage Grotesque and Manrope (SIL Open Font License 1.1) are embedded, since neither the TV nor the
  phone can reach Google Fonts during setup.

## Tests

```sh
make test
```

`internal/agent` runs the whole thing against a fake Pavilion (pairing endpoint and WebSocket): a wrong password, a
code Pavilion refuses, pairing, being told to show a screen, a command, being unpaired, a restart while paired, and
losing the Wi-Fi.
