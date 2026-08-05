# TunnelHW — Architecture (v0.2)

> Status: reviewed. v0.1 was independently reviewed by a three-model panel
> (Codex / Grok / Kimi); the adjudication is in
> [DESIGN-REVIEW-2026-08-05.md](DESIGN-REVIEW-2026-08-05.md). This version folds
> in all accepted findings.

## 0. Project stance

TunnelHW is built to be a **public, open-source, self-hostable system**: anyone
can run the agent on their machine and the relay on their own server. Nothing in
the design or code may assume a particular machine, operator, or deployment —
all endpoints, ports, and credentials are configuration.

TunnelHW is *inspired by* reverse tunnels (ngrok/frp/`ssh -R`) but is **not a
general-purpose tunnel product**. The device-session control plane — enumerate,
expose, name, open, guard — is the product. Generic TCP-forwarding features that
don't serve that are out of scope.

## 1. Problem statement

A user has physical hardware (serial devices, dev boards) attached to their
local machine (Windows / Linux / macOS). An LLM agent running on a **remote**
server must be able to use that hardware **as if it were locally attached**.

Requirements:

1. **Local agent** runs on the machine with the hardware.
2. **Local web UI** (localhost only) where the human selects which devices are
   exposed. Nothing is exposed by default.
3. Each exposed device gets a **human-readable ID**: two random words joined by
   a hyphen (`amber-falcon`) — the handle the LLM uses.
4. The local machine is typically behind NAT/firewall, so the agent **dials
   outbound** (reverse-tunnel pattern).
5. Cross-platform: Windows, Linux, macOS. Single static binary, no runtime deps.

## 2. High-level design

```
┌────────────────────────────┐                ┌─────────────────────────────┐
│  User's machine            │                │  Self-hosted remote server  │
│                            │   outbound     │                             │
│  ┌──────────┐  ┌────────┐  │   wss://443    │  ┌─────────┐  ┌──────────┐  │
│  │ hardware │──│ agent  │──┼───────────────▶│  │  relay  │──│ MCP srv  │──▶ LLM host
│  └──────────┘  └────────┘  │  (yamux over   │  └─────────┘  └──────────┘  │
│                    │       │   WebSocket)   │        internal relay API   │
│              ┌─────────┐   │                └─────────────────────────────┘
│              │ web UI  │   │
│              │127.0.0.1│   │
│              └─────────┘   │
└────────────────────────────┘
```

| Component | Runs on | Role |
|---|---|---|
| `agent` | user's machine | enumerates hardware, serves the localhost web UI, dials the relay, bridges device I/O |
| `relay` | user's server | accepts agent connections, tracks devices, exposes the **internal relay API** |
| `mcp`   | user's server | MCP server: a **thin adapter** mapping tools 1:1 onto the relay API |

`relay` and `mcp` are one process in v1 (the MCP endpoint can be disabled by
flag), with **strictly separate auth middleware** — agent credentials and
LLM-host credentials are different principals with different blast radii. The
internal relay API (`internal/relayapi`) is the tested, versioned core; MCP is
never the business-logic layer, so a plain HTTP/WS surface can be added later
without refactoring.

## 3. Why not literal (reverse) SSH?

`ssh -R` is the right *shape* and would work as a prototype, but as a product:
one TCP port per device, sshd + account/key management on the server, a
ser2net/RFC2217 bridge per device anyway (serial isn't TCP), and no clean place
for device metadata, presence, or naming. Embedding SSH-the-protocol
(`x/crypto/ssh`) buys auth/mux but forces host-key-trust UX and fits "open
serial with params" poorly; reusing frp/rathole means forking someone else's
control plane.

**Decision (panel-unanimous):** the reverse-tunnel *pattern*, implemented as one
outbound **WebSocket over TLS** (wss, port 443 — traverses firewalls that block
SSH) carrying **yamux** multiplexed streams. WSS is a *carrier*: the control
protocol is versioned and transport-agnostic so it could later ride HTTP/2 or
QUIC without touching device logic.

## 4. Technology choices

- **Go** for agent + relay: static cross-compiled binaries
  (`GOOS=windows|linux|darwin`), one-developer maintainable.
- `go.bug.st/serial` — cross-platform serial enumeration + I/O.
- `coder/websocket` (context-native), `hashicorp/yamux` with **explicitly
  pinned config** (keepalive interval, max stream window) — never defaults.
- Web UI: **zero-build** vanilla JS/htmx embedded via `go:embed` — no Node
  toolchain, preserving the single-binary install story.
- MCP: official `modelcontextprotocol/go-sdk`, streamable-HTTP transport.

## 5. Device model

### 5.1 Device classes (phased)

| Phase | Class | Mechanism |
|---|---|---|
| v1 | Serial — **any** serial port the OS sees: USB-CDC adapters, native COM ports / motherboard UARTs, PCI/PCIe serial cards, RS-232/485, Bluetooth SPP virtual ports | `go.bug.st/serial` — Arduino, ESP32, debug consoles, industrial gear, legacy equipment |
| v2 | Raw TCP endpoints on the user's LAN | TCP bridge. **SSRF guard designed now:** targets must be user-allowlisted CIDR:port entries in the UI; link-local/metadata ranges blocked by default |
| v3 | Full USB passthrough | USB/IP; deliberately deferred (kernel drivers, privileged services) |

The stream-bridge core is device-class-agnostic; later classes are additive.

### 5.2 Identity: the word-pair ID

- `adjective-noun` from two curated ~256-word lists (curated: no slurs, brands,
  or near-duplicates like gray/grey).
- **Scoped per agent.** A machine hosts tens of devices, not thousands, so the
  ~65k space is ample; when a relay has multiple paired agents, IDs are
  namespaced by agent. On the rare collision, a third word is appended.
- **Stable**: the agent persists `fingerprint → word-id` atomically in local
  config. Fingerprinting is **transport-agnostic and tiered** — USB is one
  transport, not an assumption; native COM ports and other non-USB serial
  hardware are first-class:
  - **Strong** — USB serial number, when the port is a USB device that
    reports one.
  - **Medium** — native/fixed port identity: a motherboard UART or PCI/PCIe
    serial card's platform-stable name (`COM1`, `/dev/ttyS0`) *is* its
    hardware identity and doesn't move across replugs — acceptable as-is.
  - **Weak** — USB VID:PID + port-path (no serial number), or any
    hot-pluggable port identified only by an OS-assigned name
    (`/dev/ttyUSB0`, high-numbered `COMn`, Bluetooth SPP): these renumber
    when devices are added/removed. UI shows a confidence warning and offers
    a user override.
  - The tier is shown per device in the UI; the user can always pin/override.
- Every device also has an internal **UUID** as the durable identity; the
  word-id is the presentation-layer handle. It is a handle, **not a secret**
  (the UI copy says so): authz comes from credentials + the expose-list.

## 6. Protocol (agent ⇄ relay)

Framing: yamux over one WebSocket. Yamux has no reserved stream 0 (IDs are
odd/even by side) — by convention, **the first stream the agent opens is the
control stream**. All control messages are JSON with **correlation IDs**.

Control stream:

- `hello {agent_id, credential, proto_versions: [max supported]}` → relay picks
  the version (min of maxes), enforces a floor, or closes with an error.
- `announce {devices: [{id, uuid, class, meta, online, busy, claimed_by}]}` —
  on connect and on every change (hot-plug, UI toggle, claim/release).
- Line-parameter changes (baud, DTR/RTS) and drains ride the control stream,
  correlation-ID'd.

Device sessions — self-contained, no cross-channel race:

- Consumer side opens a **new yamux stream**; its **first frame is the JSON
  open-header** `{corr_id, device_id, params}`; the reply frame is
  `open_ok {session_id}` or `open_err {reason}` (deterministic `busy` error
  includes holder metadata); thereafter the stream is raw bytes both ways.
- **Exclusive open** per device (serial is stateful; shared writers corrupt
  framed protocols). Observe-only fanout is a possible later feature, not v1.

Lifecycle (explicit, because tunnels die in boring edge cases): heartbeat
interval + liveness timeout; open timeout; half-close semantics; backpressure
when the agent can't drain; caps on concurrent opens, write rate, stream
buffer, and control-message size (a stuck LLM loop must not flood a device).

Reconnect: exponential backoff; **sessions do not survive reconnect**. The
agent re-announces; consumers re-open. Writes are never buffered or replayed
across the gap — replaying serial writes into unknown hardware state is
dangerous. MCP tool descriptions state this explicitly.

## 7. Security model

**Threat model (stated honestly):** v1 is **self-hosted with a trusted relay** —
you run your own relay. A compromised relay can operate any *currently exposed*
device. Mitigations: minimal expose-list, capability grants, kill switch,
short-lived scoped tokens, no payload logging. End-to-end encryption past the
relay is incompatible with a relay-hosted MCP server (it must read device bytes
to serve the LLM) and is out of scope unless an untrusted/multi-tenant relay
ever becomes a goal — see the design review for the full adjudication.

- **Transport:** TLS with mandatory system-CA (or pinned-cert) verification;
  plaintext `ws://` only behind an explicit `--insecure-dev` flag.
- **Agent enrollment:** relay mints a **single-use pairing token, ~5-minute
  TTL, rate-limited**; user pastes it into the local web UI; the agent
  exchanges it over verified TLS for a **per-agent credential**. The relay
  stores only a **hash**; revoke/rotate supported; stored in the OS keychain
  where available (flat file fallback on headless systems, permissions 0600);
  never logged.
- **LLM-host auth:** the MCP endpoint uses its own bearer token — a different
  principal from the agent credential, never shared, separate middleware. The
  token record carries scopes (agents / devices / read-only) from day one;
  enforcement is simple in v1, least-privilege defaults once multiple agents
  appear.
- **Exposure & capabilities:** nothing exposed by default; the expose-list
  lives on the agent, and the relay never learns about hidden devices.
  Control-line access (DTR/RTS) and baud changes are a **separate per-device
  grant** — they can reset boards or enter bootloaders — and are logged loudly
  in the activity view.
- **Local web UI:** binds `127.0.0.1` exactly (not `localhost` dual-stack);
  allowlists the `Host` header; rejects non-local `Origin`; CSRF token;
  per-launch random session secret; `Content-Security-Policy: default-src
  'self'`; no wildcard CORS. (Localhost admin UIs are routinely attacked via
  DNS rebinding and malicious pages.)
- **Privacy:** logs carry metadata only (device IDs, connection state, byte
  counts). Serial payloads are logged only under an explicit local debug
  toggle with a visible warning — serial traffic often contains firmware
  secrets and calibration data.
- **Kill switch:** "disconnect all" in the UI; agent exit severs everything
  (fail-closed).

## 8. LLM-facing MCP tools (guardrailed)

Tools map 1:1 onto the relay API. LLM-safe by construction:

- `list_devices` → id, class, meta, online/busy state.
- `open {device_id, params}` → session; deterministic `busy` error names the
  holder.
- `read {session, timeout, max_bytes, delimiter?}` — bounded, never
  indefinitely blocking; partial reads documented.
- `write` / `write_line {session, data, encoding: utf8|base64}` — base64 for
  binary.
- `set_params {session, baud?, dtr?, rts?}` — only if the device's
  control-line grant allows.
- `drain {session}`, `close {session}` — explicit close required.
- Tool descriptions state session-reset-on-reconnect so the host knows to
  re-open.

## 9. Local web UI (v1 scope)

- Device list: every enumerated serial port with metadata (VID:PID, product
  string, path), **Exposed / Hidden** toggle, assigned word-id, fingerprint
  confidence indicator, per-device control-line grant toggle.
- Relay connection status; pairing-token entry; relay URL config.
- Live activity log: which device is open, by whom, byte counters, loud
  entries for control-line/baud operations.
- Per-device "regenerate name" and fingerprint override.

## 10. Repo layout

```
tunnelhw/
  cmd/agent/         # main: local agent + embedded web UI
  cmd/relay/         # main: relay + MCP adapter (MCP disableable by flag)
  internal/proto/    # control messages, correlation IDs, version negotiation
  internal/relayapi/ # the versioned core API — MCP maps onto this
  internal/mux/      # websocket+yamux session helpers (pinned config)
  internal/serial/   # enumeration, fingerprinting, bridging
  internal/names/    # curated wordlists + stable per-agent ID assignment
  web/               # zero-build UI sources, embedded via go:embed
  docs/
```

## 11. Implementation order (locked, per panel)

1. `internal/proto`: control protocol with correlation IDs, version
   negotiation, no magic stream 0.
2. Pairing-token mint/hash/exchange/revoke + TLS verification.
3. Serial enumeration, fingerprinting, exclusive open; agent ⇄ relay bridge.
4. LLM-friendly read/write tool semantics in the relay API.
5. MCP adapter over the relay API.
6. Localhost UI with the full hardening set.
7. Everything else (TCP class, observe-only fanout) only after a working
   end-to-end serial path.
