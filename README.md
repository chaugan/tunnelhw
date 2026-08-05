# TunnelHW

Expose selected local hardware (serial devices first) to an LLM agent running on a
remote server — as if the hardware were attached to that server.

- A cross-platform **agent** (Windows/Linux/macOS) runs on the machine with the
  hardware and serves a **localhost web UI** where the user picks which devices
  to expose. Nothing is exposed by default.
- The agent dials **outbound** to a **relay** on the remote server (reverse-tunnel
  pattern over WSS), so it works behind NAT/firewalls.
- Each exposed device gets a stable, human-readable two-word ID like
  `amber-falcon` — the handle the LLM uses to open it.
- The relay exposes the devices to the LLM via an **MCP server**
  (`list_devices`, `open`, `read`, `write`, …).

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full design.

TunnelHW is designed as a **self-hostable open-source system**: you run the
agent on your machine and the relay on your own server. Nothing in the code
assumes a particular machine or operator — all endpoints, ports, and
credentials are configuration. (The repo is private during early development
and will be opened up.)

## Status

v0.1: the full serial path works end to end (agent ⇄ relay ⇄ HTTP API / MCP),
covered by a hardware-free e2e test. Architecture v0.2 was independently
reviewed by a three-model panel (Codex / Grok / Kimi) — see
[docs/DESIGN-REVIEW-2026-08-05.md](docs/DESIGN-REVIEW-2026-08-05.md); the
component code went through a second two-lens review round whose accepted
findings are all applied.

## Quickstart (agent on Windows, relay on Linux)

Build everything (needs Go ≥1.25; binaries land in `dist/`):

```bash
scripts/build.sh 0.1.0
```

**1. Relay (Linux server, next to the LLM):**

```bash
./tunnelhw-relay-linux-amd64 serve --tls-cert cert.pem --tls-key key.pem   # or --insecure-dev for a LAN test
./tunnelhw-relay-linux-amd64 pair-token          # print a single-use pairing token (5 min TTL)
./tunnelhw-relay-linux-amd64 api-token --name llm-host   # print the LLM-host bearer token
```

**2. Agent (Windows machine with the hardware):**

```powershell
.\tunnelhw-agent-windows-amd64.exe        # add --insecure-dev only for a plaintext LAN test
```

Open http://127.0.0.1:8787, paste the relay URL (`wss://your-relay:8443`) and
the pairing token, then toggle **Exposed** on the devices the LLM may use.
Each gets a stable word-pair ID like `amber-falcon`.

**3. LLM host:** point an MCP client at `https://your-relay:8443/mcp` with
`Authorization: Bearer <api token>`. Tools: `list_devices`, `open_device`,
`read`, `write`, `set_params`, `drain`, `close_session`. A plain HTTP API
mirrors them under `/api/v1/` with the same token.

macOS note: cross-compiled darwin binaries enumerate ports with degraded
metadata (no USB serial numbers — macOS needs cgo/IOKit for that); build
natively on a Mac with `CGO_ENABLED=1` for full fingerprinting.

## Repo layout (planned)

```
cmd/agent/         # local agent + embedded web UI
cmd/relay/         # relay + MCP server
internal/proto/    # control-message types, versioning
internal/mux/      # stream-multiplexing helpers
internal/serial/   # enumeration, fingerprinting, bridging
internal/names/    # wordlists + stable word-pair ID assignment
web/               # UI sources (go:embed)
docs/              # architecture & design notes
```
