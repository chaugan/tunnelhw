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

## Status

Early design phase. The architecture draft is under review by an external
model panel (Codex / Grok / Kimi) per the sparring playbook; implementation
starts once the design is adjudicated.

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
