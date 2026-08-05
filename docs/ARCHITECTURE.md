# TunnelHW — Architecture (v0.5)

> Status: this document describes the system **as built**, at v0.5.x. It began
> as a pre-implementation design (v0.1–v0.2) reviewed by a three-model panel;
> that adjudication is kept, for its rationale, in
> [DESIGN-REVIEW-2026-08-05.md](DESIGN-REVIEW-2026-08-05.md). Where the code
> and the original design diverged, the code wins here. Anything designed but
> not built is collected in §12 and is not described anywhere else as if it
> shipped.

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

In practice a sixth requirement emerged from use and now shapes the recommended
deployment: **neither machine should need a public address**. See §3.

## 2. High-level design

The recommended deployment: the relay runs on the LLM's own machine bound to
loopback, and the agent reaches it by dialing **outbound over SSH** to that
machine's `sshd`. Nothing but `sshd` listens, and no certificates are involved.

```
┌────────────────────────────┐                ┌──────────────────────────────┐
│  User's machine            │                │  LLM machine                 │
│                            │   outbound     │                              │
│  ┌──────────┐  ┌────────┐  │   ssh :22      │  ┌──────┐                    │
│  │ hardware │──│ agent  │──┼───────────────▶│  │ sshd │                    │
│  └──────────┘  └────────┘  │  (direct-tcpip │  └──┬───┘                    │
│                    │       │   → loopback)  │     │ forward to 127.0.0.1   │
│              ┌─────────┐   │                │  ┌──▼──────┐  ┌──────────┐   │
│              │ web UI  │   │                │  │  relay  │──│ MCP srv  │──▶ LLM
│              │127.0.0.1│   │                │  │127.0.0.1│  └──────────┘   │
│              └─────────┘   │                │  └─────────┘  + /api/v1 JSON │
└────────────────────────────┘                └──────────────────────────────┘
```

Inside the SSH channel the agent still speaks the same protocol it would speak
over the open network: **yamux multiplexed streams over one WebSocket**. The
carrier changes; nothing above it does.

The alternative deployment is the direct one: the relay is reachable at
`wss://host:port` with its own certificate (`--tls-cert`/`--tls-key`), and the
agent dials it straight. This is the right shape when the relay legitimately
has a reachable address (same LAN, VPN, or overlay network such as
Tailscale/WireGuard).

| Component | Runs on | Role |
|---|---|---|
| `agent` | user's machine | enumerates hardware, serves the localhost web UI, dials the relay (over SSH or directly), bridges device I/O |
| `relay` | LLM's machine (or any host both ends can reach) | accepts agent connections, tracks devices, brokers sessions, exposes the versioned relay API |
| `mcp`   | same process as the relay | MCP server: a **thin adapter** mapping tools 1:1 onto the relay API |

`relay` and `mcp` are one process (the MCP endpoint can be turned off with
`--mcp=false`), with **strictly separate auth middleware** — agent credentials
and LLM-host credentials are different principals with different blast radii.
The internal relay API (`internal/relayapi`) is the tested, versioned core;
MCP is never the business-logic layer. That seam paid off: the plain JSON
surface is **implemented** as `/api/v1` (`internal/relayapi/http.go` —
devices, sessions, read, write, params, drain, close), sitting beside the MCP
adapter over the same broker, with no refactoring of either.

## 3. Carriers

The reverse-tunnel *pattern* is fixed: the agent always dials outbound. What
carries that connection has two supported answers.

### 3.1 SSH carrier — the recommended deployment

A reverse tunnel needs a rendezvous point both ends can reach, and requiring
the relay to have a public address is a real barrier for the common case of
"LLM on one machine, hardware on another, neither with a public IP."

If the LLM machine runs `sshd` — which it usually does — that *is* the
rendezvous. The agent (`internal/sshtun`) opens an outbound SSH connection and
requests a `direct-tcpip` forward to the SSH host's own loopback, where the
relay listens. The WebSocket/yamux protocol rides inside that channel
unchanged, so this is a transport option, not a second protocol.

Consequences, deliberate:

- The relay binds `127.0.0.1` and is **never exposed**; the LLM reaches it over
  localhost, the agent through SSH.
- Plaintext `ws://` inside the SSH channel is correct, not a downgrade: SSH
  supplies the encryption and authenticates the server by host key. The TLS
  requirement is therefore waived when (and only when) the SSH carrier is in
  use — never silently for direct connections. The agent enforces exactly that
  in `Tunnel.validateURL`.
- Host keys are checked against `known_hosts`. An unknown host is a hard error
  carrying the fingerprint, surfaced in the web UI for human approval (TOFU
  with a human in the loop); a **changed** key always fails and is never
  auto-accepted.
- Credentials: an `ssh-agent` identity, a private key file (optionally
  passphrase-protected), or a password. `~/.ssh/config` is honoured for
  `HostName`, `User`, `Port` and `IdentityFile`, so a short alias resolves as
  it does in a terminal.
- The relay in this deployment runs with `--insecure-dev` on a loopback
  listener. The relay refuses that combination on a non-loopback address, so
  the flag cannot quietly become a public plaintext port.

### 3.2 Direct WSS carrier — the alternative

One outbound **WebSocket over TLS** (`wss`, typically 443 — traverses
firewalls that block SSH) carrying the same yamux streams. Used when the relay
has an address the agent can reach. TLS verification is the Go default:
system trust roots. Plaintext `ws://` is refused unless the connection runs
over SSH or the operator passed `--insecure-dev`.

WSS is a *carrier*, not the protocol: the control protocol is versioned and
transport-agnostic, which is exactly what made the SSH carrier a drop-in
addition rather than a fork, and what would let it later ride HTTP/2 or QUIC
without touching device logic.

### 3.3 Why not literal (reverse) SSH as the protocol

`ssh -R` is the right *shape* and would work as a prototype, but as a product:
one TCP port per device, sshd + account/key management on the server, a
ser2net/RFC2217 bridge per device anyway (serial isn't TCP), and no clean place
for device metadata, presence, or naming. Embedding SSH-the-protocol
(`x/crypto/ssh`) as the control plane buys auth/mux but forces host-key-trust
UX and fits "open serial with params" poorly; reusing frp/rathole means forking
someone else's control plane.

Rejecting OpenSSH as the *protocol* did not rule it out as a *carrier*, and
that distinction turned out to matter more than the original design expected —
hence §3.1. TunnelHW embeds an SSH *client* (`golang.org/x/crypto/ssh`) and
owns its own control plane on top.

## 4. Technology choices

- **Go** for agent + relay: static cross-compiled binaries
  (`GOOS=windows|linux|darwin`), one-developer maintainable.
- `go.bug.st/serial` — cross-platform serial enumeration + I/O. On macOS,
  full USB metadata needs a native cgo build; cross-compiled macOS binaries
  enumerate with degraded metadata (`enumerate_native.go` /
  `enumerate_fallback.go`).
- `coder/websocket` (context-native), `hashicorp/yamux` with **explicitly
  pinned config** in `internal/mux` (accept backlog, keepalive interval,
  connection write timeout, max stream window, stream open/close timeouts) —
  never defaults.
- `golang.org/x/crypto/ssh` for the SSH carrier, including `known_hosts`
  verification and ssh-agent support.
- Web UI: **zero-build vanilla JavaScript** (one `app.js`, one `style.css`,
  one `index.html`) embedded via `go:embed` — no framework, no Node toolchain,
  preserving the single-binary install story.
- MCP: official `modelcontextprotocol/go-sdk`, streamable-HTTP transport,
  stateless mode.
- Service installation (`internal/svc`) uses each platform's own manager:
  Windows services, systemd (user or system), launchd.

## 5. Device model

### 5.1 Device class

v1 ships exactly one device class: **serial — any serial port the OS sees**.
USB-CDC adapters, native COM ports and motherboard UARTs, PCI/PCIe serial
cards, RS-232/485, and Bluetooth SPP virtual ports all enumerate through
`go.bug.st/serial`, which covers Arduino and ESP32 boards, debug consoles,
industrial gear, and legacy equipment.

The stream-bridge core is device-class-agnostic, so further classes would be
additive — but none exist today. Candidate classes and the constraints they
would have to satisfy are in §12; nothing else in this document assumes them.

### 5.2 Identity: the word-pair ID

- `adjective-noun` from two curated lists — 228 adjectives and 232 nouns as
  shipped (curated: no slurs, brands, or near-duplicates like gray/grey).
- **Scoped per agent.** A machine hosts tens of devices, not thousands, so the
  ~53k combinations are ample. Within one agent, generation retries against the taken
  set and appends a third word if it must. Across agents, the relay does not
  rename anything: a word-ID that exists on two connected agents is reported as
  ambiguous, and the caller qualifies it as `agent_id/word-id`.
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
    when devices are added/removed.
  - The tier travels with the device: it is shown as a confidence badge per row
    in the local UI and returned to the LLM as `fingerprint_confidence`. When a
    weak fingerprint has drifted onto the wrong board, the remedy the UI
    actually offers is **Regenerate**, which assigns a fresh word-ID; there is
    no fingerprint pin or override (§12).
- Every device also has an internal **UUID** as the durable identity; the
  word-id is the presentation-layer handle. It is a handle, **not a secret**
  (the UI copy says so): authz comes from credentials + the expose-list.

## 6. Protocol (agent ⇄ relay)

Framing: yamux over one WebSocket. Yamux has no reserved stream 0 (IDs are
odd/even by side) — by convention, **the first stream the agent opens is the
control stream**. Control messages are newline-delimited JSON envelopes with
**correlation IDs**.

Control stream:

- `hello {agent_id, credential, proto_versions, agent_version}` → relay picks
  the version both sides share, enforces a floor (`VersionFloor`), or closes
  with `hello_err`. `hello_ok` carries the heartbeat interval.
- `announce {devices: [{id, uuid, class, meta, online, busy, claimed_by}]}` —
  on connect and on every change (hot-plug, UI toggle, claim/release).
- `set_params` / `drain` and their results, and `session_closed`, ride the
  control stream, correlation-ID'd.
- `ping` / `pong` in both directions.

Device sessions — self-contained, no cross-channel race:

- The relay opens a **new yamux stream**; its **first frame is the JSON
  open-header** `{corr_id, device_id, params}`; the reply frame is
  `open_ok {session_id}` or `open_err {reason}` (deterministic `busy` error
  includes holder metadata); thereafter the stream is raw bytes both ways.
- **Exclusive open** per device (serial is stateful; shared writers corrupt
  framed protocols). Observe-only fanout is not implemented (§12).

Lifecycle limits as actually enforced:

| Bound | Value | Where |
|---|---|---|
| control / open-header frame | 64 KiB | `proto.MaxControlMessage` |
| WebSocket carrier frame | 2 MiB | agent `wsReadLimit` |
| single write | 256 KiB | `relayapi.MaxWriteBytes` |
| single read | ≤ 256 KiB, default 4 KiB | `relayapi.MaxReadBytes` / `DefaultReadMax` |
| read timeout | ≤ 60 s, default 2 s | `relayapi.MaxReadTimeout` / `DefaultTimeout` |
| per-session receive buffer | 1 MiB, then backpressure | `relayapi.sessionBufCap` |
| hello / open-header handshake | 15 s | agent `helloTimeout`, `openHdrTimeout` |
| yamux | 30 s keepalive, 15 s write timeout, 256 KiB stream window, 64 accept backlog, 15 s open / 10 s close timeouts | `internal/mux` |
| liveness | agent pings each heartbeat interval; no pong within `2×interval + 5 s` drops the session | agent `heartbeatLoop` |

Bytes flow until either direction ends, at which point the session closes;
there is **no half-close**. There is also no write-*rate* limiter and no cap on
concurrent sessions per token — exclusive open per device is the only bound on
concurrency (§12).

Reconnect: exponential backoff (1 s → 60 s, reset after a session that lasted
over a minute); **sessions do not survive reconnect**. The agent re-announces;
consumers re-open. Writes are never buffered or replayed across the gap —
replaying serial writes into unknown hardware state is dangerous. MCP tool
descriptions state this explicitly.

## 7. Security model

**Threat model (stated honestly):** v1 is **self-hosted with a trusted relay** —
you run your own relay, normally on the LLM's own machine. A compromised relay
can operate any *currently exposed* device. Mitigations: minimal expose-list,
capability grants, kill switch, per-device release, single-use short-lived
pairing tokens, and no payload logging. End-to-end encryption past the relay is
incompatible with a relay-hosted MCP server (it must read device bytes to serve
the LLM) and is out of scope unless an untrusted/multi-tenant relay ever becomes
a goal — see the design review for the full adjudication.

- **Transport:** with the SSH carrier, SSH provides encryption and server
  authentication (host keys via `known_hosts`; unknown = human approval,
  changed = hard failure). Direct connections require TLS, verified against
  the **system trust store** — the default `coder/websocket` dial, with no
  custom `tls.Config`. There is no certificate pinning (§12). Plaintext `ws://`
  is accepted only inside an SSH channel or behind an explicit
  `--insecure-dev` flag, and that flag is deliberately **not persisted** — it
  must be re-passed every launch. The relay likewise refuses to start without
  TLS unless `--insecure-dev` is given, and refuses `--insecure-dev` on a
  non-loopback listen address.
- **Agent enrollment:** relay mints a **single-use pairing token with a
  5-minute TTL**; the `/pair` endpoint is rate-limited (fixed window, 10
  attempts per minute, global). The user pastes the token into the local web
  UI; the agent exchanges it over the verified transport for a **per-agent
  credential**. The relay stores only a **SHA-256 hash**; revoke is supported;
  neither token nor credential is ever logged.
- **Credential storage — flat files, no keychain.** There is no OS keychain
  integration on any platform. The agent writes `agent.json` (relay URL, agent
  ID, credential, device map, and any SSH password/passphrase the user entered)
  atomically with mode `0600` inside a `0700` directory
  (`internal/config`). The relay writes `auth.json` the same way
  (`internal/auth`), containing only hashes. The relay's store lives under the
  per-user config directory when run as a user, and under a system directory
  (`/var/lib/tunnelhw-relay`, `%ProgramData%`, `/Library/Application Support`)
  when run as root or as a system service; every command prints which store it
  is using. File permissions are the whole of the at-rest protection.
- **LLM-host auth:** the MCP endpoint and `/api/v1` use bearer tokens — a
  different principal from the agent credential, never shared, separate
  middleware. A token record carries two scopes: **read-only**, which rejects
  every mutating call (open, write, set_params, drain, close), and an optional
  **agent allowlist** (empty = all agents). There is no device-level scope
  (§12). Sessions are additionally owned by the token that opened them: another
  credential's session is indistinguishable from a nonexistent one.
- **Exposure & capabilities:** nothing exposed by default; the expose-list
  lives on the agent, and the relay never learns about hidden devices. Hiding
  an exposed device ends its session immediately. Control-line access
  (DTR/RTS) and mid-session baud changes are a **separate per-device grant** —
  they can reset boards or enter bootloaders — and both the grant denial and
  the change itself are logged loudly in the activity view.
- **Local web UI:** binds a loopback IP literal exactly (a non-loopback listen
  address is refused outright, and `localhost` is never used for the bind, so
  it can't go dual-stack); allowlists the `Host` header against the bound
  address and `localhost:<port>`; rejects non-local `Origin`; requires a
  per-launch random CSRF token on every non-GET request;
  `Content-Security-Policy: default-src 'self'`; `X-Content-Type-Options:
  nosniff`; no CORS headers at all. (Localhost admin UIs are routinely attacked
  via DNS rebinding and malicious pages.)
- **Privacy:** logs carry metadata only — device IDs, connection state, byte
  counts, session open/close, control-line events. **Serial payloads are never
  logged**, and there is no debug toggle that would log them: no code path
  writes device bytes to a log. Serial traffic often contains firmware secrets
  and calibration data, so the capability simply doesn't exist.
- **Kill switch:** "disconnect all" in the UI severs the tunnel and every
  session and *stays* severed until explicitly resumed (a latch, not a
  reconnect blip); agent exit severs everything (fail-closed). Per-device
  **Release** force-closes one session without touching the tunnel.

## 8. LLM-facing MCP tools (guardrailed)

Tools map 1:1 onto the relay API. LLM-safe by construction, and the exact set
the server registers is:

- `list_devices` → agent id/name, word-id, class, transport, path, product,
  `fingerprint_confidence`, `control_lines_allowed`, online/busy/`claimed_by`.
- `open_device {device_id, baud?, data_bits?, parity?, stop_bits?}` → session;
  deterministic `busy` error names the holder. Defaults 115200 8-N-1.
- `read {session_id, timeout_ms?, max_bytes?, delimiter?}` — bounded, never
  indefinitely blocking; returns `text` when the bytes are valid UTF-8,
  `data_b64` always, plus `timed_out` and `eof`; partial reads documented.
- `write {session_id, data, encoding: utf8|base64}` — base64 for binary.
- `set_params {session_id, baud?, dtr?, rts?}` — requires the device's
  control-line grant, including for a baud-only change.
- `drain {session_id}`, `close_session {session_id}` — explicit close required.

Tool descriptions state session-reset-on-reconnect, exclusive-open semantics,
and the requirement to close, so the host knows what to expect. Read-only
tokens are rejected on every tool except `list_devices` and `read`.

The same operations exist over HTTP for clients that don't speak MCP:
`GET /api/v1/devices`, `GET|POST /api/v1/sessions`,
`POST /api/v1/sessions/{id}/read|write|params|drain`,
`DELETE /api/v1/sessions/{id}`.

## 9. Local web UI

Implemented today:

- Device table: every enumerated serial port with its word-ID, path, transport,
  a fingerprint **confidence badge** (weak carries an explanatory tooltip),
  product string, live online/busy/offline state, an **Exposed** toggle, a
  per-device **control-lines grant** toggle, a **Regenerate** button that
  assigns a fresh word-ID, and — only while the device is busy — a **Release**
  button that force-closes the holding session.
- Relay connection: status banner, relay URL, and a pairing form with two
  modes — **Direct to relay** (`wss://…`) and **Through SSH** (host, user, key
  path, passphrase/password, optional relay URL that defaults to
  `ws://127.0.0.1:8443/ws` on the SSH host). An unrecognized SSH host key
  stops the flow and shows the fingerprint for explicit human approval.
- Live sessions list with per-session byte counters, and an activity log with
  loud entries for control-line/baud operations and grant denials.
- Kill switch.

There is **no fingerprint pin or override** in the UI or its API; Regenerate is
the only identity control (§12).

## 10. Repo layout

```
tunnelhw/
  cmd/agent/          # main: local agent + embedded web UI, service subcommand
  cmd/relay/          # main: relay serve + pairing/API-token admin CLI + service
  internal/proto/     # control messages, correlation IDs, version negotiation
  internal/mux/       # pinned yamux config for both ends
  internal/agent/     # device registry, exclusive sessions, tunnel client, activity log
  internal/config/    # agent's persisted state (0600 file, atomic writes)
  internal/serialdev/ # enumeration, tiered fingerprinting, port I/O
  internal/names/     # curated wordlists + stable per-agent ID assignment
  internal/sshtun/    # SSH carrier: known_hosts, ssh-agent, ~/.ssh/config
  internal/webui/     # localhost UI server + hardening middleware
  internal/relay/     # hub: agent connections, device registry, stream opens
  internal/relayapi/  # versioned core API (broker) + /api/v1 HTTP surface
  internal/mcp/       # MCP adapter over the relay API
  internal/auth/      # relay credential store (hashes only)
  internal/svc/       # Windows service / systemd / launchd installation
  internal/e2e/       # hardware-free end-to-end tests, incl. the SSH carrier
  web/                # zero-build UI sources, embedded via go:embed
  docs/
```

## 11. Current status

Everything below is built and covered by tests, including a hardware-free
end-to-end test that drives pairing → announce → open → echo → grants → close,
and a second one over a real in-process SSH server.

1. `internal/proto` — control protocol with correlation IDs and version
   negotiation; no magic stream 0.
2. Pairing-token mint/hash/exchange/revoke, rate-limited, plus TLS enforcement
   and the `--insecure-dev` escape hatch.
3. Serial enumeration, tiered fingerprinting, stable word-IDs, exclusive open,
   agent ⇄ relay bridge.
4. LLM-friendly bounded read/write semantics in the relay API, with
   per-credential session ownership.
5. MCP adapter over the relay API, and the `/api/v1` JSON surface beside it.
6. Localhost UI with the full hardening set, per-device grants, kill switch,
   and per-device release.
7. SSH carrier: `known_hosts` verification with human-approved TOFU,
   ssh-agent, `~/.ssh/config`, and a loopback-only relay.
8. Packaging: cross-compiled binaries, Windows version-info resources, and
   service installation on Windows/systemd/launchd.

## 12. Not implemented / possible future work

None of the following exists in the code. They are recorded because the design
work is real and the constraints are worth keeping, not because they are
pending features with a date.

- **TCP device class.** Raw TCP endpoints on the user's LAN as a second device
  class. If built, the SSRF guard is a precondition, not a follow-up: targets
  must be user-allowlisted CIDR:port entries in the UI, with link-local and
  cloud-metadata ranges blocked by default.
- **USB/IP passthrough.** Full USB device passthrough. Deliberately deferred:
  kernel drivers and privileged services, fragile across platforms, and
  overkill for serial.
- **OS keychain credential storage.** Today every credential is a `0600` file
  (§7). Keychain/Credential Manager/Secret Service integration was described in
  earlier drafts and was never built.
- **Certificate pinning.** Direct TLS connections verify against the system
  trust store only.
- **Fingerprint pin / override.** The UI surfaces a confidence tier and can
  regenerate a name; it cannot pin a word-ID to a chosen physical device or
  override a computed fingerprint.
- **Observe-only session fanout.** Open is exclusive, full stop.
- **Device-level token scopes.** API/MCP tokens carry read-only and an agent
  allowlist; per-device scoping does not exist.
- **Write-rate limiting and per-token concurrency caps.** Sizes and buffers are
  capped (§6); rates and session counts are not.
- **Half-close semantics.** Either direction ending tears the session down.
- **End-to-end encryption past the relay.** Rejected for v1 with reasons; see
  the design review. Only revisitable if an untrusted or multi-tenant relay
  becomes a goal.
