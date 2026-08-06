# TunnelHW

[![CI](https://github.com/chaugan/tunnelhw/actions/workflows/ci.yml/badge.svg)](https://github.com/chaugan/tunnelhw/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Latest release](https://img.shields.io/github/v/release/chaugan/tunnelhw)](https://github.com/chaugan/tunnelhw/releases)

**Plug a serial device into your machine and let an LLM on another machine use
it, as if it were plugged in there.**

You choose which devices are exposed, from a web UI on your own machine.
Nothing is exposed by default.

- **No public address needed.** If the LLM's machine runs `sshd`, the agent
  dials out over SSH and the relay listens on loopback only. No open ports, no
  certificates, works from behind NAT. (A direct TLS connection is also
  supported; see [Deployment topologies](#deployment-topologies).)
- **Every device gets a readable name** like `amber-falcon`, stable across
  replugs. That is the handle the LLM uses.
- **The LLM talks to it over MCP** (`list_devices`, `open_device`, `read`,
  `write`, `set_params`, `drain`, `close_session`), or a plain JSON API if it
  doesn't speak MCP.
- **Serial first:** USB adapters, native COM ports and UARTs, PCI serial
  cards, RS-232/485, Bluetooth SPP.
- **Foreground or background:** run either binary directly, or install it as a
  Windows service / systemd unit / launchd agent; see
  [Running as a service](#running-as-a-service).

Prebuilt binaries for Windows, Linux and macOS are on the
[releases page](https://github.com/chaugan/tunnelhw/releases). Design notes are
in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md); the security model is in
[SECURITY.md](SECURITY.md).

---

## Quickstart

Two binaries: the **agent** on the machine with the hardware, the **relay** on
the machine running the LLM. This walks the recommended SSH setup, which needs
no public address anywhere.

### 1. Relay (on the machine that runs the LLM)

`serve` runs in the foreground, so use two terminals (or install it as a
service; see [Running as a service](#running-as-a-service)).

```bash
# Terminal 1: loopback only, so not reachable from the network. SSH provides
# the encryption, and the relay refuses plaintext on any other address.
./tunnelhw-relay-linux-amd64 serve --listen 127.0.0.1:8443 --insecure-dev
```

```bash
# Terminal 2: mint the two secrets you need
./tunnelhw-relay-linux-amd64 pair-token                  # single use, 5 min
./tunnelhw-relay-linux-amd64 api-token --name llm-host   # bearer token for the LLM
```

> **`--insecure-dev` turns TLS off.** It is permitted *only* on a loopback
> address, where SSH or a local proxy supplies the encryption. The relay
> refuses to start with it on any other address. For a directly reachable
> relay use `--tls-cert` and `--tls-key`. See [SECURITY.md](SECURITY.md).

### 2. Agent (on the machine with the hardware)

```powershell
.\tunnelhw-agent-windows-amd64.exe      # or the linux/darwin binary
```

Open <http://127.0.0.1:8787> and:

1. Choose **Through SSH**, enter the LLM machine's SSH host and username, and a
   private key (prefer a key or `ssh-agent`; a password also works). Leave the
   relay URL blank, since it defaults to the relay on the SSH host's loopback.
   `~/.ssh/config` is honoured, so a short alias resolves as it does in your
   terminal.
2. Paste the pairing token. Verify the SSH host-key fingerprint when prompted.
   It is recorded, and a *changed* key is refused from then on.
3. Toggle **Exposed** on the devices the LLM may use. Each gets its word ID.

![The TunnelHW agent's localhost control panel: relay status, the device list
with per-device Exposed and Control-lines toggles, live sessions, and the
activity log](docs/images/web-ui.png)

Everything the LLM can reach is decided here: a device the LLM may use is one
you ticked. The activity log records every open, close and control-line change.
Note that byte counts are logged, never payloads.

### 3. Connect the LLM

The MCP server is built into the relay, so there is nothing extra to install. See
[Connecting the LLM](#connecting-the-llm).

---

## Connecting the LLM

### Register the MCP server

Point your MCP client at the relay's `/mcp` endpoint with the `api-token`
bearer. The relay runs on the LLM's own machine, so this is usually loopback.

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

**Restart the LLM session afterwards.** MCP servers load when a session
starts, so registering one mid-session does nothing.

### Verify it

Ask the LLM to list devices; it should name your exposed device. Independently
of the LLM:

```bash
curl -s -H "Authorization: Bearer <api token>" http://127.0.0.1:8443/api/v1/devices
```

An empty `{"devices":[]}` with the agent connected means nothing is toggled
**Exposed** yet, which is the usual cause.

### Rules worth knowing as the operator

The tool descriptions teach the model these; you need them to predict its
behaviour.

| Tool | Purpose |
|---|---|
| `list_devices` | word ID, transport, online/busy, fingerprint confidence |
| `open_device` | claim a device, returns a `session_id` |
| `read` | bounded read: `timeout_ms`, `max_bytes`, optional `delimiter` |
| `write` | send data, `utf8` or `base64` |
| `set_params` | change baud / toggle DTR / RTS mid-session |
| `drain` | wait for buffered output to reach the hardware |
| `close_session` | release the device |

- **Devices are exclusive.** One session at a time; a second `open_device`
  fails with a `busy` error naming the holder.
- **The port is only held during a session.** When no session is open the agent
  holds nothing, so your own terminal or IDE can use the device normally.
- **You can take a device back at any time.** **Release** in the web UI
  force-closes the session holding one device, leaving the tunnel and every
  other device untouched. Hiding a device also ends its session. The **kill
  switch** stops everything and latches off until you press Reconnect.
- **Reads never block indefinitely** and may return partial data.
- **Sessions do not survive a reconnect.** Session IDs become invalid and
  nothing is replayed; the model re-opens.
- **Control lines are a separate grant.** `set_params` is refused unless you
  enable **Control lines** for that device, because DTR/RTS can reset a board
  or drop it into its bootloader.
- **A vanished device ends its session.** If the board is unplugged, or
  re-enumerates after a firmware flash, the session closes and reads report
  `eof`. Without that, reads return nothing for ever, which is impossible to
  tell apart from a device that simply has nothing to say.
- **Monitoring keeps the port open.** Some hardware resets when its port is
  opened, and the agent cannot always prevent that: on Windows the port is
  opened by the OS before any line settings apply, so a board whose reset is
  wired to DTR reboots regardless. Enable **Monitor** for such a device and the
  agent opens the port once and holds it. Sessions then attach instead of
  reopening, so the reset happens when monitoring starts and never again. It
  also records recent output, so anything the device says between sessions is
  replayed to the next one rather than lost.
- **Opening a device does not touch its control lines.** DTR and RTS stay low,
  so simply reading from a board wired for auto-reset (every ESP32 and Arduino
  dev board) will not reboot it. Some USB-CDC firmware stays silent until the
  host raises DTR; for those, enable **DTR on open** for that device.
- **Read-only tokens** (`api-token --read-only`) cannot open a session, and
  sessions are visible only to the token that opened them, so read-only is in
  practice *list-only*, good for inventory rather than watching live traffic.
  `api-token --agents <id,…>` restricts a token to named agents.

### Without MCP

Every capability is a plain JSON API under `/api/v1/` with the same token:

```bash
TOKEN=<api token>; B=http://127.0.0.1:8443/api/v1

curl -s -H "Authorization: Bearer $TOKEN" $B/devices

SID=$(curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"device_id":"amber-falcon","params":{"baud":115200}}' $B/sessions \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["session_id"])')

curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"data":"hello\r\n"}' $B/sessions/$SID/write
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"timeout_ms":3000,"max_bytes":4096}' $B/sessions/$SID/read

curl -s -X DELETE -H "Authorization: Bearer $TOKEN" $B/sessions/$SID
```

---

## Running as a service

Both binaries run in the foreground by default: just launch them. Either can
instead be installed as a background service using the platform's own service
manager: **Windows services**, **systemd**, or **launchd**. Same subcommands
everywhere.

```bash
# The flags you give to "install" are recorded and replayed on every start.
tunnelhw-relay service install --listen 127.0.0.1:8443 --insecure-dev
tunnelhw-relay service start

tunnelhw-agent service install
tunnelhw-agent service start

# also: stop, restart, status, uninstall
tunnelhw-agent service status
```

Installed **for the current user** where the platform supports it (systemd
`--user`, launchd LaunchAgent); add `--system` for a system-wide service.
Running any service command under `sudo` implies `--system`, since a per-user
service owned by root is never what's intended. You do not need to repeat the
scope on later commands: `start`, `status` and `uninstall` find whichever
service is actually installed.

### Where credentials live

The relay's credential store follows the same split, automatically:

| How you run it | Store |
|---|---|
| as yourself | `~/.config/tunnelhw-relay` (or the OS equivalent) |
| under `sudo`, as root, or as a service | `/var/lib/tunnelhw-relay` · `/Library/Application Support/tunnelhw-relay` · `%ProgramData%\tunnelhw-relay` |

So `sudo tunnelhw-relay pair-token` and a system-scoped service agree on one
store without any flags. Every command prints which store it is using, and
warns if that store is empty while the other one has credentials, the failure
mode being minting tokens the running relay will never see.

If you move between scopes and want to keep existing pairings and tokens, copy
the store across (`service install` prints this command when it detects the
situation):

```bash
sudo mkdir -p /var/lib/tunnelhw-relay
sudo cp ~/.config/tunnelhw-relay/auth.json /var/lib/tunnelhw-relay/
sudo chmod 600 /var/lib/tunnelhw-relay/auth.json
```

### Prefer a user service for the agent

The agent resolves its config directory, `~/.ssh/known_hosts`, `~/.ssh/config`
and the ssh-agent from the user's environment. A service running as root or
LocalSystem looks in a different home directory, so it starts with **no
pairing** and cannot see your SSH keys.

Consequences when you do run the agent as a system service:

- It needs its own **pairing token**, because its config directory is empty.
- The same board gets a **different word ID**, because the `fingerprint → name`
  map lives in that config directory.
- You are asked to approve the SSH **host key** again, for the same reason.
- Give the SSH key as an **absolute path**; the ssh-agent is not reachable
  from another account.

Platform notes, which `service install` also prints:

| Platform | Notes |
|---|---|
| **Linux** | A `--user` service stops at logout unless you enable lingering: `sudo loginctl enable-linger $USER`. Serial ports usually need `sudo usermod -aG dialout $USER`. Installing over plain SSH may fail with no user D-Bus instance; the error explains the options. |
| **macOS** | Installs a LaunchAgent for your user; `--system` writes a LaunchDaemon. |
| **Windows** | Installing, starting and stopping a service needs Administrator rights, so `service install` **raises a UAC prompt** and re-runs itself elevated; you do not need an Administrator terminal. Windows has no per-user services, so the service runs as **LocalSystem**, whose home directory is not yours. For the agent that means your `~/.ssh` keys and ssh-agent are unavailable, so either pass explicit paths (`--config-dir`, an absolute key path in the web UI) or run the agent in the foreground. |

The service records the binary's current path, so reinstall if you move it.

### If something goes wrong

| Symptom | Cause and fix |
|---|---|
| `command not found` after installing | The binary is not on `PATH`. Use its full path; `service install` prints the exact command to run. |
| `status` says `not installed`, but it is | An older build required `--system` on every action. Upgrade; the scope is now detected. |
| `Init already exists` when installing | The other scope is already installed. Uninstall that one first; the error names its path. |
| Service starts, then rejects your agent and tokens | It is reading a different credential store. Check the path each command prints, and copy the store across if needed. |
| Agent service runs but cannot authenticate over SSH | It is not running as you, so `~/.ssh` is not yours. Use an absolute key path, or run the agent in the foreground. |
| Linux: install fails over SSH with no user D-Bus | `systemctl --user` needs a login session. Use `sudo … --system`, or `sudo loginctl enable-linger $USER` and reconnect. |

## How it fits together

The recommended setup (**topology A**), where the agent dials out over SSH and
the relay is reachable only on the LLM machine's loopback:

```mermaid
flowchart LR
    subgraph LOCAL["🖥️  YOUR MACHINE (where the hardware is)"]
        direction TB
        HW["🔌 Serial device<br/><i>COM3 · /dev/ttyUSB0</i>"]
        AGENT["<b>TunnelHW agent</b><br/>enumerate · bridge · dial out"]
        WEBUI["🔒 Web UI · 127.0.0.1:8787<br/><i>you choose what is exposed</i>"]
        HW <-->|"serial I/O"| AGENT
        WEBUI -.->|"expose + grants"| AGENT
    end

    subgraph REMOTE["☁️  LLM MACHINE (relay not reachable from the network)"]
        direction TB
        SSHD["🔑 sshd :22<br/><i>the only listening port</i>"]
        RELAY["<b>TunnelHW relay</b><br/>127.0.0.1:8443 · loopback only"]
        LLM["🤖 LLM<br/><i>MCP client, same machine</i>"]
        SSHD -->|"forward → 127.0.0.1:8443"| RELAY
        LLM <-->|"/mcp · bearer token"| RELAY
    end

    AGENT ==>|"<b>outbound</b> SSH · works behind NAT"| SSHD

    classDef box fill:#1f2937,stroke:#4b5563,color:#f9fafb
    classDef safe fill:#064e3b,stroke:#10b981,color:#ecfdf5
    classDef llm fill:#312e81,stroke:#818cf8,color:#eef2ff
    class HW,AGENT,SSHD,RELAY box
    class WEBUI safe
    class LLM llm
```

The one-time setup, then what happens on every device call:

```mermaid
sequenceDiagram
    autonumber
    actor You
    participant UI as Agent web UI
    participant Agent as Agent
    participant Dev as Serial device
    participant Relay as Relay + MCP
    participant LLM as LLM

    Note over You,Relay: ONE-TIME SETUP
    You->>Relay: relay pair-token (single use, 5 min)
    You->>UI: SSH host, user, key + the token
    UI->>Relay: POST /pair (through the SSH tunnel)
    Relay-->>UI: agent_id + long-lived credential
    You->>UI: toggle a device to Exposed
    Agent->>Relay: connect, announce exposed devices only

    Note over LLM,Dev: EVERY DEVICE INTERACTION
    LLM->>Relay: open_device("amber-falcon")
    Relay->>Agent: open stream (device, baud)
    Agent->>Dev: open port, exclusive
    Relay-->>LLM: session_id
    LLM->>Relay: write("hello")
    Relay->>Agent: bytes
    Agent->>Dev: bytes out
    Dev-->>Agent: bytes back
    LLM->>Relay: read(timeout_ms, max_bytes)
    Relay-->>LLM: text / base64
    LLM->>Relay: close_session
    Agent->>Dev: close port, device free again
```

## Deployment topologies

The relay only needs to be reachable **by the agent**. It does not need a
public IP.

| | Setup | When |
|---|---|---|
| **A. SSH carrier** *(recommended)* | Relay on the LLM machine, bound to loopback; agent dials out over SSH. No certificates, nothing listening but `sshd`. | The LLM machine runs `sshd`. |
| **B. Direct** | Relay reachable at `wss://host:8443` with `--tls-cert`/`--tls-key`; agent uses **Direct to relay**. | Same LAN/VPN, or the relay legitimately has a public address. |
| **C. Overlay network** | Both machines on Tailscale/WireGuard; relay on the LLM machine, agent uses its mesh address. | Neither machine is reachable and you would rather not use SSH. |

The agent has an SSH client built in for topology A, so there is no `ssh -L` to
run or keep alive.

## Security

Full detail in [SECURITY.md](SECURITY.md), including how to report a
vulnerability. The essentials:

- **The relay is trusted and self-hosted.** A compromised relay can operate
  every *currently exposed* device. It must see device bytes in clear because
  it serves them to the LLM.
- **Hidden devices are never disclosed to the relay.** Exposure is opt-in per
  device and revocable, and hiding a device ends its session immediately.
- **The API token is a credential for physical access.** Don't commit it; use
  `--read-only` where that suffices; revoke and re-mint if it leaks.
- **Control-line access is granted per device**, because it can reset hardware.
- Serial payloads are never logged; logs carry metadata only.

## Compared to the alternatives

TunnelHW is not a general-purpose tunnel. It is a device control plane for LLMs.

| | What it gives you | What it doesn't |
|---|---|---|
| `ser2net` / RFC2217 | Serial over TCP | No NAT traversal, no selection UI, no LLM interface, no naming |
| `ssh -R` / `frp` / ngrok | Generic port forwarding | A port, not a device: no enumeration, no per-device consent, no session model |
| USB/IP | Full USB passthrough | Kernel drivers, privileged, fragile across platforms; overkill for serial |
| **TunnelHW** | Enumeration, human-readable stable IDs, per-device consent + grants, exclusive sessions, LLM-safe bounded I/O, MCP and JSON APIs | Not multi-tenant, not a hosted service, serial only for now |

## Notes from real use

- **A silent device is normal.** Plenty of firmware prints nothing until
  prompted or reset. With control lines granted, a DTR/RTS pulse produces a
  boot log. On an ESP32 the reset reason (`rst:0x15 USB_UART_CHIP_RESET`)
  confirms the control line reached the chip.
- **Weak fingerprints move.** A board reporting no USB serial number is
  identified by VID:PID plus port path, so replugging it elsewhere can produce
  a new word ID. The UI flags this as weak confidence.
- **Host key "changed"** on first connect usually means your `known_hosts` has
  a different algorithm's key for that host; TunnelHW retries pinned to the
  recorded algorithms before reporting a change.
- **Minting a token needs no relay restart**, but registering an MCP server
  does need an LLM session restart.
- **macOS cross-compiles** enumerate with degraded metadata (no USB serial
  numbers, since macOS needs cgo/IOKit). Build natively with `CGO_ENABLED=1` for
  full fingerprinting.

## Verifying a release

Release binaries are built by GitHub Actions from a tagged commit and carry a
signed [build provenance attestation](https://docs.github.com/actions/security-guides/using-artifact-attestations),
so you can check they came from this repository's workflow rather than someone's
laptop:

```bash
gh attestation verify tunnelhw-agent-windows-amd64.exe --repo chaugan/tunnelhw
```

`SHA256SUMS` is attested too, so verifying it is as strong as verifying each
binary individually:

```bash
gh attestation verify SHA256SUMS --repo chaugan/tunnelhw
sha256sum -c SHA256SUMS --ignore-missing
```

Windows binaries are **not** code-signed yet, so SmartScreen may still warn on
first run; provenance proves origin, not Authenticode trust.

## Development

```bash
go test -race ./...        # no hardware required
go vet ./...
scripts/build.sh <version> # cross-compile everything into dist/
```

The end-to-end tests wire the real agent to the real relay over a live
WebSocket with an in-memory serial device, and a second suite runs the same
path through an in-process SSH server.

Every push and pull request runs vet, race-enabled tests and a build on
**Linux, Windows and macOS**, plus formatting, module tidiness, a cross-compile
of all release targets, `govulncheck`, and CodeQL. Releases are built and
attested by the same CI, never by hand.

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

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).
