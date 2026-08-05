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
- The relay exposes the devices to the LLM via a built-in **MCP server**
  (`list_devices`, `open_device`, `read`, `write`, `set_params`, `drain`,
  `close_session`) and an equivalent plain **JSON API**.

If the LLM's machine runs `sshd`, the agent can tunnel over SSH and the relay
never needs a public address — see [Deployment topologies](#deployment-topologies).

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full design.

TunnelHW is designed as a **self-hostable open-source system**: you run the
agent on your machine and the relay on your own server. Nothing in the code
assumes a particular machine or operator — all endpoints, ports, and
credentials are configuration. (The repo is private during early development
and will be opened up.)

## Status

Working end to end, including against real hardware. Prebuilt binaries for
Windows, Linux, and macOS are on the
[releases page](https://github.com/chaugan/tunnelhw/releases) — you do not need
to build from source.

The architecture was independently reviewed by a three-model panel
(Codex / Grok / Kimi); see
[docs/DESIGN-REVIEW-2026-08-05.md](docs/DESIGN-REVIEW-2026-08-05.md) for the
adjudication, and the component code went through a second security +
correctness review round.

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

Download the two binaries you need from the
[releases page](https://github.com/chaugan/tunnelhw/releases) — or build them
all yourself (needs Go ≥1.25; output lands in `dist/`):

```bash
scripts/build.sh 0.2.4
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

**3. Give the LLM access.** The MCP server is built into the relay — there is
nothing extra to install or run. See the next section.

macOS note: cross-compiled darwin binaries enumerate ports with degraded
metadata (no USB serial numbers — macOS needs cgo/IOKit for that); build
natively on a Mac with `CGO_ENABLED=1` for full fingerprinting.

## Connecting the LLM

### Register the MCP server

Point your MCP client at the relay's `/mcp` endpoint with the bearer token from
`api-token`. Because the relay runs on the LLM's own machine, that URL is
usually loopback.

Claude Code:

```bash
claude mcp add tunnelhw --transport http http://127.0.0.1:8443/mcp \
  --header "Authorization: Bearer <api token>"
```

Any client that takes a JSON config:

```json
{
  "mcpServers": {
    "tunnelhw": {
      "type": "http",
      "url": "http://127.0.0.1:8443/mcp",
      "headers": { "Authorization": "Bearer <api token>" }
    }
  }
}
```

**Restart the LLM session afterwards.** MCP servers are loaded when a session
starts — registering one mid-session does not make its tools appear.

### Verify it worked

Ask the LLM to list devices. It should name your exposed device by its word ID.
To check independently of the LLM:

```bash
curl -s -H "Authorization: Bearer <api token>" http://127.0.0.1:8443/api/v1/devices
```

An empty `{"devices":[]}` with the agent connected means nothing has been
toggled **Exposed** in the agent's web UI yet — that is the usual cause.

### What the LLM can do

No prompting or instructions are needed: the tool descriptions tell the model
the semantics, including the rules below.

| Tool | Purpose |
|---|---|
| `list_devices` | word ID, transport, online/busy, fingerprint confidence |
| `open_device` | claim a device, returns a `session_id` (baud etc. optional) |
| `read` | bounded read: `timeout_ms`, `max_bytes`, optional `delimiter` |
| `write` | send data, `utf8` or `base64` |
| `set_params` | change baud / toggle DTR / RTS mid-session |
| `drain` | wait for buffered output to reach the hardware |
| `close_session` | release the device |

Rules worth knowing as the operator:

- **Devices are exclusive.** One session at a time; a second `open_device`
  fails with a `busy` error naming the holder.
- **Reads never block indefinitely** and may return partial data — the model is
  told to check `timed_out` and read again.
- **Sessions do not survive a reconnect.** If the agent's tunnel drops, session
  IDs become invalid and nothing is replayed; the model re-opens.
- **Control lines are a separate grant.** `set_params` (baud, DTR, RTS) is
  refused unless you enable **Control lines** for that device in the web UI,
  because toggling DTR/RTS can reset a board or drop it into its bootloader.
- **Read-only tokens** (`api-token --read-only`) allow `list_devices` and
  `read` only — useful for a monitoring LLM.

### If your LLM does not speak MCP

Every capability is available as a plain JSON API under `/api/v1/` with the same
bearer token. A full round trip:

```bash
TOKEN=<api token>; B=http://127.0.0.1:8443/api/v1

# 1. find the device
curl -s -H "Authorization: Bearer $TOKEN" $B/devices

# 2. open it, keeping the session id
SID=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"device_id":"amber-falcon","params":{"baud":115200}}' $B/sessions \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["session_id"])')

# 3. write, then read (text is present when the bytes are valid UTF-8)
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"data":"hello\r\n"}' $B/sessions/$SID/write
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"timeout_ms":3000,"max_bytes":4096}' $B/sessions/$SID/read

# 4. always close
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" $B/sessions/$SID
```

### Notes from real use

- **A silent device is normal.** Plenty of firmware prints nothing until it is
  prompted or reset. If the device is granted control lines, a DTR/RTS pulse
  produces a boot log — on an ESP32 the reset reason (`rst:0x15
  USB_UART_CHIP_RESET`) even confirms the control line reached the chip.
- **Weak fingerprints move.** A board that reports no USB serial number is
  identified by VID:PID plus port path, so replugging it elsewhere can produce
  a *new* word ID. The web UI flags this as weak confidence.
- **Minting a token does not require a relay restart** (since v0.2.1), but
  registering an MCP server does require an LLM session restart.

## Repo layout

```
cmd/agent/          # local agent binary + tunnel lifecycle
cmd/relay/          # relay binary: serve + pair-token/api-token/agents/revoke
internal/agent/     # device registry, exclusive sessions, tunnel client
internal/auth/      # pairing tokens, agent credentials, API tokens (hashed)
internal/config/    # agent config persistence (atomic, 0600)
internal/mcp/       # MCP adapter over the relay API
internal/mux/       # pinned yamux configuration
internal/names/     # curated wordlists + stable word-pair IDs
internal/proto/     # control protocol: framing, versioning, correlation IDs
internal/relay/     # hub: agent tunnels, device announces, stream brokerage
internal/relayapi/  # the core API + its HTTP surface (MCP maps onto this)
internal/serialdev/ # enumeration, tiered fingerprinting, port I/O
internal/sshtun/    # SSH carrier: known_hosts policy, ssh-agent, ssh_config
internal/webui/     # localhost web UI handlers + hardening
internal/e2e/       # end-to-end tests, incl. an in-process sshd
web/                # zero-build UI assets (go:embed)
docs/               # architecture & design review
```

## Development

```bash
export PATH=/path/to/go/bin:$PATH
go test -race ./...        # includes a hardware-free end-to-end test
scripts/build.sh <version> # cross-compile everything into dist/
```

The end-to-end tests wire the real agent to the real relay over a live
WebSocket with an in-memory serial device, and a second suite runs the same
path through an in-process SSH server — so the full pipeline is testable
without any hardware.
