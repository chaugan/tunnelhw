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

Design phase complete. Architecture v0.2 was independently reviewed by a
three-model panel (Codex / Grok / Kimi) — see
[docs/DESIGN-REVIEW-2026-08-05.md](docs/DESIGN-REVIEW-2026-08-05.md) for the
full adjudication. Implementation follows the locked order in
[docs/ARCHITECTURE.md §11](docs/ARCHITECTURE.md).

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
