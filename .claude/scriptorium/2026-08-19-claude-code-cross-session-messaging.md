---
scriptorium: true
action: create
title: "Claude Code Cross-Session Messaging and Channels"
type: research
domain: general
tags: [claude-code, multi-agent, ipc, unix-socket, mcp, channels, amp, agent-coordination]
---

# Claude Code Cross-Session Messaging and Channels

Decoded from the shipped Claude Code `2.1.235` binary (macOS) and verified against live on-disk state,
2026-08-19. Full write-up including the Amp-bridge design lives in
`amp_claude/.claude/research/2026-08-19-amp-claude-code-bridge.md` (Claude side) and
`…-amp-side.md` (Amp side).

## Supersedes a stale claim

`projects/claude-elixir-phoenix/learnings-archive-2026.md` (2026-02-13) states *"Claude Code has no
inter-agent messaging bus, no persistent task queue, and runs agents as ephemeral subprocesses"* and
uses it to dismiss inter-agent protocol patterns as prompt decoration. **As of ~v2.1.224 that is
false.** There is a real, authenticated, peer-to-peer messaging bus plus a supported external
integration point. The old note should be marked superseded — left as-is it will keep steering
multi-agent design decisions wrong.

## Two surfaces, and which one to build on

**Channels — the supported extension point.** An MCP server pushes *unsolicited custom notifications*
into a live Claude session; Claude replies through a tool the same server exposes. Methods:
`notifications/claude/channel`, `…/channel/permission`, `…/channel/permission_request`. Servers are
fingerprinted `channel-capable`. Entry syntax `plugin:<name>@<marketplace>` or `server:<name>`,
selected with `--channels`. **This is the right foundation for bridging Claude Code to an external
agent runtime.**

**The peer mesh — internal, private formats.** How Claude sessions talk to each other. Documented
below because its constraints apply to any bridge, but registry schema, key derivation and message
envelope are implementation details with no stability guarantee.

## Channels: the gate that decides whether it works

Eight documented skip kinds (`tengu_mcp_channel_gate`): `capability`, `era`, `provider`, `policy`,
`disabled`, `marketplace`, `allowlist`, `session`. Two are non-obvious and will burn a day each:

1. **The protocol-era trap.** *"connection negotiated a modern protocol revision with no unsolicited
   notification path."* Claude probes the modern discovery handshake first (`server/discover`,
   `2026-07-28`) and falls back to legacy `initialize` (`2025-11-25`) only if the server does not
   answer. **The fix is to leave `server/discover` unimplemented**, so the connection stays legacy —
   not to force a protocol version. Get it wrong and the server connects, looks healthy, and messages
   silently vanish. (See the confirmed contract below.)
2. **Dev mode bypasses the allowlist.** The allowlist check is guarded by `if (!entry.dev)`. A custom
   server is on nobody's allowlist, so it is only reachable as a dev channel — the binary states
   `"server: entries need --dangerously-load-development-channels"`. Hence:
   `claude --dangerously-load-development-channels server:<name>`.

Also: not available on Bedrock/Vertex/Foundry. `channelsEnabled` defaults **off** on claude.ai
Teams/Enterprise, on for Console absent managed settings — check plan class before writing code. When
org policy blocks, *"Inbound messages will be silently dropped."* Per-channel access control looks like
`~/.claude/channels/<name>/access.json`: `{"dmPolicy":"allowlist","allowFrom":[…],"groups":{},"pending":{}}`.

## Peer mesh mechanics

**Transport.** `$XDG_RUNTIME_DIR|tmpdir()/cc-socks/<pid>.sock`, mode `0600`, directory `0700`, path
capped ~103 bytes. Newline-delimited JSON: first line `{"type":"auth","token":"…"}`, then
`{"type":"user","message":{"role":"user","content":"…"}}`; also `{"type":"control","action":"rename"|"peer_message_status"}`.
The binary ships its own `socat` example in a log string. Not on native Windows.

**Auth — two tokens.** `childToken` is exported as `CLAUDE_CODE_MESSAGING_TOKEN` (free to any child
process: hooks, Bash-tool commands, MCP servers). `peerToken` is published to
`~/.claude/sessions/<pid>.<sha256(realpath(sockPath))>.key` (mode 0600) so other sessions can
authenticate. Lookup can require a live owner, cross-checking recorded `procStart` against
`ps -o lstart=` so recycled pids can't impersonate; comparison is `timingSafeEqual`. **The security
boundary is the POSIX user.**

**Discovery.** `~/.claude/sessions/<pid>.json`, heartbeated, holding `name`, `sessionId`, `cwd`,
`status`, `messagingSocketPath`, `peerProtocol`, `kind`. Admission filter: `kind === "interactive"` &&
has `sessionId` && `peerProtocol >= 1` && `now - (updatedAt ?? startedAt) < 24h`. `name` **is** the
address (`^[A-Za-z0-9_-]{1,128}$`).

**Delivery.** Arrives wrapped as `<cross-session-message from="…">`; **reply by copying `from` into
`to`**. No "busy" state — messages enqueue and drain at the receiver's next tool round.

## The permission gate — parity, not blanket holding

`crossSessionInbound`: `accept` | `refuse` | default. **Default is parity between permission-mode
classes**, not "prompting always holds":

- Modes collapse to `bypass` (from `bypassPermissions`), `plan`, `prompting`.
- Receiver in `bypassPermissions` → accept (`bypass-default`).
- Sender class == receiver class → deliver. Two prompting sessions talk fine.
- Mismatch → hold (`mode-mismatch`). No mode asserted → hold (`no-mode-asserted`). Unrecognized →
  hold (`mode-unknown`).

**Consequence for any external bridge:** a non-Claude process asserts no permission mode, so it lands
on `no-mode-asserted` → held pending human approval, *always* — not just when unattended. The
receiving session must set `crossSessionInbound: "accept"`. Held messages emit `peer_message_status`
receipts (`held` → `delivered`|`denied`|`expired`); on shutdown they settle as `expired`.

The whole subsystem sits behind a GrowthBook gate with mid-session late-binding — probe for
`CLAUDE_CODE_MESSAGING_SOCKET` rather than assuming availability.

## Experimental: registering a non-Claude process as a peer

Because the registry is plain JSON in a user-writable directory with a discoverable admission filter,
a foreign process could in principle publish a conforming `<pid>.json` + `.key`, bind a socket, and
become addressable from Claude's native `SendMessage`/`ListAgents`. That would give ideal ergonomics.

**Marked experimental — do not build on it.** The decisive round-trip test was never run; the formats
are private and can change without notice; and the permission gate above would hold such a sender's
messages anyway. **Prefer Channels** — a working Amp↔Claude bridge was built on Channels instead and
verified end to end, so this route is unnecessary as well as risky. Recorded here as an observation
about the architecture, not a recommended technique.

## Channels: confirmed working contract

Verified live (2026-08-19) by building a Go channel server that round-trips messages between an Amp
thread and a Claude Code session. Four things are load-bearing and none are obvious:

1. **Do NOT implement `server/discover`.** Claude negotiates in two phases: it first probes the modern
   discovery handshake (`2026-07-28`), then falls back to legacy `initialize` (`2025-11-25`). Channels
   only have a delivery path on the **legacy** handshake. A server — or SDK — that answers
   `server/discover` silently kills channel delivery while every health check still passes.
2. **Capability is `experimental: {"claude/channel": {}}`**, not a top-level key. Its presence is what
   registers the listener. The server's `instructions` string is injected into Claude's system prompt.
3. **The notification field is `meta`, not the MCP-standard `_meta`.** Keys must be identifiers, values
   render as attributes on the `<channel source=… >` tag, and **`source` is reserved** — Claude sets it
   itself, so reusing it emits the attribute twice.
4. **The reply direction is an ordinary MCP tool**, not a protocol method. Claude's transcript output
   never reaches the channel.

`claude plugin init <name> --with channel` scaffolds Anthropic's reference implementation; docs at
`code.claude.com/docs/en/channels-reference`.

**Eligibility surprise:** channels worked on a personal `claude_max` account with **no**
`channelsEnabled` and **no** managed-settings file, despite a startup org-policy warning. Don't
install machine-wide managed settings to "fix" that warning before testing whether delivery actually
works — it likely already does.

**Second capability, handle with care:** `claude/channel/permission` is a separate opt-in. Claude
sends `notifications/claude/channel/permission_request` and the channel answers with
`notifications/claude/channel/permission` correlated by `request_id` — i.e. a channel can act as a
*remote permission approver*. Powerful (approve from your phone) and a real governance decision, since
it delegates approval authority away from the human at the keyboard.

## Permission caveat

Claude's tool contract warns against *cross-session permission laundering* — routing work one session
denied through another. A bridge inherits the **receiving** session's permission settings, not the
sender's. Don't design one as a bypass.

Related: [[Claude Code Session Management Commands]]
