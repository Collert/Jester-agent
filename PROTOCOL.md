# Dongle ↔ Pavilion protocol

Pavilion's side lives in Pavilion's `devices` app (`devices/views.py`, `devices/socket.py`, `devices/pairing.py`).
The agent's side is `internal/pavilion`. Change both together.

## 1. Pairing

An admin opens **Settings › Devices › Add visual dongle** in Pavilion, which makes a six-digit code (works once,
for 15 minutes). The person types it, with Pavilion's address, on their phone during the dongle's setup. Once the
dongle is online it sends:

```
POST https://<pavilion>/devices/dongles/pair
Content-Type: application/json

{
  "code": "482913",
  "hardware_id": "7F3K",                  // the dongle's short ID (from the Pi's serial)
  "model": "Raspberry Pi 4 Model B Rev 1.5",
  "version": "0.1.0",
  "resolution": "1920x1080",              // the TV's preferred mode, "" with no TV
  "network": {"ssid": "Juniper-Staff", "signal": 88, "ip": "192.168.1.42", "wired": false}
}
```

| Answer | Body | The dongle |
| --- | --- | --- |
| 200 | `{"token": "12.<secret>", "dongle": {"id": 12, "name": "Dongle 7F3K"}, "store": {"name": "Juniper Lane"}}` | keeps the token, shows "Paired with Juniper Lane", connects the WebSocket |
| 404 | `{"error": "bad_code", "message": …}` | "Pavilion didn't take the code", setup Wi-Fi back on, code forgotten |
| 410 | `{"error": "expired" \| "used", "message": …}` | same |
| 400 | `{"error": "bad_request", "message": …}` | same |
| 429 | `{"error": "throttled", "retry_after": s, …}` | same (10 tries a minute per address, 60 for everyone) |
| anything else, or no answer | — | "Couldn't reach Pavilion" (wrong address, DNS, TLS, server down) |

The token is `<dongle id>.<secret>`. Pavilion keeps only the secret's SHA-256.

## 2. The WebSocket

```
GET wss://<pavilion>/ws/dongle
Authorization: Bearer 12.<secret>
```

Text frames, one JSON object each. The connection stays open; the dongle reconnects after 1 s, doubling to a minute,
with jitter.

**Close code 4401** (or a 401/403 on the upgrade) means the token isn't valid any more: the dongle was unpaired. The
dongle forgets its token, name and screen (keeping Pavilion's address and the Wi-Fi) and goes back to "Scan to
start". Any other close just reconnects.

### Pavilion → dongle

**config**: on every connection, and whenever the dongle or its screen changes in Pavilion.

```json
{"type": "config", "name": "Front counter TV", "store": "Juniper Lane",
 "screen": {"name": "Window specials", "url": "/devices/dongles/open/<signed>"},
 "orientation": "landscape"}
```

- `screen` is `null` when none is assigned (the TV shows "No screen assigned").
- `url` is a path on the same Pavilion (the agent joins it to the address it paired with) or a full URL. Opening it
  sets a cookie that lets that browser see the screen, then redirects to `/screens/<slug>`. It changes when the
  screen changes, and stops working when the dongle is unpaired or re-paired. The agent opens it whenever it changes.
- `orientation`: `landscape` | `portrait` (turned right, 90°) | `portrait_flipped` (turned left, 270°).

**command**: from the buttons on the Devices page. The dongle answers each with an `ack`.

```json
{"type": "command", "id": "a1b2c3", "command": "identify", "seconds": 10}
```

| command | does |
| --- | --- |
| `identify` | shows the dongle's name full-screen for `seconds` (default 10) |
| `reload` | reloads the page (restarts the browser if it can't) |
| `restart` | acks first, then reboots the Pi |
| `change_wifi` | turns the setup Wi-Fi on so someone can pick a new network; stays paired |

**unpaired**: followed by close 4401.

### Dongle → Pavilion

**status**: on connecting and every 30 s. Pavilion keeps it on `Dongle.info` and counts the dongle online while
these keep coming (90 s).

```json
{"type": "status", "hardware_id": "7F3K", "model": "Raspberry Pi 4 Model B Rev 1.5", "version": "0.1.0",
 "resolution": "1920x1080", "network": {"ssid": "Juniper-Staff", "signal": 88, "ip": "192.168.1.42", "wired": false},
 "showing": "screen", "uptime": 3600, "browser_restarts": 0}
```

`showing`: `screen` | `idle` (no screen assigned) | `identify` | `setup`.

**ack**

```json
{"type": "ack", "id": "a1b2c3", "ok": true}
{"type": "ack", "id": "a1b2c3", "ok": false, "error": "unknown command \"dance\""}
```

Unknown message types are ignored on both sides, so either can add new ones first.
