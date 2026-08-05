# TunnelHW — Architecture (Draft v0.1)

> Status: DRAFT — under review by the external sparring panel (Codex/Grok/Kimi).

## 1. Problem statement

A user has physical hardware (serial devices, dev boards, USB gadgets) attached to
their local machine (Windows / Linux / macOS). An LLM agent running on a **remote**
server must be able to use that hardware **as if it were locally attached** — open
it, read/write, run full workflows against it.

Requirements:

1. **Local agent** ("the agent") runs on the machine with the hardware.
2. **Local web UI** (localhost only) where the human selects which devices are
   exposed for remote control. Nothing is exposed by default.
3. Each exposed device gets a **human-readable unique ID**: two random words
   joined by a hyphen, e.g. `amber-falcon`. This is the handle the LLM uses.
4. The remote LLM connects "into" the local machine — but the local machine is
   almost certainly behind NAT/firewall, so the connection must be
   **dialed outbound from the agent** (reverse tunnel).
5. Cross-platform: Windows, Linux, macOS.

## 2. High-level design

```
┌────────────────────────────┐                ┌─────────────────────────────┐
│  User's machine            │                │  Remote server (LLM host)   │
│                            │   outbound     │                             │
│  ┌──────────┐  ┌────────┐  │   WSS/TLS      │  ┌─────────┐  ┌──────────┐  │
│  │ hardware │──│ agent  │──┼───────────────▶│  │  relay  │──│ MCP srv  │──▶ LLM
│  └──────────┘  └────────┘  │  (multiplexed  │  └─────────┘  └──────────┘  │
│                    │       │   yamux)       │                             │
│              ┌─────────┐   │                └─────────────────────────────┘
│              │ web UI  │   │
│              │localhost│   │
│              └─────────┘   │
└────────────────────────────┘
```

Three components, one repo (monorepo):

| Component | Runs on | Role |
|---|---|---|
| `agent`   | user's machine | enumerates hardware, serves the localhost web UI, dials the relay, bridges device I/O |
| `relay`   | remote server  | accepts agent connections, tracks online devices, exposes an API for consumers |
| `mcp`     | remote server  | MCP server giving the LLM tools (`list_devices`, `open`, `read`, `write`, …) backed by the relay |

`relay` and `mcp` may live in the same process initially.

## 3. Answer to "can this be done in a (reverse) SSH way?"

Yes — the *shape* is exactly a reverse tunnel (`ssh -R`), and that would work as a
prototype. But plain OpenSSH has real drawbacks for this product:

- One TCP port per forwarded device; port allocation bookkeeping on the server.
- Requires managing an SSH server + accounts/keys on the LLM host.
- Serial devices aren't TCP — you'd need a ser2net/RFC2217 bridge per device anyway.
- No clean place to put device metadata, presence, or the word-ID handshake.

**Decision:** use the reverse-tunnel *pattern* but implement it as a single
outbound **WebSocket over TLS (wss://, port 443)** connection from agent → relay,
with **yamux** stream multiplexing on top. One connection carries control
messages + any number of concurrent device streams. This is the architecture of
frp/ngrok/rathole, and wss/443 traverses corporate firewalls that block SSH.

SSH remains an inspiration (we could even use the SSH protocol via
`golang.org/x/crypto/ssh` as the muxed transport instead of yamux) — but we do
NOT depend on OpenSSH, sshd, or system SSH config.

## 4. Technology choices

- **Language: Go** for agent + relay.
  - Single static binary per OS/arch (`GOOS=windows/linux/darwin`) — trivial
    install for end users, no runtime deps.
  - `go.bug.st/serial` — solid cross-platform serial enumeration + I/O.
  - `gorilla/websocket` (or `nhooyr.io/websocket`), `hashicorp/yamux`.
  - Web UI embedded into the binary via `go:embed` (plain HTML/JS or a small
    Preact/htmx page — no build-step-heavy framework for v1).
- **MCP server: Go** in the same relay process (official `modelcontextprotocol/go-sdk`),
  speaking streamable-HTTP so any remote LLM host can attach.

## 5. Device model

### 5.1 Device classes (phased)

| Phase | Class | Mechanism |
|---|---|---|
| v1 | Serial (USB-CDC, UART, RS-232) | `go.bug.st/serial` — covers Arduino, ESP32, debug consoles, industrial gear |
| v2 | Raw TCP endpoints on the LAN (e.g. instrument at `192.168.x.x:5025`) | plain TCP bridge |
| v3 | Full USB passthrough | USB/IP protocol (Linux server side); needs kernel support — deliberately deferred |
| later | Audio/video/HID | TBD |

Starting serial-first keeps v1 shippable; the stream-bridge core is
device-class-agnostic so later classes are additive.

### 5.2 Identity: the word-pair ID

- Generated as `adjective-noun` from two curated wordlists (~256 × ~256 ≈ 65k
  combos; collision-checked against existing assignments).
- **Stable**: the agent persists a mapping `hardware fingerprint → word-id` in
  its local config. Fingerprint = best available of USB serial number,
  VID:PID+port-path, or user override. Replugging the same board keeps its name.
- The word-id is a **handle, not a secret**. Authz comes from the agent's token
  + the user's explicit expose-list; knowing a name grants nothing by itself.

## 6. Protocol sketch (agent ⇄ relay)

Control channel (yamux stream 0, JSON messages):

- `hello {agent_id, token, proto_version}`
- `announce {devices: [{id: "amber-falcon", class: "serial", meta: {...}, online: true}]}`
  — sent on connect and on any change (hot-plug, UI toggle).
- `open {stream_id, device_id, params {baud, ...}}` / `open_ok` / `open_err`
- `close {stream_id, reason}`

Data: one yamux stream per open device session, raw bytes both ways.
Serial line-parameter changes (baud, DTR/RTS) ride the control channel.

Reconnect: agent redials with exponential backoff; sessions do not survive
reconnect in v1 (LLM re-opens; explicit and simple).

## 7. Security model

- Agent → relay: TLS (wss). Agent authenticates with a **pairing token** minted
  on the relay (shown once; user pastes it into the local web UI).
- Local web UI binds `127.0.0.1` only. Simple session auth even locally
  (defense against malicious local pages / DNS-rebinding; CSRF token + Origin
  checks on the API).
- **Nothing is exposed unless the user toggles it on** in the web UI. The
  expose-list lives on the agent; the relay only ever learns about exposed
  devices.
- MCP side: the relay's MCP endpoint authenticates the LLM host (bearer token).
- Kill switch: big "disconnect all" in the web UI; closing the agent severs
  everything (fail-closed).

## 8. Local web UI (v1 scope)

- Device list: every enumerated serial port with metadata (VID:PID, product
  string, path), toggle **Exposed / Hidden**, shows assigned word-id.
- Connection status to relay; pairing-token entry; relay URL config.
- Live activity log (which device is open, byte counters, by whom).
- Per-device "regenerate name" and "rename fingerprint override".

## 9. Repo layout

```
tunnelhw/
  cmd/agent/         # main: local agent + embedded web UI
  cmd/relay/         # main: relay + MCP server
  internal/proto/    # control-message types, versioning
  internal/mux/      # yamux/session helpers shared by both ends
  internal/serial/   # enumeration, fingerprinting, bridging
  internal/names/    # wordlists + stable ID assignment
  web/               # UI sources, embedded via go:embed
  docs/
```

## 10. Open questions (for the panel)

1. Transport: is WSS+yamux the right call vs embedding SSH-the-protocol
   (`x/crypto/ssh` reverse channels) vs reusing frp/rathole outright?
2. Is Go the right stack, or is there a stronger case for Rust (rathole-style)
   or Node/TS (faster UI iteration)?
3. Device sessions: exclusive-open per device (one LLM session at a time) or
   shared with read-fanout? (Draft says exclusive in v1.)
4. Is the MCP server the right LLM-facing interface, or should v1 expose a plain
   HTTP/WS API and let the LLM host wrap it?
5. Word-pair ID space (~65k): sufficient? Collision/renaming policy sane?
6. Anything fatally wrong with the security model, especially the local web UI
   attack surface and the "relay compromise" scenario?
