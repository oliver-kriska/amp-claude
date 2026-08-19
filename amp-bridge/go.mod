module amp-bridge

// No dependencies, and that is deliberate — do not add an MCP SDK.
//
// Claude Code negotiates the MCP handshake in two phases and channels only have
// a delivery path for unsolicited notifications on the legacy `initialize` one.
// Every SDK implements `server/discover`, which wins the modern negotiation and
// silently kills channel delivery while every health check still passes. The
// transport here is hand-rolled for that reason. See
// .claude/research/2026-08-19-amp-claude-code-bridge.md §10.
//
// The toolchain is pinned in ../.tool-versions (mise).
go 1.26.6
