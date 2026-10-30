# Mosh WebRTC Implementation Spec & Architecture Document

This document records the exact mechanics, protocol flows, data structures, bug fixes, edge-case solutions, and security architectures implemented in **`mosh-webrtc`**.

---

## 1. High-Level Architecture

Traditional Mosh connects client and server via direct UDP sockets. In browser environments, raw UDP sockets are impossible. Rather than using slow, Head-of-Line-blocked WebSockets, this implementation transports Mosh’s State Synchronization Protocol (SSP) directly over **WebRTC DataChannels**.

```
┌────────────────────────────────────────────────────────────────────────┐
│ BROWSER (Client)                                                       │
│                                                                        │
│  [xterm.js + Unicode 11 addon]                                         │
│         ▲                    │                                         │
│   ANSI  │              Input │ (Keystrokes, SGR Mouse)                 │
│         │                    ▼                                         │
│  [WASM State Tracker]  ───▶  [WASM Mosh Client (mosh-go)]              │
│  - Shadow vt-go emulator     - OCB-AES-128 crypto                      │
│  - Speculative prediction    - SSP diff engine                         │
│  - Exact-state diff ACK      - Heartbeat / Retransmit                  │
│         ▲                    │                                         │
│         │ Decrypted SSP      │ Encrypted SSP Datagrams                 │
│         └────────────────────┼─────────────────┐                       │
│                              ▼                 │                       │
│                     [RTCDataChannel "mosh"]    │                       │
│                     (unordered, maxRetrans=2)  │                       │
└───────────────────────────────┬────────────────┼───────────────────────┘
                                │ DTLS / SCTP    │
                                ▼                │
                    [Cloudflare TURN Relay]      │
                    (turn.cloudflare.com:3478)   │
                                │                │
┌───────────────────────────────┼────────────────┼───────────────────────┐
│ HOST GATEWAY (macOS Daemon)   ▼                │                       │
│                                                │                       │
│  [Pion WebRTC PeerConnection] ─────────────────┘                       │
│  - In-process DataChannel termination                                  │
│         │                                                              │
│         │ Inbound/Outbound byte slices (no UDP sockets)                │
│         ▼                                                              │
│  [In-Process Mosh Server (mosh-go)]                                    │
│  - Server-side vt-go virtual terminal                                  │
│  - DEC mouse mode interceptor (modes 1000, 1006, etc.)                 │
│  - Full frame resync trigger (on reconnect & resize)                   │
│  - Auth failure counter (terminates on 5 failed attempts)              │
│  - Drain pipe writer (prevents terminal query deadlocks)               │
│         │                                                              │
│         │ Master PTY (/dev/ptmx)                                       │
│         ▼                                                              │
│  [/usr/bin/login -p] ──▶ [/bin/zsh (or user default shell)]            │
└────────────────────────────────────────────────────────────────────────┘
```

---

## 2. In-Process Mosh Server (Zero UDP Sockets)

### The Problem
Initial prototypes launched `mosh-server new -p 0` as a subprocess and bound local loopback UDP sockets to relay packets to the WebRTC DataChannel. This caused:
- Port accumulation (hundreds of leaked UDP ports on flappy connections).
- Process orphan risk if the Go gateway died while `mosh-server` remained running.
- Unnecessary kernel context switches and socket buffer overhead.

### The Solution: `NewServerIO`
`mosh-go/server.go` was refactored to eliminate all OS UDP socket calls (`net.ListenUDP`, `net.DialUDP`):

1. **Channels Instead of Sockets**:
   ```go
   type Server struct {
       inbound  <-chan []byte
       sendOut  func([]byte)
       resync   chan struct{}
       // ...
   }
   ```
2. **Channel-Driven Datagram Loop**:
   - In `recvUDP()`, if `s.inbound != nil`, it reads raw bytes from `<-s.inbound` directly into the Mosh crypto engine.
   - In `sendDatagrams()`, if `s.sendOut != nil`, each encrypted packet is handed directly to a closure that puts it into `s.DataChannel.Send()`.
3. **PTY Lifecycle Management**:
   The server launches `/usr/bin/login -p` via `pty.StartWithAttrs()`, hooking directly to macOS system authentication.

---

## 3. WebRTC DataChannel Tuning for Terminal Traffic

### SCTP Profile
Terminal traffic has unique constraints:
- **Zero Head-of-Line Blocking**: Never block new terminal screen updates behind old dropped screen updates.
- **Tolerant to Small Packet Drops**: Mosh SSP diffs are cumulative; a lost state diff is automatically corrected by the next incoming diff.
- **Low Latency on Weak Links**: Pure `maxRetransmits: 0` causes too many missed keystrokes on dirty cellular links, while reliable/ordered causes stutter.

### Configuration
```javascript
const dc = pc.createDataChannel("mosh", {
    ordered: false,      // Out-of-order delivery allowed (SSP sequences them)
    maxRetransmits: 2    // Allow up to 2 sub-RTT retransmissions before giving up
});
dc.binaryType = "arraybuffer";
```

---

## 4. Solving Terminal Blanking & Desynchronization Bugs

### Bug 1: The `vt-go` Query Pipe Deadlock (Herdr / TUI Blank Screen)
- **Symptom**: `herdr`, `less`, or apps querying terminal status (DECRQM, DA, OSC 4 palette) caused the terminal to permanently freeze blank on startup.
- **Root Cause**: Modern TUI apps emit queries like `\x1b[?1016$p` and 256 OSC palette queries. The Go emulator (`vt-go`) writes replies into an unbuffered pipe (`io.PipeWriter`). Because nothing was reading from the pipe reader, `s.emu.Write(data)` blocked forever on the first query.
- **The Fix** (`mosh-go/server.go`):
  ```go
  func (s *Server) drainReplies(w io.Writer) {
      buf := make([]byte, 1024)
      for {
          n, err := s.emu.Read(buf)
          if err != nil { return }
          if n > 0 && w != nil {
              _, _ = w.Write(buf[:n]) // Forward terminal queries straight back to PTY master!
          }
      }
  }
  ```

### Bug 2: The Stale-Base Mismatched Diff Corruption
- **Symptom**: On throttled/lossy connections, text drifted 1 column left, border box-drawing lines collapsed, and characters corrupted (`histofy`, `ephemerzl`).
- **Root Cause**:
  1. The client received a diff calculated against state `OldNum=4`, but the client dropped state 4 and only had state 3.
  2. The old fallback code attempted to diff against `latestState` instead of dropping it, corrupting the shadow emulator.
  3. Mosh transport auto-ACKed packet sequence numbers even if the state wasn't applied, so the server kept advancing its base.
- **The Fix**:
  - **Confirmation-Driven ACKs**: Added `SetCanApply(func(oldNum) bool)` to `mosh-go/transport.go`. ACKs are only sent if the client strictly holds `OldNum` in memory.
  - **Clean Diff Discarding**: If `OldNum` is missing, the WASM client drops the diff and refuses to ACK, forcing the server to re-transmit a diff calculated from the last confirmed state.

### Bug 3: Reconnect Frozen Terminal (SCTP Open Race)
- **Symptom**: Reconnecting to an existing session showed "Connected", but the screen remained frozen until someone resized the window or typed.
- **Root Cause**: On reconnect, the server called `TriggerResync()` before DTLS/SCTP handshaking finished. The full-screen redraw packet hit a closed DataChannel and vanished. The server thought it already refreshed the screen, leaving the client stuck.
- **The Fix** (`main.go`):
  ```go
  dc.OnOpen(func() {
      // Trigger full frame redrawing ONLY after SCTP channel is verified open!
      sess.Server.TriggerResync()
  })
  ```

---

## 5. Mouse Event & SGR Mode Forwarding

### The Challenge
When running `herdr` or `tmux`, mouse scrolling and clicking are essential. However:
1. `mosh` framebuffers only synchronize rendered character cells; raw escape codes are stripped.
2. `xterm.js` does not enable mouse tracking unless it receives DEC private modes like `\x1b[?1000h` and `\x1b[?1006h`.

### The Dual-End Interceptor Fix
1. **Server-Side Mode Sniffer** (`mosh-go/server.go`):
   Intercepts raw PTY output before it enters `vt-go` using regex `\x1b\[\?([0-9;]+)([hl])`. Whitelists mouse modes (`9, 1000, 1002, 1003, 1005, 1006, 1015, 1016`) and prepends them to outgoing diffs.
2. **Client-Side Escape Injection** (`cmd/mosh-rtc-wasm/state.go`):
   When unpacking the diff, strips mode escapes and pushes them directly to `xterm.js` via `st.output`, activating the mouse engine (`areMouseEventsActive = true`).
3. **Double Click Prevention**:
   Disabled duplicate `term.onBinary` handlers in JavaScript; modern `xterm.js` emits SGR mouse reports through `term.onData`.
4. **Context Menu Suppression**:
   Attached `e.preventDefault()` to `#terminal-container` `contextmenu` events so right-clicking inside the terminal interacts with terminal apps rather than opening browser context menus.

---

## 6. Unicode 11 & Grapheme Alignment

### The Problem
Rendering emojis (`❤️`, `👍`) or East Asian wide characters (`中`) caused horizontal shift across rows.

### The Fix
1. **Engine Synchronization**:
   - `xterm.js` defaulted to Unicode version 6 (emojis width = 1).
   - Added `xterm-addon-unicode11@0.6.0` and forced `term.unicode.activeVersion = '11'`.
2. **Multi-Rune Grapheme Storage**:
   Refactored `mosh-go/framebuffer.go` `Cell.Rune rune` to `Cell.Text string` so emojis with variation selectors (`U+FE0F`) or ZWJ sequences don't truncate to single bytes.
3. **Cursor Position (CUP) Force-Resync**:
   In `Diff()` and `fullRedraw()`, whenever a cell with `Width != 1` is rendered, an explicit cursor positioning escape sequence (`\x1b[row;colH`) is injected to force browser-emulator alignment.

---

## 7. Fast Stall Detection & Seamless Reconnection

Traditional WebRTC applications rely on `RTCPeerConnection.oniceconnectionstatechange` to detect disconnects. In reality:
- When a phone switches from Wi-Fi to 5G, the old TURN allocation dies quietly.
- Chrome's ICE layer can sit in `iceConnectionState: "connected"` for **30+ seconds** before timing out.

### The SSP Traffic Watchdog
Instead of trusting WebRTC's ICE state, we monitor Mosh's State Synchronization traffic directly:
```javascript
// Start watchdog ticking every 2 seconds
function startWatchdog() {
    setInterval(() => {
        if (!moshSession || !activeDC || activeDC.readyState !== "open" || isReconnecting) return;
        if (Date.now() - lastWatchdogReconnect < 10000) return; // 10s cooldown

        const down = moshSession.sinceRecvMs();  // ms since last valid decrypted packet
        const up = moshSession.unackedMs();      // ms keystrokes have waited for ACK

        // 6 seconds without server traffic = link is dead!
        if (down > 6000 || up > 6000) {
            lastWatchdogReconnect = Date.now();
            triggerReconnect("Link stalled");
        }
    }, 2000);
}
```

### Hot-Swapping the DataChannel in WASM
When reconnecting:
1. Client generates a new `RTCPeerConnection` and DataChannel.
2. Posts SDP to `/api/offer` with `{ sessionId: currentSessionId }`.
3. Gateway binds the new DataChannel to the existing in-memory `Session` and running shell process.
4. Client calls `moshSession.swapDataChannel(newDC)`.
5. The WASM state machine updates its internal transport pointers and forces a resync **without restarting or resetting the shell**.

---

## 8. Hardened Security & Gateway Defense

When running over Cloudflare Tunnels and the public internet, multiple security controls are enforced:

### 1. Cloudflare Access JWT Validation (`APP_ENV=production`)
- Verifies the cryptographic signature of `Cf-Access-Jwt-Assertion` using Cloudflare's public keys (`https://<team>.cloudflareaccess.com/cdn-cgi/access/certs`).
- Validates RS256 algorithm, Audience (`CF_ACCESS_AUD`), Issuer, and Expiration.
- Caches RSA public keys for 1 hour with on-demand refresh.
- Alternative static Bearer token support via `GATEWAY_TOKEN` using `subtle.ConstantTimeCompare`.

### 2. Cryptographic 128-bit Session IDs
- Replaced timestamp session IDs (`s-<UnixNano>`) with `crypto/rand` 16-byte random hex tokens (e.g. `56df0c17b3f8ac185f1a95c475be65c1`).
- Protects against session hijacking and session enumeration on `/api/session/`.

### 3. Rate Limiting Tuned for Lossy Reconnects
- Per-IP rate limiting (`RATE_LIMIT_PER_MIN`, default `10 req/min/IP`).
- Resolves real client IP from `CF-Connecting-IP` header.
- **Critical Exemption**: Valid reconnects presenting an existing session ID bypass the rate limiter, ensuring flaky mobile connections are never locked out.
- **Orphan Prevention**: If an SDP offer fails parsing or validation (`SetRemoteDescription`), the gateway terminates the pre-allocated PTY session immediately.

### 4. Brute-Force & Session Reapers
- **Login Brute-Force Killing**: Tracks `Login incorrect` occurrences in PTY output. On the 5th failed attempt, immediately executes `kill` on the process group.
- **Unauthenticated Login Timeout**: Sessions sitting unauthenticated at the `login:` prompt for > 10 minutes are reaped.
- **Disconnected Session Reaper**: Runs every 1 minute. Prunes sessions disconnected for > 1 hour or idle for > 24 hours.
- **Session Cap**: Enforces `MAX_SESSIONS` (default 8, 503 Service Unavailable when exceeded).

---

## 9. UI & Rendering Polish

- **Spleen Bitmap Fonts**: Pre-converted and packaged `Spleen 8x16`, `16x32`, and `32x64` fonts with responsive preset selector, persistent in `localStorage`.
- **WASM Loading Progress**: Streams WASM download with real-time progress indicators: `Loading Mosh WASM (1.2 MB/5.36 MB)` via custom `X-File-Size` response headers (bypassing Cloudflare gzip chunking issues).
- **Mobile Responsive Layout**: Single-column flex layout on screens `< 600px` with fixed badge circularity (`flex: 0 0 8px`) to prevent oval distortion.

---

## 10. Key File Manifest

| File | Purpose |
|------|---------|
| `main.go` | WebRTC signaling gateway, session registry, reaper, rate limiter, and HTTP/API routing. |
| `cfaccess.go` | Cloudflare Access JWT validator with JWKS key caching and fallback token check. |
| `mosh-go/server.go` | In-process Mosh server engine, PTY master, mouse mode sniffer, and frame resync. |
| `mosh-go/transport.go` | SSP protocol state machine, confirmed-diff ACK logic, and stall timers. |
| `mosh-go/framebuffer.go`| Virtual terminal diffing, Unicode 11 grapheme storage, and CUP cursor correction. |
| `cmd/mosh-rtc-wasm/main.go` | Go WASM bridge exposing Mosh client and transport controls to browser JS. |
| `cmd/mosh-rtc-wasm/state.go`| WASM shadow terminal state tracker, prediction diffing, and DEC mode forwarding. |
| `web/static/index.html` | Client UI, xterm.js setup, WebRTC negotiation, watchdog, and font selector. |
| `run.sh` | LaunchAgent wrapper script sourcing `.env` and configuring production environment. |

---

## 11. Why Sessions Died a Few Minutes After Login (`404 Session Ended`)

### Symptom
A few minutes into a working session, the browser reported `"Shell session ended"` and reset to the connect screen, even though the shell had been authenticated and in active use. The server log showed:

```
Session <id> ended: shell exited
mosh server exited: signal: killed
```

### The SIGHUP False Lead (Retracted)
An initial hypothesis blamed SIGHUP from a network switch. **Wrong**, and the log proves it:

- `signal: killed` is **SIGKILL (9)**, not `signal: hangup` (SIGHUP = 1).
- A network switch never touches the PTY: reconnect only swaps `sess.DataChannel` / `sess.PeerConn` (`main.go:381-389`); `s.ptmx` and the login process are untouched, because the gateway process — not the WebRTC socket — holds the PTY master.
- Nothing in the exec path can send SIGKILL: no `exec.CommandContext` and no cancelled context anywhere in `mosh-go`.

There are exactly **three** `Process.Kill()` sites in the codebase:

| Site | Logged? | Reachable in production? |
|---|---|---|
| `mosh-go/server.go` `Close()` | no | no (tests only) |
| `mosh-go/server.go` `mainLoop` auth limit | no (now yes) | **yes — the culprit** |
| `mosh-go/server.go` `mainLoopRW` auth limit | no (now yes) | yes |

### The Actual Mechanism
`countAuthFailures(data []byte)` scanned **all** raw PTY output for the literal `"Login incorrect"` and killed the shell process at 5 hits, regardless of authentication state.

That literal was ordinary text in this very repo:

```
mosh-go/server.go:        4 occurrences
mosh-go/authfail_test.go: 4 occurrences
```

Paging, diffing, or running `go test` on those files inside the session streamed ≥5 occurrences to the PTY within seconds, triggering:

```
countAuthFailures -> true -> cmd.Process.Kill() -> SIGKILL
  -> cmd.Wait() returns "signal: killed"
  -> close(s.done)
  -> gateway goroutine <-s.Server.Done() runs endSession(s, "shell exited")
  -> delete(sessions, s.ID)
  -> client's GET /api/session/{id} returns 404
  -> frontend wipes currentSessionId, status "Shell session ended", prompts a new connect
```

---

## 12. macOS `login(1)` Process Tree & Fork Semantics

The first fix attempt gated the counter on `StillAtLogin()`, which read `ps -o comm= -p <pid>`. It failed due to platform-specific semantics:

- **Linux** `login(1)` `execve()`s the shell in place, so the PID's `comm` becomes `zsh`.
- **macOS** `login(1)` `fork()`s the child shell, drops privileges, and the **parent stays alive as `login`**, waiting on the child. The process tree is `login (PID X)` -> `-zsh (PID Y)`.

Consequence: `ps -o comm=` reports `"login"` for the entire authenticated session, so `StillAtLogin()` returned `true` forever and the guard was inert.

### Correct Detection: Inspect the Process Tree
`mosh-go/server.go:StillAtLogin()` now requires **both** conditions:

```go
func (s *Server) StillAtLogin() bool {
	if s.cmd == nil || s.cmd.Process == nil {
		return false
	}
	pid := s.cmd.Process.Pid
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // process gone: session is over anyway
	}
	name := strings.TrimSpace(string(out))
	if name != "login" && !strings.HasSuffix(name, "/login") {
		return false
	}
	kids, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	return err != nil || strings.TrimSpace(string(kids)) == "" // exit 1 = no children = still at prompt
}
```

- At the username/password prompt: process is `login`, **no children** -> `StillAtLogin() == true`, failures count.
- After authentication: `login` has a `-zsh` child -> `StillAtLogin() == false`, counter resets and the literal is ignored forever after.

Two side benefits: `MOSH_LOGIN=/bin/zsh` (dev/scratch instances) is immediately "logged in" because its `comm` is not `login`, and the 10-minute unauthenticated-login reaper (`loginGracePeriod`) uses a signal that macOS actually produces.

### `countAuthFailures` Final Implementation
```go
func (s *Server) countAuthFailures(data []byte) bool {
	s.scanTail = append(s.scanTail, data...)     // 32-byte tail survives chunk splits
	matched := false
	for {
		i := bytes.Index(s.scanTail, []byte("Login incorrect"))
		if i < 0 { break }
		s.scanTail = s.scanTail[i+len("Login incorrect"):]
		matched = true                            // one increment per chunk, not per occurrence
	}
	if len(s.scanTail) > 32 { s.scanTail = s.scanTail[len(s.scanTail)-32:] }
	if !matched { return false }
	if !s.StillAtLogin() { s.authFailures = 0; return false }  // shell text, not a rejection
	s.authFailures++
	return s.authFailures >= maxLoginFailures
}
```

### Tests (`mosh-go/authfail_test.go`)
| Test | Asserts |
|---|---|
| `TestCountAuthFailuresAcrossChunks` | Message split across PTY reads, tripping exactly on attempt 5. |
| `TestCountAuthFailuresIgnoresShellOutput` | `comm=zsh` + 10 hits -> no kill, `authFailures == 0`. |
| `TestCountAuthFailuresIgnoresOutputAfterLoginForksShell` | `comm=login` **with a child** (login's post-auth shape) + 10 hits -> no kill. |

---

## 13. Session Lifetime Map (What a 404 Really Means)

`/api/session/{id}` returns `404` **only** when the ID is absent from the `sessions` map. Everything that deletes:

| Cause | Log Line | Timing |
|---|---|---|
| Shell/login process exit | `ended: shell exited` | Immediate |
| SIGKILL from auth-failure limit | `killing shell: N failed logins` | Immediate |
| Nobody logged in | `ended: login timed out` | 10 min (`loginGracePeriod`) |
| Session cap eviction | `503` on new offer instead | n/a |
| Malformed SDP on a new session | `ended: invalid offer` | Immediate, new session only |
| Disconnected | `Reaping mosh session ...` | > 1 h disconnected |
| Idle | `Reaping mosh session ...` | > 24 h since `LastSeen` |
| Gateway restart | — | All sessions lost (state is in-memory only) |

The frontend treats `404` as "shell is gone, start over" and `204` as "server alive, just a network drop" (`sessionAlive()` in `web/static/index.html`).

---

## 14. Reconnect Path & Rate-Limit Coupling

- `offerRL` is a package-level `*rateLimiter` built once at startup (`RATE_LIMIT_PER_MIN`, default 10/min/IP, `0` disables). `/api/config` keeps the middleware; `/api/offer` checks *inside* `handleOffer` so it can be skipped:

```go
if !isReconnect && offerRL != nil && !offerRL.allow(clientIP(r), time.Now()) { ... 429 }
```
  Reconnects present a 128-bit session ID and are exempt; only new sessions consume budget. Client IP comes from `CF-Connecting-IP`, falling back to `r.RemoteAddr`.

- The client no longer refetches `/api/config` on every reconnect: `getIceServers()` caches for 1 h (server caches credentials 1 h, TURN TTL is 2 h), making reconnect exactly one HTTP request.

- A failed `SetRemoteDescription` on a **new** session tears that session down (`endSession(sess, "invalid offer")`) so a garbage offer cannot leave an orphan `/usr/bin/login` PTY occupying a `MAX_SESSIONS` slot.
