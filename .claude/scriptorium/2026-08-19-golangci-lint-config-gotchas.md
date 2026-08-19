---
scriptorium: true
action: append
title: "golangci-lint v2 Production Configuration"
type: pattern
domain: general
tags: [go, linting, golangci-lint, build-tags, misspell, noctx]
---

## Two settings that bite (added 2026-08-19)

Found while applying this config to a Go MCP server (`amp_claude/amp-bridge`).
Both fail quietly, which is what makes them worth recording.

### `misspell` must not be set to the UK locale in protocol code

`locale: UK` rewrites `initialize` → `initialise`. In a codebase that implements
a wire protocol, those are **method names**, not prose: JSON-RPC `initialize`,
MCP `notifications/initialized`. Accepting the linter's advice silently breaks
the handshake; suppressing it per-line litters the dispatch table.

Leave `locale` unset. `misspell` still catches genuine typos and accepts both
spellings, which is the right behaviour for British prose wrapped around
US-spelled protocol identifiers.

### `run.build-tags` — otherwise the tagged tier is never linted

With [[Build-Tag-Based Test Pyramid]], integration tests carry `//go:build
integration`. golangci-lint respects build constraints, so by default it does
not compile — and therefore never lints — any of them. The tier looks clean
because it was never examined.

```yaml
run:
  build-tags:
    - integration
```

Same applies to `go vet`: it needs `go vet -tags=integration ./...` as a separate
invocation. A `make check` target should run both.

### `noctx` is right about Unix sockets

`noctx` flags `net.Dial`, `net.Listen` and `net.DialTimeout` even for
`unix`-network calls, which reads as a false positive. It is not worth
suppressing: switching to `(&net.Dialer{}).DialContext` and
`(&net.ListenConfig{}).Listen` is a two-line change and gives the client a real,
cancellable connect timeout it did not previously have.

### Validate a lint/refactor pass against the harness you are replacing

When a lint-driven refactor is large enough to touch wire behaviour, keep the old
test harness until after the refactor, run it against the rebuilt binary, and
delete it only once it passes. The harness knows nothing about the new structure,
which is exactly what makes it a trustworthy oracle. Deleting it first turns a
regression into an unfalsifiable claim.
