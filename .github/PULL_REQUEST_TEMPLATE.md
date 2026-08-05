## What and why

<!-- What changes, and what problem it solves. Link the issue if there is one. -->

## Checklist

- [ ] Tests added, or a note here on why they aren't (e.g. hardware-specific
      enumeration that `internal/e2e` cannot reach)
- [ ] `go test -race ./...` passes
- [ ] `go vet ./...` passes
- [ ] Docs updated: `README.md` for user-visible changes (flags, MCP tools,
      topologies), `docs/ARCHITECTURE.md` for design decisions
- [ ] Security impact considered: does this widen what the relay, the LLM, or an
      unpaired caller can reach? Does it touch credentials, the expose-list, the
      control-line grant, the SSH host-key policy, or web UI hardening? Say so
      below if yes.

## Testing

<!-- What you ran, on which OS, and against which hardware (if any). -->
