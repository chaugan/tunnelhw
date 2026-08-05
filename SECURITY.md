# Security

TunnelHW gives a remote LLM the ability to operate physical hardware. Please
read this before deploying it.

## Reporting a vulnerability

Report privately — **do not open a public issue**. Use GitHub's
[private vulnerability reporting](https://github.com/chaugan/tunnelhw/security/advisories/new)
on this repository.

Please include the version of both binaries, the deployment topology (SSH
carrier / direct TLS / overlay), and a reproduction if you have one. This is a
small project with no dedicated security team; expect a human, not an SLA.

Fixes ship in a normal release with the issue described in the release notes.

## Threat model

**TunnelHW assumes a self-hosted, trusted relay.** You run the relay; it is not
a service anyone else operates for you.

What that means concretely:

- **A compromised relay can operate every *currently exposed* device** — open
  sessions, read, write, and (where granted) toggle control lines. The relay
  must see device bytes in clear because it hosts the MCP server that serves
  them to the LLM. There is deliberately no end-to-end encryption past the
  relay; that would be a different product.
- **Hidden devices are never disclosed.** The expose list lives on the agent,
  and the relay is only told about devices you have explicitly exposed.
- **Anyone with the API/MCP bearer token can drive your exposed hardware.**
  Treat it as a credential for physical access, because that is what it is.
- **A local user on the agent machine can reach the web UI.** The per-launch
  CSRF secret defends against browsers (DNS rebinding, malicious pages), not
  against another process running as you or as root on the same machine. On a
  shared machine, assume any local user can re-point the agent.

Out of scope: multi-tenant relays, untrusted relay operators, and protecting
the hardware from a user who already has an LLM session on it.

## Safe defaults, and how to stay on them

- **Nothing is exposed until you toggle it.** The agent announces only devices
  you have marked Exposed, and hiding a device now also terminates any session
  holding it.
- **Prefer the SSH carrier** (topology A). The relay binds `127.0.0.1`, is
  reachable only through your SSH server, and needs no certificates.
- **`--insecure-dev` disables TLS on the relay.** It is correct *only* when the
  relay is bound to loopback and reached through SSH or from the same machine.

  | Command | Verdict |
  |---|---|
  | `serve --listen 127.0.0.1:8443 --insecure-dev` reached via SSH | fine — SSH provides the encryption |
  | `serve --listen 0.0.0.0:8443 --insecure-dev` | **never** — agent credentials and device traffic cross the network in clear |
  | `serve --listen :8443 --tls-cert … --tls-key …` | correct for a directly reachable relay |

- **Control lines are a separate per-device grant.** Toggling DTR/RTS or
  changing baud can reset a board or drop it into its bootloader, so
  `set_params` is refused unless you enable it for that device.
- **Use the kill switch.** The web UI can release a single device, or sever the
  tunnel and every session at once. Stopping the agent process severs
  everything (fail-closed).

## Credentials

| Credential | Lifetime | Notes |
|---|---|---|
| Pairing token | single use, 5 minutes | shown once; exchanged over TLS/SSH for an agent credential |
| Agent credential | long-lived, revocable | stored `0600` in the agent's config dir; only a hash is kept on the relay |
| API / MCP token | long-lived, revocable | bearer secret; `--read-only` restricts it to `list_devices` and `read` |

- Only hashes are stored relay-side; plaintext exists only at mint time.
- **Do not commit tokens.** MCP client configs often contain them — check
  before pushing dotfiles.
- Tokens typed on a command line land in shell history and are visible in
  `ps`. Prefer a config file or environment variable.
- Mint a **read-only** token for anything that only needs to observe.
- Revoke an agent with `tunnelhw-relay revoke-agent <id>`; re-mint API tokens
  and update clients if one leaks.
- Serial payloads are **never logged** by default — logs carry metadata only
  (device IDs, connection state, byte counts). Enable payload logging only
  deliberately; serial traffic often contains firmware secrets.

## Supported versions

Only the latest release receives fixes. Upgrade before reporting.
