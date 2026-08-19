# Amp ↔ Claude Code bridge — Amp-side research and synthesis

**Date:** 2026-08-19  
**Local versions:** Amp `0.0.1787112648-gd35a66`; Claude Code `2.1.235`; Node `25.4.0`; macOS  
**Companion research:** [`2026-08-19-amp-claude-code-bridge.md`](./2026-08-19-amp-claude-code-bridge.md)

## Conclusion

Build the first robust bridge on Claude Code's official **Channels** API, not by impersonating a
Claude peer in the private cross-session registry.

Channels are specifically documented for pushing external events and chat messages into an open
Claude Code session. They support two-way chat by pairing inbound channel notifications with an MCP
reply tool. That removes the two brittle parts of the socket approach: synthesizing a peer identity
and recovering replies from an internal envelope.

The tradeoff is that a channel must be enabled when Claude Code starts. It cannot be attached to the
currently running session without restarting it. The existing conversation should be resumable with
the channel flag, but that exact combined launch should be verified during the prototype rather than
assumed.

## What the Claude-side research corrected

The earlier statement that Claude Code's messaging socket is only an inbox is incomplete. Between
real Claude Code sessions, cross-session messaging is symmetric:

- sessions discover one another by name;
- `SendMessage` carries sender/reply identity;
- an idle receiver starts a turn and a busy receiver queues the message;
- the receiver can reply to the sender through the same peer mesh.

The official documentation confirms those product semantics. The local registry also confirms that
the current machine has a working mesh. `claude agents --json` is a supported scripting command and
currently reports the target session as:

```text
name:       amp-claude-a6
pid:        52527
session:    b959f5dd-fee6-43f6-bdba-20978fb09aa6
cwd:        /Users/oliverkriska/Projects/amp_claude
```

The distinction is that the documented **external script** contract is still an ingress contract for
a session's own socket. The official docs do not specify:

- the peer key-file schema;
- an external-client sender envelope;
- synthetic peer registration;
- delivery receipts as a public protocol;
- compatibility guarantees for `peerProtocol: 1` registry entries.

The reverse-engineered registry and key-file findings are credible observations of Claude Code
`2.1.235`, but they are private implementation details. The synthetic peer round trip—the one test
that would prove Claude accepts and replies to a non-Claude registry entry—was blocked and remains
unverified.

## A second correction: inbound defaults

`crossSessionInbound` should still be set explicitly for deterministic unattended peer messaging,
but the default is more nuanced than "prompting or plan mode always holds."

With no explicit setting, Claude Code compares the sender and receiver's permission-mode classes:

- same class → deliver;
- different classes → hold for approval.

The two classes are roughly "bypasses permission prompts" and "prompts for permissions." The exact
classification has special cases (including plan mode when bypass is available). A synthetic client
that cannot assert a recognized class may be held. Channels do not use this peer-inbound gate; they
use explicit per-session channel opt-in and sender gating instead.

## The supported two-way path: a custom Channel

Claude Code's own cross-session guide says to use **Channels** for external events. A channel is a
session-owned MCP subprocess connected to Claude Code over stdio.

Inbound:

```text
Amp plugin ──local authenticated IPC──▶ channel MCP server
                                        │
                                        └─ notifications/claude/channel
                                           {content, meta}
```

Claude receives an event resembling:

```text
<channel source="amp_bridge"
         amp_thread_id="T-..."
         request_id="...">
message text
</channel>
```

Outbound:

```text
Claude ──MCP reply tool──▶ channel server ──same pending IPC request──▶ Amp tool result
```

The channel server declares:

```ts
capabilities: {
  experimental: { "claude/channel": {} },
  tools: {}
}
```

It emits `notifications/claude/channel` for Amp messages and exposes a normal MCP `reply` tool.
`amp_thread_id` and `request_id` are application metadata, not hidden Claude protocol fields. They
provide explicit routing and correlation.

### Why this maps cleanly to an Amp plugin

Amp's supported plugin API provides all the synchronous request/reply pieces:

- `amp.registerTool(...)` for `list_claude_sessions` and `ask_claude`;
- `ToolExecutionContext.thread.id` for the originating Amp thread;
- an async tool execute handler that can hold a local IPC request until Claude calls `reply`;
- `PluginThread.appendUserMessage(...)` and `waitForResponse(...)` for a later proactive
  Claude-to-Amp path.

The channel server owns the local listener because its lifecycle already matches the Claude session.
The Amp plugin is a client for ordinary request/reply and does not need to run a long-lived callback
server in version one.

### Discovery without private Claude registry parsing

`claude agents --json` explicitly supports non-TTY scripting and returns each live session's `pid`,
`sessionId`, `name`, `cwd`, and `status`. Use that for the user-facing session list.

Each channel subprocess should publish a small bridge-owned registry entry under a private directory,
keyed by the parent Claude PID and containing its Unix-socket endpoint and heartbeat. The Amp plugin
joins this with `claude agents --json` to show which sessions are channel-ready. This avoids depending
on Claude's private `.key` schema or pretending to be an interactive Claude process.

## Proposed stages

### Preconditions verified after the Claude-side revision

- This account resolves to a personal Claude Max organization. The official Channels documentation
  says Pro and Max users skip the `channelsEnabled` organization-policy check, so the unset local
  setting is not a blocker on this machine.
- Keep the channel on the official TypeScript SDK pattern:
  `Server.connect(new StdioServerTransport())`. That direct connection remains on the legacy,
  initialize-based bidirectional protocol and supports unsolicited `notifications/claude/channel`.
  Do not switch the prototype to the modern/dual-era `serveStdio()` entry point. The official
  fakechat, Telegram, Discord, and iMessage channels neither pin a protocol version nor set
  `MCP_PROTOCOL_NEGOTIATION`.
- The protocol-era warning in Claude Code is real, but it is a guard against a channel-capable server
  arriving over a connection without a custom-notification delivery path. Following the official
  direct-stdio construction avoids that failure; it does not require an extra negotiation setting.
- `claude agents --json` is working locally and currently returns the documented fields (`pid`,
  `sessionId`, `name`, `cwd`, `kind`, `status`, and `startedAt`), so it remains the supported session
  discovery source.

### Stage 1 — request/reply prototype (recommended first)

Build one source package containing:

1. an Amp plugin with `list_claude_sessions` and `ask_claude` tools;
2. a custom Claude channel MCP server;
3. a project `.mcp.json` entry for local development;
4. user-owned Unix-socket IPC with restrictive permissions;
5. correlation IDs, timeouts, cancellation, and bounded pending-request state.

Start or resume Claude with the custom channel development flag:

```text
claude --dangerously-load-development-channels server:amp-bridge
```

Custom channels are research preview. The flag bypasses only Anthropic's channel allowlist; it still
shows a confirmation prompt and still obeys organization policy. The MCP server also requires the
normal first-use project consent.

Stage 1 deliberately supports Amp-initiated conversations only: Amp sends a request, Claude calls the
reply tool, and the waiting Amp tool returns the text. This is enough to prove the transport without
introducing background thread mutation or automatic agent loops.

### Stage 2 — proactive Claude → Amp messages

Add an explicit `send_to_amp` channel tool. Prefer a live Amp-plugin registration and
`PluginThread.appendUserMessage(...)` while the Amp client is open. A durable fallback can use
`amp threads continue <thread-id> --execute`, but that starts a new Amp turn and consumes usage, so it
must be opt-in and visibly distinct from merely appending a passive notification.

Required safeguards:

- origin and hop-count metadata;
- deduplication by message ID;
- maximum automatic reply depth;
- per-direction rate limits;
- allowlisted Amp thread IDs;
- no automatic permission relay.

### Stage 3 — optional experimental native peer

Only after the supported channel works, test a synthetic peer in a disposable environment. Capture
the real outbound envelope and receipts from `2.1.235`, verify registration, and version-gate the
backend. Treat it as an experimental optimization that gives Claude native `ListAgents`/`SendMessage`
ergonomics—not as the default transport.

If implemented, it needs fail-closed schema/version checks and must never rewrite or delete Claude's
own registry entries.

## Why not start with the two-script Tier 0 design

It is useful as a throwaway probe but not the best foundation:

- Amp → Claude reads undocumented peer key files and speaks an internal peer protocol, not merely the
  documented own-child script ingress;
- Claude → Amp through `amp threads continue --execute` is a separate mechanism, not a symmetric
  reply channel;
- there is no request correlation, delivery contract, timeout behavior, or loop boundary;
- it would validate less of the final supported architecture than a minimal Channel prototype.

## Security and operational boundaries

- Bind IPC to a Unix socket in a user-private directory; do not expose an unauthenticated TCP port.
- Treat all inbound text as untrusted channel content. Sender gating must happen before emitting a
  Claude channel notification.
- Do not expose Claude's messaging tokens or private peer keys to model-visible output or logs.
- Do not enable channel permission relay in the first version. Anyone able to send an approval through
  that path can approve Claude tool use.
- An Amp orb cannot access the local channel. The Amp thread must execute on this Mac/local runner.
- Channel notifications are transport-level fire-and-forget. The reply tool is the application-level
  acknowledgement.
- Notifications arriving while Claude is busy queue in order and may be grouped into one later turn;
  every request therefore needs its own explicit ID.
- The custom Channel API is itself research preview, but it is the documented extension point and is
  materially less fragile than a decompiled registry protocol.

## Current workspace state

- The directory is not a Git repository.
- No implementation exists yet.
- A Claude process is now resumed with
  `--dangerously-load-development-channels server:amp-bridge`, but this project still has no
  `.mcp.json` entry or channel server. The flag has nothing to load until those exist; after adding
  them, restart or resume the session again so Claude Code spawns the server.
- No messages were injected and no tokens, settings, sockets, or external state were modified during
  this Amp-side investigation.

## Sources

- [Claude Code: cross-session messaging](https://code.claude.com/docs/en/cross-session-messaging)
- [Claude Code: Channels](https://code.claude.com/docs/en/channels)
- [Claude Code: Channels reference](https://code.claude.com/docs/en/channels-reference)
- [Claude Code: hooks reference](https://code.claude.com/docs/en/hooks)
- [Official fakechat channel source](https://github.com/anthropics/claude-plugins-official/tree/main/external_plugins/fakechat)
- [Amp plugin manual](https://ampcode.com/manual#plugins)
- [Amp plugin API](https://ampcode.com/manual/plugin-api)
