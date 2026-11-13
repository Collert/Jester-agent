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

- **The browser crashes**: a page that crashes is loaded again (cog), and a browser that exits is started again
  (systemd) on the agent's page, which sends it back to the screen. The agent restarts one that doesn't come when
  called.
- **The screen's address doesn't answer** (Pavilion down when the TV turns on): the TV stays on the agent's page,
  saying so, and tries again every 30 seconds. Once a screen is up it keeps itself current and rides out Pavilion
  restarts on its own.
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
                    │   │  └── TV pages 127.0.0.1:8080 ◀── cog (WPE WebKit) │──HDMI──▶ TV
                    │   │          cogctl (D-Bus): "back to the TV pages" ─▲ │
                    └───┼───────────────────────────────────────────────────┘
                        └── wss://pavilion/ws/dongle, POST /devices/dongles/pair
```

The kiosk browser is [cog](https://github.com/Igalia/cog), a small browser around WPE WebKit that draws straight
onto the display, with no window system under it. It only ever opens two things: the agent's page
(`http://127.0.0.1:8080/`, which shows whatever the agent's state calls for and updates itself over SSE) and, while
showing, the Pavilion screen. Moving between them is shared out:

- **To the screen, the page goes by itself.** While the browser should be on the screen the agent's page carries
  its address (`data-go`), checks that the screen's server answers, and goes. A browser that starts, starts on the
  agent's page, so whatever happens to it (a crash, a restart, a reboot) ends on the screen again.
- **Back to its page, the agent calls it** (`cogctl open`, through `deploy/jester-cogctl`): when the screen is taken
  away or changed, for the dongle's name (*identify*), for setup. The agent knows where the browser is by whether
  its page is open: the page holds a connection to it (`/events`) for as long as it's showing. When calling doesn't
  work or brings nothing within a minute, the agent restarts the kiosk instead, which ends on its page too.

Chromium, which the dongle ran before, wanted about as much memory for one page as a Pi Zero 2 W has (416 MB), so
it lived in swap: a screen took a minute or two to open. In the same test here (same pages, software drawing) cog
took about half of what Chromium did. The agent can still steer a Chromium through its DevTools port (`-cdp`),
which the tests and a laptop can use.

| Package | Does |
| --- | --- |
| `cmd/jester-agent` | flags, wiring, the HTTP servers |
| `internal/agent` | the state machine: setup, pairing, the WebSocket session, Wi-Fi watching and rescue, steering the browser |
| `internal/ui` | the TV's pages and the phone's setup page (Go templates, embedded CSS/JS/fonts) |
| `internal/netmgr` | the network: `NM` (nmcli) on the Pi, `Fake` for laptops |
| `internal/pavilion` | pairing and the WebSocket client ([PROTOCOL.md](PROTOCOL.md)) |
| `internal/browser` | restarts the browser, calls it to a page (cogctl), or steers a Chromium over the DevTools protocol |
| `internal/store` | SQLite: the config and a log |
| `internal/device` | the dongle's ID, model, TV resolution, how the Pi is coping; turning a Chromium kiosk's picture (wlr-randr) |

## Try it on a laptop

```sh
make run          # or: go run ./cmd/jester-agent -fake-network -db ./dev.db -portal "" -cdp "" -listen 127.0.0.1:8080
```

- The TV: <http://127.0.0.1:8080/>
- The phone: <http://127.0.0.1:8080/portal/> (open it in a narrow window)
- What it's doing: <http://127.0.0.1:8080/state>

The fake network has a few networks in range: any password of 8+ characters works except `wrongpass`, and `Far-Away`
is never found. For Pavilion's address use your dev server, e.g. `http://127.0.0.1:8000`, and get a code from its
Devices page. In your own browser the TV page takes itself to the screen as it does in the kiosk, but nothing calls
it back (that's cogctl, on the dongle). `JESTER_FAKE_IMAGER_WIFI=Juniper-Staff:juniper2026` pretends Raspberry Pi Imager saved that Wi-Fi on
the card (a password of 64 hex digits is a key, as Imager saves it); it only counts on a fresh `dev.db`.

## Put it on a Pi

The simplest way is the ready-made SD card image: flash it and the Pi starts as a dongle. See
[image/README.md](image/README.md) for building and flashing it (GitHub Actions builds it).

By hand, on Raspberry Pi OS Trixie Lite, 64-bit (Pi 3/4/5 or Zero 2 W). Set the Wi-Fi country with `raspi-config` first.

```sh
make pi                     # bin/jester-agent-linux-arm64 (no cgo needed)
scp -r bin deploy pi@dongle.local:jester/
ssh pi@dongle.local 'cd jester && sudo deploy/install.sh bin/jester-agent-linux-arm64'
```

`install.sh` installs NetworkManager and cog, makes a `kiosk` user, and installs two systemd units: `jester-agent`
(root: it drives nmcli, serves port 80 and can reboot) and `jester-kiosk` (cog on tty1 as `kiosk`, started by
`deploy/jester-kiosk`). It also makes every DNS name on the setup Wi-Fi point at the dongle
(`deploy/captive-dnsmasq.conf`), which is what makes phones pop the setup page open.

Trixie, for its browser: its cog (0.18) comes with WebKit 2.48. Bookworm's (cog 0.16, WebKit 2.38, from early 2023
and not updated since) runs the same way with the same launcher, but screens and other web pages written for
today's browsers will go wrong on it sooner. Debian doesn't update either WebKit for security problems once the
release is out, which is a reason to keep a dongle on pages you trust.

Logs: `journalctl -u jester-agent -u jester-kiosk -f`. State: `curl 127.0.0.1:8080/state` on the Pi.

cog is turned when it starts: the launcher asks the agent which way up Pavilion wants the picture
(`/kiosk/rotation`), so it's right after a boot or a crash, and a new orientation from Pavilion restarts the kiosk
once. A turned picture goes through cog's OpenGL renderer; landscape goes to the display as it is, which is cheaper.

`/etc/default/jester-kiosk` has the kiosk's settings (then `sudo systemctl restart jester-kiosk`):
`JESTER_KIOSK_MODE` for a smaller picture than the TV's own (`1280x720` is under half the pixels of 1080p to draw),
and `JESTER_COG_FLAGS` for anything `cog --help-all` lists, such as `--web-mem-limit`.

What isn't there with cog, and was with Chromium: the agent can't see a screen that stopped working after it opened
(it sees its own page, not the screen), and it can't tell a hung browser from one that's on a screen. And sound:
music may play (WebKit plays it through GStreamer, installed for that), but Pavilion's live voice channels decode
their audio with WebCodecs, which this WebKit doesn't have. Neither has been tried on a dongle yet.

### When a dongle is slow

A Pi Zero 2 W has 416 MB for everything; what doesn't fit goes to compressed swap (zram).
`curl -s 127.0.0.1:8080/state` on the Pi says how it's coping:

- `system`: memory left (`memory_available_mb`), how much is swapped out, `memory_pressure` (the percent of the last
  10 seconds everything stood still waiting for memory: over 10 and the Pi is starved, not slow), the load and the
  temperature (at 80 °C a Pi slows itself down).
- `on_dongle_page`: whether the browser is showing the agent's page right now (else a screen, or it isn't running).
- `browser`: with a Chromium (`-cdp`), its version and whether the graphics chip draws its pages. Empty with cog.

The TV's own pages have no animation that runs on and on: while one does, a browser redraws the TV 60 times a
second, which a Pi Zero can't spare. A screen that's slow once it's open is worth looking at for the same thing.

### Flags (or environment)

| Flag | Env | Default | |
| --- | --- | --- | --- |
| `-db` | `JESTER_DB` | `/var/lib/jester/agent.db` | the state |
| `-listen` | `JESTER_LISTEN` | `127.0.0.1:8080` | the TV's pages |
| `-portal` | `JESTER_PORTAL` | `:80` | the setup page (answers only phones on the setup Wi-Fi, only during setup) |
| `-wifi`, `-ethernet` | `JESTER_WIFI`, `JESTER_ETHERNET` | `wlan0`, `eth0` | interfaces (a cable can be picked instead of Wi-Fi) |
| `-browser-open` | `JESTER_BROWSER_OPEN` | — | opens an address in the kiosk browser, added as the last word (`jester-cogctl open`) |
| `-browser-reload` | `JESTER_BROWSER_RELOAD` | — | reloads its page (`jester-cogctl reload`) |
| `-cdp` | `JESTER_CDP` | — | a Chromium's DevTools port (e.g. `127.0.0.1:9222`), to steer that instead of cog |
| `-browser` | `JESTER_BROWSER` | — | a browser command line for the agent to run itself (instead of a systemd unit) |
| `-browser-restart` | `JESTER_BROWSER_RESTART` | — | restarts a browser something else runs |
| `-reboot` | `JESTER_REBOOT` | `systemctl reboot` | Pavilion's *Restart* |
| `-rotate` | `JESTER_ROTATE` | on | turn the picture as Pavilion says (cog: when it starts; a Chromium in Cage: with wlr-randr) |
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
