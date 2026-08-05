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

## Deployment topologies

The relay is a small process that both ends connect *out* to. It does **not**
need a public IP — it only needs to be reachable by the agent. Pick whichever
of these describes your situation:

### A. Through SSH — recommended, needs no public address at all

If the LLM machine runs `sshd`, that is all the reachability you need. Run the
relay on the LLM machine bound to loopback; the agent connects outbound over
SSH and reaches the relay on the SSH host's own `127.0.0.1`. SSH provides the
encryption and server authentication, so no TLS certificates are involved and
**nothing is exposed to the internet**.

```
Windows agent  ──outbound SSH──▶  LLM machine
(hardware)                        ├─ sshd
                                  ├─ relay on 127.0.0.1:8443
                                  └─ LLM → http://127.0.0.1:8443/mcp
```

The agent has an SSH client built in — no `ssh -L` to run or babysit. In the
web UI choose **Through SSH**, enter the host and username, and leave the relay
URL blank (it defaults to `ws://127.0.0.1:8443/ws`, i.e. the relay on the SSH
host). On first connect the UI shows the server's host-key fingerprint for you
to verify; it is recorded in `known_hosts` and a *changed* key is refused from
then on.

### B. Direct — relay reachable from the agent

Agent and relay on the same LAN/VPN, or the relay has a public address. Use
**Direct to relay** with `wss://host:8443` and run the relay with TLS certs.

### C. Overlay network

Both machines join Tailscale/WireGuard; the relay runs on the LLM machine and
the agent uses its private mesh address. Topology B with private addressing.

## Quickstart (agent on Windows, relay on Linux)

Build everything (needs Go ≥1.25; binaries land in `dist/`):

```bash
scripts/build.sh 0.1.0
```

This walks topology **A** (over SSH), which needs no public address.

**1. Relay (on the Linux machine that runs the LLM):**

```bash
# Loopback-only: nothing is exposed to the network. SSH is the transport,
# so --insecure-dev is correct here — it means "no TLS", not "no encryption".
./tunnelhw-relay-linux-amd64 serve --listen 127.0.0.1:8443 --insecure-dev

./tunnelhw-relay-linux-amd64 pair-token                  # single-use, 5 min TTL
./tunnelhw-relay-linux-amd64 api-token --name llm-host   # bearer token for the LLM
```

(For topology B instead, use `--listen :8443 --tls-cert cert.pem --tls-key key.pem`.)

**2. Agent (Windows machine with the hardware):**

```powershell
.\tunnelhw-agent-windows-amd64.exe
```

Open http://127.0.0.1:8787, choose **Through SSH**, enter the LLM machine's SSH
host and username (plus a key file or password), leave the relay URL blank, and
paste the pairing token. Verify the host-key fingerprint when prompted. Then
toggle **Exposed** on the devices the LLM may use — each gets a stable
word-pair ID like `amber-falcon`.

**3. LLM host:** the MCP server is built into the relay — nothing extra to
install. Point an MCP client at `http://127.0.0.1:8443/mcp` (it is local to the
LLM machine) with `Authorization: Bearer <api token>`. For example, in Claude
Code:

```bash
claude mcp add tunnelhw --transport http http://127.0.0.1:8443/mcp \
  --header "Authorization: Bearer <api token>"
```

Tools: `list_devices`, `open_device`, `read`, `write`, `set_params`, `drain`,
`close_session`. If your LLM does not speak MCP, the same capabilities are
available as a plain JSON API under `/api/v1/` with the same token.

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
