# Development

Building, testing, and the protocol decisions you must not undo.

## Build and test

```bash
make check                # the gate: tidy, skill drift, format, vet, lint, both test tiers under -race
make test                 # fast unit pass
make test-integration     # spawns a real bridge process and drives both ends
make doctor               # diagnose the installed bridge
make help                 # every target
```

Neither test tier needs the network or a live Claude session.

A rebuild does not change what a running session is executing — that is what
`doctor`'s fingerprint check reports. [`AGENTS.md`](../AGENTS.md) carries the full
build and test reference, including the toolchain pins and why the module must
stay dependency-free.

## Three things that look like bugs

They are load-bearing. The reasoning, and how each was found, is in
[the research log](../.claude/research/2026-08-19-amp-claude-code-bridge.md).

1. **`server/discover` is deliberately unimplemented.** Answering it negotiates
   the modern MCP era, which has no delivery path for unsolicited custom
   notifications. Channels then stop working while every health check still
   passes. **This is why the module has no dependencies:** every MCP SDK
   implements `server/discover`, so adding one silently kills the bridge.
2. **The capability is `experimental: {"claude/channel": {}}`**, not a top-level
   capability key.
3. **The notification field is `meta`, not the MCP-standard `_meta`.** Keys must
   be identifiers, values become attributes on the `<channel>` tag, and `source`
   is reserved — Claude sets it itself.
## Further reading

- [`AGENTS.md`](../AGENTS.md) — how Amp should drive the bridge
- [`.claude/skills/amp-bridge/SKILL.md`](../.claude/skills/amp-bridge/SKILL.md) — how Claude should
- [the research log](../.claude/research/2026-08-19-amp-claude-code-bridge.md) — the protocol archaeology, in full