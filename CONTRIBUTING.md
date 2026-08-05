# Contributing to TunnelHW

TunnelHW exposes local serial hardware to an LLM running somewhere else. It is
two binaries — an **agent** on the machine with the hardware, and a **relay** on
the machine with the LLM — plus an MCP adapter hosted in the relay process.
Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) before proposing anything
structural; it records what was decided and why, including the choices that were
deliberately rejected.

**Security issues do not go in the issue tracker.** Follow
[SECURITY.md](SECURITY.md) instead.

## Build and test

Go ≥ 1.25.1 (see `go.mod`). No Node toolchain, no code generation, no `make` —
the web UI is vanilla JS embedded with `go:embed`. If Go is not on your `PATH`
(on the primary dev machine it lives in `your Go installation`), export it
first:

```bash
export PATH=/path/to/go/bin:$PATH

go test -race ./...        # full suite, no hardware needed
go vet ./...
scripts/build.sh <version> # cross-compiles every target into dist/
```

`scripts/build.sh` builds agent and relay for windows/linux/darwin on amd64 and
arm64 with `CGO_ENABLED=0`. It embeds Windows version resources if `go-winres`
is on your `PATH` and warns if it is not — the build still succeeds without it.
Run it before sending a change that touches platform-specific code, since
`go build` on your own machine only covers one of eleven targets.

### You do not need serial hardware

The full pipeline is testable without plugging anything in. `internal/e2e` wires
the **real** agent (core + tunnel) to the **real** relay (hub + broker) over a
live WebSocket, with an in-memory serial port standing in for the device; a
second suite in the same package runs that same path through an in-process SSH
server with generated host keys, covering the SSH-carrier topology. If you break
the protocol, the announce path, session exclusivity, or the SSH transport,
these tests fail on a laptop with no devices attached.

Hardware is still worth using when you touch `internal/serialdev`
(enumeration and fingerprinting are OS-specific and cannot be faked
convincingly). Say in the PR what you tested against and on which OS.

macOS caveat: cross-compiled darwin binaries fall back to degraded enumeration
(no USB serial numbers — that needs cgo/IOKit). `internal/serialdev` has both
paths behind build tags; check which one your change affects.

## Package layout

```
cmd/agent/          agent binary + tunnel lifecycle
cmd/relay/          relay binary: serve + pair-token/api-token/agents/revoke
internal/agent/     device registry, exclusive sessions, tunnel client
internal/auth/      pairing tokens, agent credentials, API tokens (hashed)
internal/config/    agent config persistence (atomic, 0600)
internal/mcp/       MCP adapter — a thin mapping onto relayapi, no logic
internal/mux/       pinned yamux configuration
internal/names/     curated wordlists + stable word-pair IDs
internal/proto/     control protocol: framing, versioning, correlation IDs
internal/relay/     hub: agent tunnels, device announces, stream brokerage
internal/relayapi/  the core API + its HTTP surface
internal/serialdev/ enumeration, tiered fingerprinting, port I/O
internal/sshtun/    SSH carrier: known_hosts policy, ssh-agent, ssh_config
internal/webui/     localhost web UI handlers + hardening
internal/e2e/       end-to-end tests, incl. an in-process sshd
web/                zero-build UI assets (go:embed)
```

Two layering rules matter more than the rest:

- **`internal/relayapi` is the business logic.** `internal/mcp` and the JSON API
  are both adapters over it. Never add behaviour that only one surface gets — if
  MCP and `/api/v1/` disagree about what a device does, that is a bug.
- **Agent and relay credentials are separate principals.** Agent enrollment
  (`/pair`) and LLM-host bearer tokens go through different middleware on
  purpose. Do not merge them for convenience.

The relay's threat model assumes it is trusted and self-hosted. Changes that
quietly widen what the relay can do to a device — or what an LLM can do without
an explicit grant — need to be argued for, not just implemented.

## Code style

- **Godoc comments on every exported item**, starting with the identifier's
  name. Each package has a package comment on one file saying what the package
  is for.
- **Table-driven tests** with named cases and `t.Run`, as in
  `internal/mcp` and `internal/webui`. New tests must pass under `-race`; use
  contexts and timeouts rather than sleeps.
- **Comments explain constraints, not narration.** Say why the yamux window is
  pinned, why `127.0.0.1` and not `localhost`, why writes are never replayed
  across a reconnect. Do not restate what the next line does.
- Standard `gofmt` (tabs, no line-length rule). Wrap errors with `%w` and
  context; don't log-and-return the same error twice.
- No new dependencies without a reason in the PR description. The dependency
  list is short deliberately, and the single-static-binary install story is a
  feature — nothing that requires cgo, a runtime, or a build toolchain.
- Serial payloads never reach the logs. Logs carry metadata only: device IDs,
  connection state, byte counts.

## Commits and pull requests

- One logical change per PR. Split refactors away from behaviour changes so
  reviewing the behaviour change is possible.
- Imperative subject line, ≤72 chars, no trailing period
  (`relay: reject changed SSH host keys on reconnect`). Explain the *why* in the
  body when it isn't obvious from the diff.
- `go test -race ./...` and `go vet ./...` must pass before you open the PR.
- Update the docs in the same commit: `README.md` for anything user-visible
  (flags, tools, topologies), `docs/ARCHITECTURE.md` for design decisions.
- Protocol changes in `internal/proto` need a version-negotiation story — an
  older agent talking to a newer relay must fail loudly, not corrupt a session.
- Open an issue first for anything large or architectural. A rejected 800-line
  PR helps nobody.

## Good first areas

- **More device classes.** The stream bridge is device-class-agnostic by
  design. Raw TCP endpoints are the planned v2 class (with the SSRF allowlist
  described in the architecture doc); USB/IP passthrough is v3.
- **Platform testing.** macOS native builds with `CGO_ENABLED=1`, Windows
  ARM64, native COM ports and PCI/PCIe serial cards, Bluetooth SPP virtual
  ports. Reports that a real device enumerates and opens correctly are useful
  even without a code change.
- **Fingerprinting.** The strong/medium/weak tiering in `internal/serialdev` is
  where identity stability comes from; better heuristics for devices that report
  no USB serial number directly improve whether a word ID survives a replug.
- **Docs.** Setup walkthroughs for topologies B and C, and for MCP clients other
  than Claude Code.
- **Web UI.** Small, self-contained, zero-build — a good place to start without
  learning the whole protocol first.

## License

TunnelHW is MIT licensed. Contributions are accepted under the same license.
