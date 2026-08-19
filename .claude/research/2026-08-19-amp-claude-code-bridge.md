# Amp ↔ Claude Code bridge — research from the Claude Code side

**Date:** 2026-08-19 (rev. 12)
**Context:** Independent Claude-Code-side investigation of the cross-session messaging layer, decoded
from the shipped binary and verified against live state. Rev. 2 incorporates the Amp-side finding that
**Channels** — not synthetic peer registration — is the officially supported integration point, plus
two corrections.
**Verified against:** Claude Code `2.1.235`, macOS (Darwin 25.5.0).
**Companion:** `2026-08-19-amp-claude-code-bridge-amp-side.md` (Amp-side synthesis).

---

## TL;DR

There are **two** viable integration surfaces, and I originally found only one.

1. **Channels** *(supported, recommended)* — a first-class extension point where an MCP server pushes
   unsolicited `notifications/claude/channel` into a live session, and Claude replies through a tool
   the same server exposes. This is what the official cross-session guide points at for external
   events. Credit to the Amp-side research for catching it; I missed it.
2. **The cross-session peer mesh** *(internal, not a public contract)* — the UDS socket + registry
   that Claude sessions use to talk to each other. Fully decoded below. Real, and more symmetric than
   Puck's first read suggested, but it is a *private* format: registry schema, key-file derivation and
   message envelope are all implementation details that can change without notice.

Both are documented here because the mesh explains what Claude-to-Claude coordination actually *is*,
and its constraints (especially the permission gate) apply to any bridge. But **build on Channels.**

Channels carry their own gauntlet of gate conditions, several of which will block this machine by
default — see §6. Those are the practical blockers, and they're the part the Amp-side report doesn't
cover.

---

## 1. What is live right now

```
$ claude --version
2.1.235 (Claude Code)

$ env | grep CLAUDE_CODE_MESSAGING
CLAUDE_CODE_MESSAGING_SOCKET=/tmp/cc-socks/52527.sock
CLAUDE_CODE_MESSAGING_TOKEN=<64 hex chars>
```

`ListAgents` returned **15 reachable peer sessions** across projects (`enaia-main-81`, `scribe-65`,
`kadetade-elixir-86`, …) with name, kind, status and age. A live 15-node mesh already exists.

`/tmp/cc-socks/` held 18 sockets; 3 stale ones were correctly filtered out (see §4).

Also live: `~/.claude/channels/imessage/access.json` — **a channel is already installed on this
machine** (the iMessage capture path from `scribe`), with a DM allowlist policy.

---

## 2. Transport (peer mesh)

### Socket path

```js
XDG_RUNTIME_DIR || tmpdir()  →  <base>/cc-socks/<pid>.sock
// if the path exceeds 103 bytes:
//   /tmp/cc-socks-<uid>/<pid>.sock
```

- Socket mode `0600`, containing directory `0700`.
- Every ancestor of the socket directory is vetted before binding: must be private-or-sticky, owned by
  the user or root, no foreign-owned symlinks, no symlink loops. Failure kinds are enumerated
  (`directory_rule`, `foreign_owner`, `leaf_shape`, `dangling_link`, `symlink_loop`, `not_directory`,
  `raced`); any of them means the session **refuses to bind** and messaging is off for that session.
- Path capped ~103 bytes (`ENAMETOOLONG` guard). Override via `--messaging-socket-path`,
  `CLAUDE_CODE_TMPDIR`, or `XDG_RUNTIME_DIR`.
- Not available on native Windows (that path uses `\\.\pipe\…`).

### Wire format

Newline-delimited JSON. The binary ships its own usage example in a log string:

```
[uds-messaging] Inject messages (auth line REQUIRED here):
{ echo '{"type":"auth","token":"'"$CLAUDE_CODE_MESSAGING_TOKEN"'"}';
  echo '{"type":"user","message":{"role":"user","content":"hello"}}'; } \
| socat - UNIX-CONNECT:/tmp/cc-socks/52527.sock
```

| `type` | Purpose |
|---|---|
| `auth` | Must be the **first** line. A bad/blank/unparseable first line drops the connection. |
| `user` | The message. Optional `session_id` (mismatch → dropped), optional `file_attachments`, delivery priority (`next` / `later`). |
| `control` | `action`-dispatched: `rename`, `peer_message_status`. Unknown actions logged and ignored. |

Guards: per-line length cap (over-long line drops the connection), unauthenticated frames dropped and
connection closed, socket-level peer credential checks (`getPeerUid` / `getPeerPid`) with self-send
ancestry detection.

> `ping`/`pong`/`control_request`/`control_response`/`whoami` are abundant in the binary but belong to
> the **daemon / Remote Control** IPC channel, not this inbox. Not confirmed against the UDS handler.

---

## 3. Authentication (peer mesh)

Two tokens per session, each 32 random bytes hex-encoded:

- **`childToken`** → exported as `CLAUDE_CODE_MESSAGING_TOKEN`. Free to any child process of the
  session (hooks, Bash-tool commands, MCP servers it spawns).
- **`peerToken`** → published so *other* sessions can authenticate inbound.

```
~/.claude/sessions/<pid>.<sha256(realpath(socketPath))>.key    mode 0600
{"peerToken":"…","procStart":"Wed Aug 19 05:19:53 2026"}
```

Verified empirically:

```
$ ls ~/.claude/sessions/ | grep 52527
52527.cc94821f75709e033cbe846a953b34b36f46290c4dc5c516ca8915901a17fd1e.key

$ python3 -c "import hashlib;print(hashlib.sha256(b'/tmp/cc-socks/52527.sock').hexdigest())"
cc94821f75709e033cbe846a953b34b36f46290c4dc5c516ca8915901a17fd1e   ← exact match
```

Lookup filters `~/.claude/sessions/` for `.<hash>.key`, parses the pid from the filename, and can
require a live owner — cross-checking recorded `procStart` against
`LC_ALL=C TZ=UTC ps -o lstart= -p <pid>` so recycled pids can't impersonate. Comparison is
`crypto.timingSafeEqual`.

**Security boundary is the POSIX user.** Any process running as Oliver can read any live session's
`peerToken`. That's by design — it's how peers authenticate to each other.

---

## 4. Discovery (peer mesh)

`~/.claude/sessions/<pid>.json`, heartbeated. Real contents of this session's entry:

```json
{
  "pid": 52527,
  "sessionId": "b959f5dd-fee6-43f6-bdba-20978fb09aa6",
  "cwd": "/Users/oliverkriska/Projects/amp_claude",
  "startedAt": 1787116795358,
  "procStart": "Wed Aug 19 05:19:53 2026",
  "version": "2.1.235",
  "peerProtocol": 1,
  "kind": "interactive",
  "entrypoint": "cli",
  "messagingSocketPath": "/tmp/cc-socks/52527.sock",
  "name": "amp-claude-a6",
  "nameSource": "derived",
  "status": "busy",
  "updatedAt": 1787116897032,
  "statusUpdatedAt": 1787116897032
}
```

`name` is the address (`^[A-Za-z0-9_-]{1,128}$`), set by `--name` / `/rename` or auto-derived.

Peer admission filter (FleetView poll):

```js
entries.filter(e =>
  e.kind === "interactive" && e.pid !== process.pid && e.sessionId &&
  !alreadyKnown.has(e.sessionId) &&
  (e.peerProtocol ?? 0) >= 1 &&
  Date.now() - (e.updatedAt ?? e.startedAt) < 86_400_000   // 24h
)
```

Explains the 18-sockets → 15-peers gap.

### On synthetic peer registration — downgraded

Rev. 1 of this document leaned hard on the idea that a bridge could write a conforming `<pid>.json` +
`.key` and appear as a native peer. **Treat that as experimental, not a plan.** Three reasons:

1. The decisive test never ran — the auto-mode classifier blocked it (binding into `/tmp/cc-socks/`
   plus writing `~/.claude/sessions/` reads as suspicious, fairly). It left no artifacts.
2. Registry schema, key derivation and the outbound envelope are **private formats**. Nothing commits
   Anthropic to keeping them stable; a point release could break the bridge silently.
3. §5 shows the permission gate would hold an external sender's messages anyway, for a reason that is
   hard to work around honestly.

It remains the most *elegant* end state — native `ListAgents`/`SendMessage` ergonomics with zero
bridge-specific tooling on the Claude side. It is not the right thing to build first.

---

## 5. The inbound permission gate — corrected

**Correction to rev. 1.** I wrote that "`plan` / prompting modes hold for user approval". That's
wrong, and the Amp-side report caught it. The real rule is **parity between permission-mode classes**.

Modes collapse into three classes: `bypass` (from `bypassPermissions`), `plan`, and `prompting`
(everything else). Then:

| Situation | Outcome | Cause code |
|---|---|---|
| Receiver in `bypassPermissions` | **accept** | `bypass-default` |
| Sender class == receiver class | **deliver** | — |
| Sender class != receiver class | **hold** | `mode-mismatch` |
| Sender asserts no mode | **hold** | `no-mode-asserted` |
| Either mode unrecognized | **hold** | `mode-unknown` |
| `crossSessionInbound: "accept"` | **accept** | `explicit-setting` / `policy-accepts` |
| `crossSessionInbound: "refuse"` | **refuse** | `opt-out` |

So two sessions both in prompting mode deliver to each other fine. Amp's correction stands.

**And it sharpens into the real blocker for a UDS bridge:** an external process is not a Claude
session and asserts no permission mode, so it lands on `no-mode-asserted` → **hold**. Every message
waits on a human approval prompt unless the receiving session sets `crossSessionInbound: "accept"`.
That is a stronger constraint than "set accept for unattended use" — it applies *always*, not just
when unattended.

Settings resolve through `policySettings` → `flagSettings` → `userSettings`, with
`localSettings` / `projectSettings` / `repoSettings` in the chain (so per-project scoping looks
possible). Held messages emit `peer_message_status` receipts back to the sender:
`held` → `delivered` | `denied` | `expired`, each with reason text. On shutdown, still-held messages
settle as `expired`.

Ingress origins are classified `peer` / `hostInjected` / `coordinator` / `ungated`, with subkinds
`peer-send-message` and `task-notification`.

Delivery semantics, from Claude's own `SendMessage` contract and the injected prompt text:

> *"A peer session sent a message while you were working: … After completing your current task, decide
> whether/how to respond (reply via SendMessage to the `from=` address)."*

Messages arrive wrapped as `<cross-session-message from="…">`; reply by copying `from` into `to`. No
"busy" state — messages enqueue and drain at the receiver's next tool round.

The tool contract also warns against **cross-session permission laundering** — routing work one
session denied through another. A bridge inherits the *receiving* session's permissions. Don't build
one as a bypass. (Directly relevant here: my own blocked test in §4 is exactly the kind of thing that
must not be laundered through a peer.)

---

## 6. Channels — the supported path, and its gauntlet

Confirmed real. `channelsEnabled` and `allowedChannelPlugins` are settings-schema entries;
`--channels` and `--dangerously-load-development-channels` are real CLI flags.

### Contract

Inbound is **unsolicited custom MCP notifications** from a channel-capable server:

```
notifications/claude/channel
notifications/claude/channel/permission
notifications/claude/channel/permission_request
```

Servers are fingerprinted and cached as `channel-capable` (alongside `skills-capable`). Channel entry
syntax is `plugin:<name>@<marketplace>` or `server:<name>`.

The session displays a standing banner:
> *"Channels (experimental) messages from X inject directly in this session — restart without … to stop"*

### The gate (`tengu_mcp_channel_gate`) — 8 ways to be silently skipped

| skip kind | Meaning |
|---|---|
| `capability` | Server never advertised channel capability |
| `era` | **Protocol revision has no delivery path for unsolicited custom notifications** |
| `provider` | *"Channels are not available on Bedrock, Vertex, or Foundry"* |
| `policy` | Org hasn't set `channelsEnabled: true` in managed settings |
| `disabled` | *"Channels are not currently available"* |
| `marketplace` | Installed plugin's source doesn't match the requested marketplace |
| `allowlist` | Not on Anthropic's default allowlist or the org's `allowedChannelPlugins` |
| `session` | Session-level gate |

Two of these matter enormously and neither is obvious:

**(a) The protocol-era trap.** The binary warns:
> *"skipping … a modern-era protocol revision, which has no delivery path for unsolicited custom
> notifications"* → telemetry `channels-blocked-era-`, user-facing *"Channel messages from X are
> unavailable: this connection's protocol version has no channel delivery path."*

Unsolicited custom notifications are not expressible in modern-era MCP revisions. A channel server
**must** land on a protocol era that has a delivery path — see the `MCP_PROTOCOL_NEGOTIATION` env var
(`auto` | `legacy`) and the negotiation denylists. Get this wrong and everything connects cleanly, the
server looks healthy, and messages vanish. This is the single most likely way the Channels build fails
mysteriously.

**(b) Dev mode bypasses the allowlist.** The allowlist check is guarded by `if (!entry.dev)`. A custom
`amp-bridge` is on nobody's allowlist, so it is *only* reachable as a dev channel — and the binary
says so explicitly: **`"server: entries need --dangerously-load-development-channels"`**. Amp's
recommended invocation is therefore not merely convenient, it's mandatory:

```
claude --dangerously-load-development-channels server:amp-bridge
```

Other concrete failure strings worth grepping for when debugging: `"no MCP server configured with that
name"`, `"plugin not installed"`, `"not on your org's approved channels list"`, and — when org policy
blocks — **`"Inbound messages will be silently dropped"`**.

### Eligibility on this machine

Checked: `channelsEnabled` is **not set** in `~/.claude/settings.json`, `settings.local.json`, or any
project settings, and there is **no managed-settings file**. Per the schema description:

> *"claude.ai Teams/Enterprise: default off. Console: default on unless managed settings exist. Set
> true to allow; users then select servers via `--channels`."*

So on a claude.ai Teams/Enterprise account this is **off by default** and needs setting before anything
works. Worth confirming which plan class this account resolves to before writing code — it's a
five-minute check that decides whether step one is "write a server" or "get a setting flipped".

Existing channel on disk (`~/.claude/channels/imessage/access.json`) shows the per-channel access
model to mirror:

```json
{ "dmPolicy": "allowlist", "allowFrom": ["…"], "groups": {}, "pending": {} }
```

---

## 7. Recommended path

Agreeing with the Amp-side staging, with Claude-side prerequisites folded in:

**Step 0 — clear the gates before writing code.** Confirm plan class; set `channelsEnabled: true` if
needed; decide the MCP protocol era and verify the server negotiates one with a notification delivery
path. Ordering matters: (a) and the era trap will otherwise burn a day.

**Step 1 — Amp side.** `list_claude_sessions` and `ask_claude` tools. Explicit request IDs and Amp
thread IDs in every payload.

**Step 2 — Claude side.** A custom channel MCP server (`server:amp-bridge`), loaded with
`--dangerously-load-development-channels`. Inbound via `notifications/claude/channel`; replies via a
tool the same server exposes. Session must be restarted or resumed with the flag.

**Step 3 — IPC.** Private Unix socket between the Amp plugin and the channel server. Note this is the
bridge's *own* socket, unrelated to `/tmp/cc-socks/`.

**Step 4 — hold the line on scope.** No permission relay, no automatic agent loops initially.

**Loop prevention** (Puck's "ancient and surprisingly achievable failure mode"): tag bridged messages
with an origin marker and refuse to forward anything already carrying one; keep a hop counter; rate
limit per direction. Claude's `selfSent` detection does not help — the bridge is a distinct
correspondent.

**Fallback if Channels is blocked** (wrong plan class, era trap unresolvable): the Tier-0 UDS path
still works today — a shell script that reads the peer token and pipes two NDJSON lines into a
session's socket, with `amp threads continue <id> --execute` for the return leg. Requires
`crossSessionInbound: "accept"` on the receiving session (§5). Use it as a stopgap, not a destination.

---

## 8. Amp CLI surface (brief)

`amp` `0.0.1787112648-gd35a66`:
`amp threads continue <id> --execute "<prompt>"`; `--stream-json` / `--stream-json-input` (JSON Lines
on stdin, requires `--execute` + `--stream-json`) for a persistent bidirectional channel;
`amp mcp add`; `amp plugins` (`amp plugins show-docs` for the contract); `amp tools list`.

Constraint Puck flagged and worth underlining: the Amp side must run on **this machine** (local runner,
not an orb) to reach local sockets.

---

## 9. Prior art

`qmd query` over scriptorium returned **nothing** on cross-session messaging or an Amp bridge — a
genuine gap.

Worth flagging: the top hit, `projects/claude-elixir-phoenix/learnings-archive-2026.md` (2026-02-13),
asserts *"Claude Code has no inter-agent messaging bus, no persistent task queue, and runs agents as
ephemeral subprocesses"*, and uses it to dismiss inter-agent protocol patterns as prompt decoration.
**That is now false** and should be marked superseded, or it will keep steering multi-agent decisions
wrong.

---

## 10. Live probe results (rev. 3)

Built `amp-bridge/server.py` — an instrumented stdio MCP server that logs the whole handshake,
declares `claude/channel`, and pushes one `notifications/claude/channel` after initialization. It
connected to a real Claude Code client, which resolved several open questions immediately.

### The negotiation is two-phase — and my first reading of it was wrong

**Correction.** Rev. 3 claimed `2025-11-25` was a *modern*-era revision and that a server must
negotiate down to `2025-06-18`. Both wrong; the Amp-side review caught it and the full log proves it.
Claude Code negotiates in **two phases**:

```jsonc
// phase 1 — MODERN, discovery-based:
{"id":"server-discover-probe-1","method":"server/discover",
 "params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28", …}}}

// phase 2 — server didn't answer, so it falls back to the LEGACY handshake:
{"id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25", …}}
```

So **modern era = `2026-07-28` via `server/discover`**; `2025-11-25` is the *legacy*
`initialize` handshake, and legacy is exactly where channels work. The binary's skip reason reads
*"connection negotiated a modern protocol revision with no unsolicited notification path"*. The forced
downgrade to `2025-06-18` was accepted but was never necessary.

**The real rule, and it inverts the earlier advice:** do **not** implement `server/discover`. Letting
it fail keeps the connection on the legacy handshake, which is the one with a channel delivery path.
A server — or an MCP SDK — that helpfully answers `server/discover` silently kills channel delivery
while continuing to report healthy. The Python probe got this right by accident; the Go bridge does it
deliberately and says so in a comment.

### Channel contract — from Anthropic's own scaffold

`claude plugin init <name> --with channel` generates a reference channel server, and
`https://code.claude.com/docs/en/channels-reference` is the published doc. Generating and reading it
settles the wire shapes (both as the Amp-side review predicted — my probe had both wrong):

```ts
capabilities: {
  tools: {},
  // Required: presence of this key registers the channel notification
  // listener on Claude's side.
  experimental: { 'claude/channel': {} },
}
```

```ts
mcp.notification({
  method: 'notifications/claude/channel',
  params: { content: 'the event body', meta: { chat_id: '...', sender: '...' } },
})
// Each meta key becomes an attribute on the <channel> tag. Keys must be
// identifiers (letters/digits/underscores) — others are silently dropped.
```

Note **`meta`, not the MCP-standard `_meta`**, and values render as tag attributes (so strings).
Events reach the model as `<channel source="amp-bridge" request_id="…">`. The scaffold's own
`instructions` string carries the load-bearing constraint:

> *"Anything you want the sender to see must go through the reply tool — your transcript output never
> reaches the channel."*

The reply direction is therefore an ordinary MCP tool (the scaffold names it `reply`, taking `text`),
not a protocol method. For plugin-delivered channels, `plugin.json` declares
`"channels": [{"server": "<name>", "displayName": "<name>"}]`; `server:` entries are the dev-mode
equivalent.

Telemetry `tengu_mcp_channel_message` records `prompt`, `next`, `channel`, `meta_key_count` — the
event is injected as a **prompt with `next` priority**. `notifications/claude/channel/permission`
destructures `request_id` and is matched against pending requests, i.e. it correlates an approval back
to a specific request.

Full skip-reason strings, now confirmed verbatim: `server did not declare claude/channel capability`
(capability) · `connection negotiated a modern protocol revision with no unsolicited notification
path` (modern) · `channels are not available on third-party providers` (provider) · `channels feature
is not currently available` (disabled) · plus policy, marketplace, allowlist, and entry kinds
`team` / `enterprise` / `server` / `plugin` / `ledger`.

### Eligibility — resolved

Account is `organizationType: "claude_max"`, `organizationRole: "admin"`, self-owned org, no
managed-settings file. Startup with `--dangerously-load-development-channels server:amp-bridge`
produced the **org-policy warning**, so channels are policy-blocked on this account today.
`channelsEnabled` is documented as a *managed-org opt-in* and the error text points at managed
settings, so one fix is a machine-level policy file (Oliver is admin, so it is his to write):

```
sudo mkdir -p "/Library/Application Support/ClaudeCode"
echo '{"channelsEnabled": true}' | sudo tee "/Library/Application Support/ClaudeCode/managed-settings.json"
```

**Hold off on running this** — the Amp-side review pushed back and the caution is right. Installing
managed settings activates machine-wide policy precedence permanently, to fix what may be a
misapplied gate: a personal Max account arguably shouldn't be evaluated against an *org* policy at
all, so the warning may be an account-classification or preview-rollout artifact rather than a real
denial. Retest first with a correctly-shaped server actually registered — the earlier warning appeared
when no `amp-bridge` server existed at all, so it was never a clean test. Escalate to the policy file
only if a correct server still gets skipped with a `policy` reason.

### Gotcha: `--scope local` gets clobbered

`claude mcp add amp-bridge --scope local` writes into `~/.claude.json` under the project key. **A
running Claude Code session rewrites `~/.claude.json` from its own in-memory state and silently
overwrote the entry** — the server registered, connected once, then vanished from `claude mcp list`.
Use `--scope project`, which writes a separate `.mcp.json` the session doesn't own. Costs a one-time
approval prompt; worth it. This applies to any tooling that mutates MCP config while a session is
live.

---

## 11. Hardening (rev. 5)

The Amp-side review raised four issues against the first Go build. All four were valid; three are
fixed, the fourth is the standing next step.

**Multi-session collision → per-session identity.** A fixed `/tmp/amp-bridge.sock` was worse than a
collision: the old code did a blind `os.Remove` before binding, so a second session would silently
*hijack* the first one's address and messages would route to the wrong session. Fixed by giving each
bridge its own identity.

The interesting part is where identity comes from. **A Claude-spawned MCP server inherits no `CLAUDE_*`
environment variables** — verified by registering a probe server that dumped its env; nothing came
through. But the MCP server is spawned *directly by* the Claude process, so `os.Getppid()` is the
session's PID, and Claude publishes `~/.claude/sessions/<pid>.json` (§4) with that session's `name`,
`sessionId` and `cwd`. So the bridge reads its own parent's session file to learn who it belongs to —
using the internal registry decoded earlier, as best-effort enrichment with a PID-derived fallback,
never a hard dependency.

Result: sockets live in `/tmp/amp-bridge-<uid>/<session-name>.sock` (dir `0700`, socket `0600`), each
bridge publishes its own registry entry, and the client grew `--list` and `--session <name>`.
Binding now probes for a live listener and **refuses to hijack** rather than stealing, mirroring
Claude's own `live`/`dead` socket check. Stale entries from hard-killed bridges are swept on the next
`--list`, so it self-heals without cleanup.

**Unsafe correlation fallback → strict routing.** `request_id` was optional with a "most recent
request" fallback, which silently mis-routes under concurrency. It is now required in the tool schema,
and an omitted id is honoured only when exactly one request is in flight. Otherwise the reply is
refused with `isError` and a message telling Claude to include the id — failing loudly beats answering
the wrong question.

**Log hygiene.** The log was `0644` and recorded complete protocol frames including conversation
content. Now `0600`, and frames are logged as shape only (`method`, `id`, byte count) unless
`AMP_BRIDGE_LOG_BODIES=1` is set for protocol debugging. Any pre-existing looser log gets chmod'd on
startup.

**Test coverage.** 22 checks now, including: `server/discover` is declined, capability shape,
`meta`-not-`_meta` with string values and identifier keys, two concurrent requests get distinct ids
and each caller receives the answer bound to *its own* `request_id` with no cross-talk, ambiguous and
unknown ids are refused, log mode is `0600`, and conversation text does not appear in the log.

**Still standing:** these simulate Claude's half of the conversation. The live run remains the
decisive test — nothing here proves the channel gate actually delivers the notification.

---

## 12. IT WORKS — live end-to-end confirmed (rev. 6)

The decisive test passed. Full round trip, no simulation:

```
$ ./amp-bridge --ask "SELF-TEST ... reply with exactly GATE-OPEN"
GATE-OPEN
[exited with code 0]
```

The event arrived in the Claude session as a real channel event:

```
<channel source="amp-bridge" request_id="amp-1787126723104490000-1" source="amp">
SELF-TEST from the bridge: ...
</channel>
```

…Claude called `mcp__amp-bridge__reply` with the `request_id`, the bridge correlated it and wrote the
answer back over the Unix socket, and the CLI printed it. Every hop verified.

**The `experimental: {"claude/channel": {}}` capability also propagates the server's `instructions`
string into Claude's system prompt** — the tool arrives as `mcp__amp-bridge__reply` with the channel
guidance attached, which is how the model knows replies must go through the tool.

### The policy warning was a red herring — no managed settings needed

This is the important operational finding. Channels work on this `claude_max` account **without**
`channelsEnabled` and **without** a managed-settings file, despite the startup org-policy warning.
The Amp-side review's instinct was right and the caution to not run the sudo command was correct —
installing machine-wide policy precedence would have been a permanent change made to fix a
non-problem. §6's eligibility analysis stands as written for *what the gate checks*, but the observed
default on personal Max is: it delivers.

### Two live-only findings

**`source` is a reserved meta key.** Claude sets `source="<channel name>"` on the tag itself, so a
`meta` key named `source` renders the attribute twice (visible above). Dropped it — it was redundant
anyway, since the channel identifies the sender. No other reserved keys observed; `request_id` is fine.

**Never `cp` over a Mach-O binary on macOS.** Overwriting the bridge binary in place invalidated its
code signature, and macOS then SIGKILLed it on exec (exit 137) — including for subsequent `go build`s
to the same path, because the bad signature is cached per path. `rm` the file first, then build. The
*running* process was unaffected throughout (it holds its original inode), which is also why a rebuild
never disturbs a live session's bridge: fixes land on the next restart, not mid-session.

---

## 13. Hardening, full duplex, and the permission channel (rev. 7)

### Hardened

- **Resource caps.** Max 8 in-flight requests and 64 KB per message
  (`AMP_BRIDGE_MAX_INFLIGHT` / `AMP_BRIDGE_MAX_BYTES`). Without these a runaway
  Amp loop floods the session and a large payload eats the context window. Both
  fail with an explanatory error rather than degrading.
- **Caller disconnect.** The connection handler now reads continuously and
  answers each request in its own goroutine, so EOF is noticed immediately and
  in-flight waits are abandoned (`AMP_ABANDONED`) instead of holding a slot for
  the full 180 s. This also gives per-connection request pipelining for free.
- **Clean shutdown.** SIGTERM/SIGINT/SIGHUP now remove the socket and registry
  entry. Previously only `--list`'s sweep cleaned up, leaving a confusing window.
- **Log accuracy.** `AMP_REQUEST inflight=` was sampled *before* registering the
  request, under-reporting by one and making concurrent traffic look serial — the
  defect was visible in Amp's own test log. Now sampled after.

Test suite at this point was 30 checks in a Python harness: oversize rejection,
in-flight cap, disconnect recovery, unknown-tool error, `ask_amp` failure path,
plus the original protocol and concurrency coverage. SIGTERM cleanup verified
separately. (Superseded in rev. 8 — see §14.)

### Full duplex

`ask_amp` (`text`, optional `thread_id`) shells out to
`amp threads continue <id> --execute`. The bridge remembers the last thread id
seen from an inbound request, so Claude can reach back without being told one;
the client passes it with `--thread`. `AMP_BRIDGE_DISABLE_OUTBOUND=1` kills the
direction entirely. Previously the bridge was half-duplex — Claude could only
answer, never initiate.

### Skills for both sides

`.claude/skills/amp-bridge/SKILL.md` (Claude) and `AGENTS.md` (Amp). Both carry
the same three load-bearing warnings, the failure-triage table, and the etiquette
rules — notably that transcript output never reaches the other agent, and that
neither side should route work around a permission denial.

### The permission channel — a second capability

`claude/channel/permission` is a **separate capability key** from
`claude/channel`. The two appear adjacent in the binary alongside `protocolEra`,
`isServerRegistered`, `fromServer`, and `setMode` / `bridge setMode '…' reject`.
The observable flow:

- Claude → server: `notifications/claude/channel/permission_request`
- server → Claude: `notifications/claude/channel/permission`, carrying a
  `request_id` that is matched against pending requests (`"matched pending"`)

**Inferred, not verified:** a channel that declares this capability can act as a
*remote permission approver* — Claude asks the channel for approval and the
channel answers. That reading fits the shipped iMessage channel exactly (approve
Claude's action from your phone), and it explains why `permission` is a
server→client notification correlated by id.

If correct, the Amp side could approve or deny Claude's permission prompts. That
is a genuine coordination feature and a governance decision, not just plumbing:
it delegates approval authority from the human to another agent, and the
`setMode` strings hint a channel may also be able to change the session's
permission mode. **Do not enable this without deciding deliberately who is
allowed to approve what.** Our bridge does not declare the capability.

Verifying it needs a live experiment: declare `claude/channel/permission`,
trigger a permission prompt, and capture the `permission_request` payload.

### Lifecycle — still unknown

Not yet tested, and each affects whether this is dependable beyond a fresh
session: does the channel survive `/compact` or a context reset; does Claude
respawn a crashed bridge (there is reconnection logic — `transport
closed/disconnected, attempting automatic reconnection`, with
`maxReconnectAttempts`); what happens to inbound events while the session sits in
plan mode or on a permission prompt; and whether two channels can be loaded at
once.

---

## 14. Cleanup, test suite and quality gates (rev. 8)

The bridge was proven working but had grown as a spike: one 700-line `main.go`,
configuration in package-level globals read from the environment at init, a
superseded Python probe still on disk, and a Python end-to-end harness sitting
beside a Go project.

### What was removed

- `probe-v0-python/` — the throwaway v0 probe. Its findings are in §10; the code
  had no further use and its presence invited someone to run the wrong server.
- `AMP_BRIDGE_PROTOCOL` / `forceProto` — an escape hatch built on the *wrong*
  reading of the era trap (§10). Forcing a protocol version is not the fix and
  never was; leaving `server/discover` unimplemented is. Keeping the lever around
  would have re-taught the mistake.
- `sessionInfo.Status` — parsed from Claude's session file, never read.
- Stale scratch: `handshake.log`, `e2e.log`, and three 4 MB binaries under `/tmp`.

### Testability was the real defect

Caps, timeouts and the Amp binary path lived in package-level `var`s initialised
from `os.Getenv` at program start. Nothing could be tested without mutating
process-global state, which in turn made parallel tests unsafe. They are now a
`config` struct resolved once in `main` and carried on the `bridge`; the
Claude→Amp state (`lastThread`) moved onto the bridge with it. Tests construct
the config they want and never touch the environment.

`handle()` split into `handleInitialize` / `handleToolsCall` / `handleReply` /
`handleAskAmp`, and the file split into `config.go`, `mcp.go`, `channel.go`,
`client.go`, `amp.go`, `main.go`. Arguments now parse in a pure `parseArgs`
function, and an unrecognised flag is an error — previously it was silently
ignored, so a typo looked like the feature failing rather than the command being
wrong.

### The test pyramid

Two tiers, per the build-tag convention:

| Tier | Command | What it covers |
|---|---|---|
| Unit | `make test` | Protocol frames, correlation, caps, identity, registry, client dispatch, `askAmp` against a stub Amp binary. In-process; also drives a real Unix listener. |
| Integration | `make test-integration` | Spawns the actual binary and drives it over stdio as Claude and over the socket as Amp: handshake, round trip, concurrency, caps, log hygiene, disconnect, `--list`/`--ask`, SIGTERM cleanup, stdin close, hijack refusal. |

66 top-level tests, both tiers under `-race`, 77.7% statement coverage. The
remainder is `main`/`runServer`, which the integration tier exercises
out-of-process where the coverage tool cannot attribute it.

**The migration was validated against the thing it replaced.** The refactor ran
first, then the *original* Python harness — which knew nothing about the new
structure — was run against the rebuilt binary. All 30 of its checks passed,
proving the wire contract was unchanged, and only then was it deleted. Its
coverage is now in the Go integration tier plus a few checks it never had
(SIGTERM cleanup, hijack refusal, stdin close, client `--list`/`--ask`).

### Quality gates

`.golangci.yml` (golangci-lint v2) follows the house standard: 29 linters across
bugs / security / performance / style / modernization / testing, thresholds
gocyclo 15, gocognit 30, funlen 80/50, nestif 4, formatters `gci` + `gofumpt`.
`run.build-tags: [integration]` matters — without it the integration tier is
never linted. `make check` runs `go mod tidy -diff`, `golangci-lint fmt --diff`,
`go vet` on both tag sets, the linters, and both test tiers. Clean.

Two config notes worth keeping. `misspell` must **not** be set to the UK locale:
it rewrites `initialize` and `notifications/initialized`, which are MCP method
names and not ours to rename. And the `noctx` linter is right about
`net.Dial`/`net.Listen` even for Unix sockets — those are now `DialContext` and
`ListenConfig.Listen`, which gives the client a real connect timeout.

---

## 15. OTP supervision, ported to Go (rev. 9)

The bridge had a failure class it could not report. `serveSocket` returned on any
`Accept` error and the process carried on: Claude still saw a healthy MCP server,
Amp could never connect again, and nothing said so. An unrecovered panic anywhere
was worse — it killed the channel for the whole session.

That is the shape of the `:one_for_one` anti-pattern from
[[OTP rest_for_one Supervision for Shared State]]: a sibling continuing to run
while holding a dead reference to shared state.

### What ported, and what did not

| OTP idea | Bridge | Verdict |
|---|---|---|
| Bounded mailbox | `maxInFlight` | already had it |
| `terminate/2` | `sync.OnceFunc` cleanup + signal handler | already had it |
| Registry | `/tmp/amp-bridge-<uid>/*.json` | already had it |
| Process isolation | `guard()` — `recover` per connection, per waiter, per request | **added** |
| Supervisor restart | `superviseSocket` rebinds a lost listener | **added** |
| `max_restarts` / `max_seconds` | `restartBudget`, 5 per minute, then exit non-zero | **added** |
| GenServer for state | the pending-request map | **declined** |

The last row is the one worth arguing. Turning `pending` into a
GenServer-style owning goroutine would be the most literal translation and the
least useful: it is twenty lines, mutex-guarded, race-tested, and an actor would
add a hop while fixing no bug. Same reasoning as
[[Atomics-Based Rate Limiter Design]] choosing atomics over a GenServer — a
process earns its place when it buys concurrency or isolation, and here it buys
neither.

### Escalation, not infinite retry

The supervisor rebinds the socket on loss, backing off 100 ms → 2 s. After five
restarts inside a minute it stops and exits non-zero. Restarting forever would
hide a permanent fault behind the same silence the change was meant to remove;
giving up makes it visible.

**Why self-heal rather than "let it crash" and be respawned?** Because the
supervisor above us is unverified. Claude Code's binary contains
`maxReconnectAttempts` and *"transport closed/disconnected, attempting automatic
reconnection"*, but whether it respawns a dead stdio MCP server has never been
tested. A fail-fast design would have been betting the bridge's availability on
that. Self-healing works either way, and still exits when the fault is permanent.

### Realistic trigger — and the reason the first attempt could not detect it

The socket lives under `/tmp`, which macOS periodically sweeps. Losing the path
leaves exactly the silent-degradation state above.

**The first implementation could never fire on it.** It watched for `Accept` to
return, on the assumption that losing the socket file would fail the accept loop.
It does not. A Unix listener holds the *inode*; unlinking the path leaves `Accept`
blocked indefinitely while every new dial gets `ENOENT`. Verified directly on
Go 1.26.6 / macOS: after `os.Remove(sock)`, `Accept` was still blocked two
seconds later.

Worse, the test passed. It simulated the sweeper with `ln.Close()` — which
unlinks the path as a side effect — so it exercised a completely different
failure. The watchdog could have been absent and the suite would still have been
green.

The fix is an explicit path watchdog: poll `Lstat(sock)` alongside the accept
loop and, when the path is gone, close the listener ourselves to unblock `Accept`
and hand control to the restart loop. See §16.

---

## 16. External review (rev. 10)

A full adversarial review by a separate agent on a different model, briefed with
the four load-bearing constraints so it would not "helpfully" recommend an MCP
SDK. No critical findings; six confirmed defects, all now fixed.

| # | Severity | Defect |
|---|---|---|
| 1 | high | The socket watchdog watched the wrong signal — unlinking a socket does not fail `Accept`, so the sweeper case it existed for could never trigger it. Its test modelled the sweeper as `ln.Close()` and so passed regardless. |
| 2 | med-high | `tools/call` was handled on the read loop, so `ask_amp` stalled the entire transport for up to 5 minutes — no ping, no `reply`, not even stdin EOF noticed. |
| 3 | medium | `ensureRuntimeDir` did MkdirAll → Chmod → symlink check. Both mutating calls follow symlinks, so the refusal came *after* chmod 0700 had been applied to an attacker-chosen directory. |
| 4 | med-low | The supervisor never closed the old listener before rebinding, so `bindSocket`'s liveness probe dialled *our own* listener, concluded another bridge owned the path, and refused to hijack it — burning the whole budget and blaming a process that did not exist. |
| 5 | low-med | At the deadline boundary Claude could be told "delivered" while Amp was told "timed out", losing the answer in between. |
| 6 | low | `AGENTS.md` documented a triage message the Amp side can never observe — the `request_id is required` guard reports to Claude, not to the caller. |

### What the two highest findings have in common

Both are *tests that could not fail*. Finding 1's watchdog test passed with the
watchdog removed; a first attempt at finding 2's regression test started its
clock after the blocking call returned, so it also passed against the unfixed
code. Both were caught by mutation-checking: break the fix, confirm the test goes
red, restore. Any test written to pin a subtle concurrency or syscall behaviour
should be mutation-checked before it is trusted — otherwise it is documentation,
not verification.

### Also changed

`ask_amp` turns are now serialised (concurrent `threads continue` runs against
one thread would interleave writes into a single conversation), thread ids are
validated so a leading `-` cannot be read as a CLI flag, an empty `reply` text is
rejected rather than delivered as a blank answer, and the supervisor re-checks
for shutdown after its backoff sleep so it cannot recreate the socket file
microseconds before exit.

89 tests, 78.5% coverage.

### Deferred, with reasons

- **Registry published before the handshake completes.** A very fast `--ask` in
  that window pushes an event Claude may drop, costing the caller a full timeout.
  Real, but the fix couples registry publication to `notifications/initialized`;
  worth doing deliberately rather than as a review sweep.
- **No connection cap or idle deadline on the socket.** An idle local client
  holds a goroutine and an fd indefinitely, and the JSON decoder buffers a large
  payload before the size check sees it. Same-uid exposure only.
- **Empty-`request_id` fallback can misroute across a timeout boundary.** If A
  times out and B becomes the only pending request, a belated id-less answer to A
  is delivered to B. Requires Claude to violate its own tool schema, which is why
  the fallback exists at all.
- **Client read deadline uses the *client's* `AMP_BRIDGE_TIMEOUT`.** A server
  started with a longer timeout than the client's environment produces
  `no reply: i/o timeout`, which reads as a dead socket.

---

## 17. Peer review over the bridge, and the installation story (rev. 11)

The second review came from Amp, and it arrived **through the bridge** — a 3.6 KB
channel event, answered with a 4.5 KB `reply`. The artefact reviewed itself over
itself. Worth recording because it is the first use of the thing for the thing it
was built for, and because the round trip (10:52:32 → 10:54:00, 88 s) is a fair
sample of real turn latency: almost all of it is Claude thinking, not transport.

### Findings fixed

| # | Defect | Fix |
|---|---|---|
| 1 | `pushEvent` ignored the result of `send`. A failed write left the request registered, so the caller waited the full 180 s for an event that never left the process. | `send` returns an error; on failure the slot is dropped and the caller is told immediately. |
| 2 | Registry publication failure was non-fatal — the bridge ran healthy and undiscoverable, which is exactly the silent-failure class this design keeps tripping over. | Fatal. Fail loudly at startup instead. |
| 3 | `listBridges` swallowed the symlink refusal from `trustedRuntimeDir` and reported "no live sessions". A planted `/tmp` symlink read as *nothing is running*, not as *something is wrong*. | The error propagates; `--ask` and `--list` exit 2 with the refusal. |
| 4 | The empty-`request_id` fallback (deferred in §16) could hand A's belated answer to B across a timeout boundary. | Removed entirely. `request_id` is required by the tool schema, so inferring it only ever accommodated a client already violating the contract — at the cost of the mis-routing correlation exists to prevent. |
| 5 | `askAmp` derived its context from `Background`, so a shutdown left `amp threads continue` running as an orphan. | Derived from the bridge context, with a distinct "shutting down" error so cancellation is not misreported as an Amp failure. |
| 6 | Long session names overflowed `sockaddr_un`'s ~104-byte limit; `bind` then failed with an error naming neither the length nor the culprit. | `capSocketName` caps the name against the directory length before binding. |

### Routing, not guessing

Amp's substantive request was addressing: with one thread the "reply to whoever
messaged last" fallback is fine, with two it is wrong. `--thread` now rides along
in the notification meta, so the event Claude sees is

```
<channel source="amp-bridge" request_id="amp-…-2" thread_id="T-abc123">
```

and `ask_amp` can be pointed at the originating thread rather than the most
recent one. This is the first use of `meta` for anything beyond correlation, and
it confirms the field takes arbitrary identifier keys, not just `request_id`.

### `doctor` — making the quiet failures loud

Every failure this project has hit reports healthy from the inside. The channel
not loaded, `.mcp.json` pointing at a stale build, a binary whose signature was
invalidated by `cp`, a registry entry nobody published: in all of them the
process runs, the logs look ordinary, and messages simply never arrive.

`amp-bridge doctor` checks the six places that can be individually fine while the
system is broken — binary on PATH vs. `os.Executable()`, `.mcp.json` target vs.
this build, runtime directory trust, live sessions, the Amp CLI, and log
freshness — prints the fixing command for anything wrong, and exits non-zero.

The check that earns its keep is `.mcp.json` drift. `make build` does not change
what a live session launches, and neither does `make install` without a restart;
the resulting "I fixed it and nothing changed" is the most common self-inflicted
confusion here. `doctor` compares the configured path against the running binary
and says so:

```
[FAIL] mcp config  points at /nonexistent/amp-bridge, which does not exist
       fix: amp-bridge init
```

Verified by breaking it deliberately: no `.mcp.json` → warn, exit 0; stale path →
FAIL, exit 1; `amp-bridge init` → repaired, recheck green.

`init` merges into an existing `.mcp.json` rather than overwriting it (other MCP
servers survive) and refuses a file it cannot parse rather than clobbering it.
`make setup` is build + install + init in one step, with `PROJECT=` to register
the bridge in a different project.

99 tests, 161 including subtests. Docs split three ways by reader: `README.md` for a human installing it,
`AGENTS.md` for Amp driving it, `SKILL.md` for Claude answering on it.

---

## 18. `ask_amp` cannot reach a thread that is open (rev. 12)

The report from §17 could not be delivered. `ask_amp` failed, twice, with:

```
Error: Unexpected error inside Amp CLI.
Use 'amp threads report T-01a01877-…' to generate a diagnostic report
```

I had already diagnosed this once, wrongly, as "the thread is wedged on Amp's
side" — the guess the error message invites, and it survived because the thread
*was* unusable and no other thread was tried. It is not wedged. Amp's structured
log (`~/.cache/amp/logs/cli.log`, or wherever `--log-file` points) says exactly
what happened:

```json
{"level":"ERROR","message":"[thread-client] Executor handshake rejected by server",
 "code":"EXECUTOR_ALREADY_CONNECTED",
 "existingExecutorInfo":{"capabilities":{"workspaceId":"/Users/…/amp_claude","pid":50778}}}
```

**Amp allows one executor per thread.** PID 50778 was a live interactive `amp`
that had been sitting in that thread for nine hours — the very session that sent
the review. `threads continue --execute` tries to attach a second executor and is
refused, non-retryably.

Reproduced outside the bridge with the identical argv and `stdin < /dev/null`, so
this is Amp's constraint, not the bridge's invocation of it. (A direct run with a
terminal stdin fails *differently* — `Timeout while reading from stdin` — which is
what made the earlier diagnosis look plausible: two failures, two messages, one
misattributed cause.)

### Why this matters more than it sounds

It hits the primary use case exactly. The thread the bridge most wants to reach
is the one the human is actively working in, and that is precisely the thread
that has an executor attached. `ask_amp` therefore works for *quiescent* threads
and never for the live one.

The bidirectional design still holds — it is just asymmetric in a way now worth
stating plainly:

| Thread state | Amp → Claude | Claude → Amp |
|---|---|---|
| Open in an interactive session | `amp-bridge --ask` ✓ | `ask_amp` ✗ (`EXECUTOR_ALREADY_CONNECTED`) |
| Nobody has it open | `amp-bridge --ask` ✓ | `ask_amp` ✓ |

For a live thread the inbound direction is the whole bridge: Amp asks, Claude
answers with `reply`. Which is what worked at 10:52 and what the design was
originally built around — the outbound direction was the later addition, and this
is the boundary it ran into.

### The fix that was possible

The constraint is Amp's and cannot be worked around. What could be fixed is the
bridge repeating an error that names nothing:

`askAmp` now runs amp with `--log-file` pointing at a private temp file, and on
failure parses it. `EXECUTOR_ALREADY_CONNECTED` becomes

> that thread is already open in an Amp session (pid 50778) — Amp allows one
> executor per thread, so ask_amp cannot attach a second one. Ask from that
> session instead (it can reach this bridge with `amp-bridge --ask`), or pass
> thread_id for a thread nobody has open

with a generic fallback to the first `level:ERROR` message, and silence when the
log knows nothing — so amp's own stderr still shows through rather than being
replaced by an invented cause. A private log file rather than the shared one
keeps other concurrent `amp` processes out of the parse.

Tested against both real captured logs and four synthetic fixtures. The
tail-truncation test was mutation-checked: reading the head instead of the tail
turns it red.

### The lesson, again

§16's finding was a test that could not fail. This one is the same species one
level up: **a diagnosis that was never falsified.** "The thread is wedged" fit
every observation I had, and I never ran the one experiment — a different thread,
or the log — that could have contradicted it. The tell was there in plain sight:
the CLI prints `Use 'amp threads report …'`, which is an invitation to read the
diagnostic, and I treated it as boilerplate.

When a tool says its error is unexpected, that is a claim about the tool's
self-knowledge, not about the cause. Find where it writes what it does know.

---

## Open questions

1. ~~Which protocol era must a channel server negotiate?~~ **Answered:** Claude proposes `2025-11-25`
   (modern, no delivery path); server must negotiate down. Whether `2025-06-18` is accepted by the
   client's negotiator is the remaining half.
2. ~~Console or Teams/Enterprise?~~ **Answered:** neither — `claude_max`, and channels are
   policy-blocked pending a managed-settings file.
3. ~~Payload schema~~ **Partially answered:** `content` + `_meta`, injected as a `next`-priority
   prompt; `/permission` carries `request_id`. The reply direction (what tool shape Claude expects to
   call back through) is still unverified — the probe exposes `reply_to_amp` to find out.
4. Does the client accept a server-initiated downgrade to `2025-06-18`, or does it hard-fail /
   re-offer? If it refuses, `MCP_PROTOCOL_NEGOTIATION=legacy` on the Claude side is the fallback lever.
5. Can `crossSessionInbound` be scoped per-project? (`localSettings`/`projectSettings`/`repoSettings`
   are all in the chain, so probably.)
6. Behaviour when the bridge's Amp thread is mid-turn — does `amp threads continue --execute` queue or
   reject?

## Method note

Findings come from `strings` over the 299 MB bun-compiled binary, cross-checked against live
filesystem state, the `SendMessage` tool contract, and `--help` output. Minified identifiers were
traced by hand. Claims resting on a single decompiled expression rather than observed behaviour are
marked as such. No live-session messaging was performed.

**Changelog.** Rev. 12: added §18 — the `ask_amp` failure was never a wedged thread; Amp permits one executor per thread and an open interactive session holds it, so the outbound direction cannot reach the very thread the bridge most wants; corrected the earlier misdiagnosis, documented the asymmetry, and made `askAmp` parse amp's own log so the real cause is reported. Rev. 11: added §17 — peer review received over the bridge itself; fixed six findings (unchecked `send` result, non-fatal registry publish, swallowed symlink refusal, the empty-`request_id` fallback removed outright, orphaned Amp subprocess on shutdown, `sockaddr_un` overflow); added `thread_id` routing in the notification meta, and `doctor`/`init`/`make setup` so the quiet failure modes announce themselves. Rev. 10: added §16 — external adversarial review; fixed six confirmed defects, the worst being that the socket watchdog watched a signal that can never fire (unlinking a Unix socket does not fail Accept) and its test simulated the wrong failure; corrected the §15 rationale accordingly. Rev. 9: added §15 — ported OTP supervision to the Go bridge after live testing of ask_amp; added panic containment (`guard`), listener supervision with rebind, and a bounded restart budget that escalates by exiting rather than retrying forever; declined the GenServer-for-state translation with reasoning. Rev. 8: added §14 — removed the superseded Python probe, the `AMP_BRIDGE_PROTOCOL` escape hatch (built on the wrong reading of the era trap) and other dead code; moved configuration out of package globals into a `config` struct so tests are hermetic; replaced the Python e2e harness with a two-tier Go suite (66 tests, `-race`, 77.7% coverage) after validating the refactor against the old harness; added `.golangci.yml` (29 linters) and a `make check` gate. Rev. 3: added §10 — built and ran an instrumented probe server; captured Claude's real
MCP handshake (`2025-11-25`, modern era), confirmed the era trap and the `not-in-era` mechanism, pinned
the channel payload fields, resolved account eligibility (policy-blocked on `claude_max`), and found
the `--scope local` clobber. Rev. 2: added §6 Channels (missed in rev. 1 — caught by the Amp-side research);
corrected §5 from "plan/prompting hold" to mode-class parity, and identified `no-mode-asserted` as the
real bridge blocker; downgraded synthetic peer registration (§4) from recommendation to marked-
experimental; restaged §7 around Channels.
