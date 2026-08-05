# Design review — external panel, 2026-08-05

Draft v0.1 of `ARCHITECTURE.md` was reviewed independently by **Codex
(gpt-5.5, high)**, **Grok**, and **Kimi**, per the sparring playbook. This file
records the adjudication: every material finding, accepted or rejected, with a
reason. Accepted findings are folded into `ARCHITECTURE.md` v0.2.

## Unanimous — accepted as-is

| Topic | Panel verdict |
|---|---|
| WSS + yamux over embedded SSH or frp/rathole | All agree. Own the control plane; wss/443 traverses firewalls. |
| Go, single static binary; zero-build embedded web UI (vanilla JS/htmx, no Node build step) | All agree. |
| Exclusive-open per device in v1, with explicit `busy`/`claimed_by` state and deterministic errors | All agree. |
| MCP ships in v1 but as a **thin adapter** over an internal, versioned relay API — never the core protocol | All agree. |
| Serial-first phasing; USB/IP deferred | All agree. |
| Sessions do not survive reconnect; explicit re-open; never buffer/replay writes | All agree (Kimi: replaying serial writes after reconnect is dangerous for hardware state). |
| Nothing exposed by default; expose-list lives on the agent; fail-closed kill switch | All agree — "correct trust split". |
| Monorepo layout | All agree (Grok: add an `internal/relayapi` seam so MCP can't become the business-logic package — accepted). |

## Accepted findings (with source)

1. **No "yamux stream 0"** (Codex 1, Grok 7). Yamux allocates odd/even stream
   IDs; a reserved stream 0 doesn't exist. → First stream opened after session
   start is the control stream, by convention; all control messages carry
   **correlation IDs** (Grok 8).
2. **Device-open flow** (Grok 8): don't pre-announce a `stream_id` on the
   control channel. The consumer side opens a new yamux stream whose **first
   frame is the JSON open-header**; the response frame is `open_ok`/`open_err`;
   then raw bytes. Self-contained, no cross-channel race.
3. **Protocol lifecycle must be explicit** (Codex 10, Grok 22, Kimi 1): version
   negotiation in `hello` (agent sends max, relay picks, floor enforced —
   Kimi 12); heartbeats; open timeout; half-close semantics; backpressure;
   caps on concurrent opens, write rate, buffer sizes, control-message size.
   Pin yamux keepalive/window config explicitly, never defaults.
4. **Pairing token lifecycle** (Codex 7, Grok 16, Kimi 7 — unanimous): the
   pasted pairing token is **single-use and short-lived (~5 min)**; it is
   exchanged over verified TLS for a per-agent credential; the relay stores
   only a **hash**; revoke/rotate supported; OS keychain where available;
   never logged.
5. **TLS policy stated** (Grok 17): system-CA (or pinned cert) verification
   mandatory; `ws://` only behind an explicit `--insecure-dev` flag.
6. **Localhost UI hardening** (Codex 8, Grok 18, Kimi 6 — union): bind
   `127.0.0.1` exactly (not `localhost`/dual-stack); allowlist `Host:`;
   reject non-local `Origin`; CSRF token; per-launch random session secret;
   `CSP: default-src 'self'`; no wildcard CORS.
7. **Word-ID policy** (conflict resolved): Codex/Kimi wanted ≥1M-combination
   lists; Grok argued ~65k is fine because IDs are **scoped per agent** (a
   machine has tens of devices, not thousands) and exhaustion is a
   multi-tenant naming problem. **Grok's scoping analysis accepted** as the
   primary fix: word-IDs are unique *per agent*; the relay namespaces by
   `agent_id` when more than one agent is paired. Codex's cheap additions also
   accepted: collision → append a third word; a durable internal UUID backs
   every device so the word-ID stays a presentation-layer handle. Wordlists
   curated (no slurs/brands/near-duplicates like gray/grey — Grok 5).
8. **Fingerprinting honesty** (Codex 12, Grok 12, Kimi 9 — unanimous): prefer
   USB serial number; fallback VID:PID + USB port-path is *weak* — surface a
   confidence level in the UI, warn, and offer user override. Never
   fingerprint on bare OS path (`/dev/ttyUSB0`, `COM3`).
9. **Per-device capability grants** (Codex 9, Grok 21): control-line toggles
   (DTR/RTS) and baud changes can reset boards / enter bootloaders. Separate
   per-device "allow control lines" grant, loud in the activity log.
10. **MCP tool guardrails** (Codex 13, Grok 10): `read` takes
    timeout + max_bytes + optional delimiter; `write`/`write_line`; base64
    mode for binary; explicit `close`; `set_params`; `drain`; document
    partial reads and session-reset-on-reconnect in the tool descriptions.
11. **Credential separation** (Codex 15, Kimi 13, Grok 20): the LLM-host MCP
    token and the agent credential are different principals — never shared,
    separate middleware; MCP token record carries scopes
    (agents/devices/read-only) from day one, enforced simply in v1.
12. **No payload logging** (Kimi 15): logs carry metadata only (IDs, state,
    byte counts). Serial payloads logged only under an explicit local debug
    toggle with a warning.
13. **Threat-model honesty** (Grok 19, Codex 6): v1 is **self-hosted,
    relay-trusted**. A compromised relay can operate any *currently exposed*
    device. Stated plainly in the doc; mitigations are a minimal expose-list,
    kill switch, short-lived tokens, capability grants, and no payload logs.
14. **v2 TCP class = SSRF risk** (Grok 28): designed now — LAN targets are
    user-allowlisted CIDR:port entries; link-local/metadata ranges blocked by
    default.
15. **Product boundary** (Grok 26): TunnelHW is *inspired by* reverse tunnels
    but is **not** a general-purpose tunnel — the device-session control plane
    is the product. Written into the doc to prevent frp-feature creep.
16. **Library choices** (Kimi 1, Grok 25): `coder/websocket` (context-native)
    over gorilla; minor, accepted.

## Rejected findings (with reason)

1. **Kimi 6 — end-to-end encrypt device streams so a compromised relay cannot
   read/inject I/O.** Rejected for v1. The relay *hosts the MCP server*, which
   must read and write device bytes to serve the LLM — E2E past the relay
   requires moving key material to the LLM host and is, as Grok put it, "a
   different product (and much harder)". Kimi's specific construction
   (deriving the stream key from the pairing token) is additionally weak — the
   relay sees the pairing token at enrollment. Instead: honest trusted-relay
   threat model (accepted finding 13). Revisit only if a multi-tenant public
   relay ever becomes a goal.
2. **Codex 5 / Kimi 5 — grow wordlists to ≥1M combinations as a requirement.**
   Partially rejected: per-agent scoping (accepted finding 7) removes the
   exhaustion scenario; ~65k curated combos plus third-word fallback is ample
   for tens of devices per agent. Curating 2×1024 *good* words costs real
   effort for no v1 benefit.

## Panel health

All three partners responded on the first attempt; no failures, no dropped
members. Codex ran read-only sandboxed; Grok via the plugin subagent
(read-only); Kimi via `-p --output-format stream-json` with no auto-approve
flags.
