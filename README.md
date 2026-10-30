# mosh-webrtc

A terminal in your browser that survives losing your network.

`mosh-webrtc` runs a [Mosh](https://mosh.org) server **in-process** and carries its State
Synchronization Protocol over a WebRTC DataChannel instead of UDP. The browser side is the
`mosh-go` client compiled to WebAssembly, rendering into xterm.js.

- **Roaming**: switch Wi-Fi ↔ cellular, or sleep the laptop — the shell keeps running. The client
  re-attaches a fresh DataChannel to the same session and the server repaints from scratch.
- **Typing that survives lag**: local echo prediction with server confirmation, like Mosh.
- **No UDP anywhere**: the browser talks to a TURN relay (Cloudflare) and the gateway talks only to
  local `pty`. Nothing needs a port forward.
- **Real login**: the PTY runs `/usr/bin/login`, so you authenticate with your own account.

## Architecture

```
browser (xterm.js + mosh-go/wasm)  ── WebRTC DataChannel ──►  mosh-webrtc gateway
        │  signaling only over HTTPS                                   │
        └────────────── /api/offer, /api/config ──────────────────────┤
                                                                      ▼
                                                         mosh-go server (in-process)
                                                                      │
                                                              /usr/bin/login
                                                                      │
                                                                   your shell
```

The gateway never shells out to `mosh-server` and never binds a relay socket: `mosh-go.Server`
exchanges SSP datagrams through channels fed by the DataChannel. Mosh's OCB-AES encryption,
diffing and prediction are unchanged — only the wire got replaced.

Deep dive: [`IMPLEMENTATION.md`](IMPLEMENTATION.md) (protocol details, bug post-mortems and the
security model).

## Requirements

- Go 1.24+ (for the WASM client and the gateway)
- A TURN server for anything that is not on the same LAN. The default configuration expects
  Cloudflare Realtime TURN credentials; plain STUN also works for local use.
- macOS for the `/usr/bin/login` flow as shipped (Linux works with `MOSH_LOGIN` set to your own
  login program).

## Build

```sh
go build -o mosh-webrtc-server main.go cfaccess.go

GOOS=js GOARCH=wasm go build -o web/static/mosh.wasm ./cmd/mosh-rtc-wasm
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" web/static/wasm_exec.js  # or misc/wasm on older Go
```

Then open <http://127.0.0.1:8090> and press **Connect**.

## Configuration

All settings are environment variables (put them in a `.env` file next to `run.sh`; it is sourced
on start).

| Variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8090` | Listen port |
| `HOST` | `127.0.0.1` | Listen address |
| `APP_ENV` | `development` | `production` enforces Cloudflare Access JWTs |
| `MOSH_LOGIN` | `/usr/bin/login` | Program started in the PTY |
| `MAX_SESSIONS` | `8` | Concurrent Mosh sessions before new ones are refused |
| `RATE_LIMIT_PER_MIN` | `10` | New-session requests per IP per minute (`0` disables). Reconnects are exempt |
| `CF_TURN_KEY_ID` | – | Cloudflare Realtime TURN key id |
| `CF_TURN_KEY_API_TOKEN` | – | Cloudflare Realtime API token, used to mint short-lived TURN credentials |
| `CF_TEAM_DOMAIN` | – | `<team>.cloudflareaccess.com`, required in production |
| `CF_ACCESS_AUD` | – | Cloudflare Access application audience tag, required in production |
| `GATEWAY_TOKEN` | – | Optional static bearer token accepted instead of an Access JWT |

TURN credentials are minted server-side with a 2h TTL and cached for an hour, so the browser never
sees your Cloudflare API token.

## Security

- **Production mode** (`APP_ENV=production`) rejects every request without a valid
  `Cf-Access-Jwt-Assertion` (signature, audience, issuer and expiry verified against the team's
  JWKS) or the static `GATEWAY_TOKEN`.
- Media does **not** flow through Cloudflare Access — it goes browser ↔ TURN ↔ gateway. Protect the
  gateway with the JWT check above and keep session ids secret; they are the only handle to a shell.
- Session ids are 128-bit random hex, never timestamps.
- Five failed passwords kill the login process; sessions stuck at the login prompt for 10 minutes
  are reaped, as are sessions disconnected for 1 hour or idle for 24 hours.

## HTTP API

| Endpoint | Description |
| --- | --- |
| `GET /` | Web client |
| `GET /api/config` | ICE servers (`iceServers`) |
| `POST /api/offer` | SDP offer; returns `sessionId`, `key`, `sdp`. Pass `sessionId` to re-attach |
| `GET /api/session/{id}` | `204` if the session is alive, `404` if it ended |

## Running as a service (macOS)

[`run.sh`](run.sh) sources `.env` and execs the gateway. A LaunchAgent keeps it alive:

```xml
<!-- ~/Library/LaunchAgents/com.user.mosh-webrtc.plist -->
<key>ProgramArguments</key>
<array>
  <string>/bin/bash</string>
  <string>/path/to/mosh-webrtc/run.sh</string>
</array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
```

```sh
launchctl load ~/Library/LaunchAgents/com.user.mosh-webrtc.plist
launchctl kickstart -k gui/$(id -u)/com.user.mosh-webrtc   # restart after a rebuild
```

Expose it with a Cloudflare Tunnel instead of opening a port:

```sh
cloudflared tunnel --url http://127.0.0.1:8090
```

Put Cloudflare Access in front of that hostname and set `APP_ENV=production`.

## Controls

- Font size presets: 8x16 (default), 16x32, 32x64 ([Spleen](https://github.com/fcambus/spleen)).
- Mouse reporting, scroll wheel, bracketed paste and Unicode 11 widths are supported.
- **Retry** repaints and re-negotiates when the link comes back.

## Tests

```sh
go test ./...
```

Covers the transport (fragmentation, OCB, SSP acks), state synchronization and resync, login
state detection, JWT middleware, rate limiting and session limits. The optional interop test
against the C++ `mosh-server` is skipped when it is not installed.

## Credits

The Mosh protocol implementation is [`mosh-go`](https://github.com/unixshells/mosh-go), vendored
under `mosh-go/` and MIT licensed (Copyright (c) 2026 Unix Shells) — its client is what runs in the
browser as WASM, and its server is what runs in the gateway. The wire format, AES-128-OCB3
encryption, diffing and predictive echo all come from there; this project adds the WebRTC transport,
session roaming, access control and the UI around it.

Terminal rendering uses [xterm.js](https://xtermjs.org) with the
[Spleen](https://github.com/fcambus/spleen) bitmap fonts.
