# Channel DX/UX opportunities for amp-bridge

Sources: Claude Code `CHANGELOG.md` (6060 lines, fetched 2026-08-31), the
[channels reference](https://code.claude.com/docs/en/channels-reference), and
`claude --help` on 2.1.251. Written while the working tree held Amp's
per-request-timeout / late-reply-retention work.

## Channel feature timeline

| Version | Change |
|---|---|
| 2.1.80 | `--channels` research preview — MCP servers can push messages into a session |
| 2.1.81 | Permission relay: channels declaring the permission capability receive tool approval prompts |
| 2.1.83 | `AskUserQuestion` and plan-mode tools disabled when `--channels` is active |
| 2.1.105 | Fixed inbound notifications silently dropped after the first message (Team/Enterprise) |
| 2.1.126 | Fixed plan-mode tools being unavailable in interactive `--channels` sessions |
| 2.1.128 | `--channels` works with console (API key) auth |
| 2.1.187 | **Fixed channel connections dropping after the agents view, `/bg`, `/tui`, or `/update`** |
| 2.1.234 | Relay restricted to servers admitted by the inbound trust gate; explicit permission-capability opt-out honored; credential masking on relayed previews |
| 2.1.251 | Current |

2.1.187 matters for our docs: before it, a channel session that visited the
agents view or ran `/bg`, `/tui` or `/update` lost its channel silently. That is
a distinct cause of "the bridge went quiet" from anything `doctor` checks, and it
is fixed — so a **minimum recommended version** is more useful guidance than the
"last tested against 2.1.246" line the README carries.

`AskUserQuestion` was disabled for `--channels` sessions in 2.1.83 and no
changelog entry reverses it, yet it is present in this channel session's tool
list. Undocumented either way; do not assert it in the README.

## Ranked opportunities

### 1. Permission relay — the biggest missing piece

`capabilities.experimental['claude/channel/permission']`. amp-bridge declares only
`claude/channel` (mcp.go:378), which since 2.1.234 correctly reads as opted out.

Today, when an Amp-initiated request makes Claude call a tool needing approval,
the terminal dialog opens and the session waits. Amp sees nothing, burns its whole
reply window, and gets `timed out waiting for Claude` — an outcome
indistinguishable from Claude being wedged. This is the worst remaining failure in
the loop because the diagnosis is wrong, not just slow.

Protocol:

- Claude Code sends `notifications/claude/channel/permission_request` with
  `request_id` (five lowercase letters, no `l`), `tool_name`, `description`,
  `input_preview`.
- The server replies with `notifications/claude/channel/permission` carrying the
  echoed `request_id` and `behavior: 'allow' | 'deny'`.
- The local dialog stays live throughout; first answer wins, the other is dropped.
- Fields are sanitized from 2.1.211 (3,500 code points per top-level field,
  whitespace folded, direction-override characters neutralized) and credential-
  masked from 2.1.234. Masking never hides the command, path, or destination.

Two build stages, and the first is the valuable one:

- **Visibility only.** Handle `permission_request`, do not answer it, and push a
  channel event to the waiting Amp thread: "Claude is blocked on approval for
  Bash: …". Amp then reports a real cause instead of a timeout. No new authority
  is granted, so the security question does not arise.
- **Remote approval.** Let the Amp side return a verdict. The docs' caveat is
  "only declare the capability if your channel authenticates the sender, because
  anyone who can reply through your channel can approve or deny tool use in your
  session." amp-bridge's gate is a uid-scoped Unix socket under
  `/tmp/amp-bridge-<uid>` — a stronger boundary than the chat platforms the docs
  are written for. The real question is not transport but that the approver would
  be an LLM. Should be a per-session opt-in, off by default, and probably a
  separate flag from enabling the bridge at all.

Note the capability is declared once at construction, so declaring it turns relay
on for every session using the bridge. If approval is to be opt-in, the opt-in has
to live in the handler, not the capability.

### 2. Catch the missing flag at session start

Forgetting `--dangerously-load-development-channels` produces total silence: the
MCP server still loads from `.mcp.json`, still registers, still completes the
handshake and sets `InitializedAt`. Only the channel listener is absent, and the
docs confirm the server gets no signal — "Claude Code drops the events silently
and returns no error to your server." So the registry cannot distinguish the two
cases, and neither can `doctor` from the bridge side.

A `SessionStart` hook can, by inspecting its own ancestor process's command line
for the flag, and emitting a warning when the bridge is configured but the flag is
absent. `amp-bridge init` already writes Claude-side config; installing the hook
there would close the loop on the single most common setup failure.

### 3. Document the batching semantics

"Events queue into the session and are processed in order. If several
notifications arrive while Claude is busy, they're delivered together on the next
turn and Claude handles them as a group. To process independent event streams
concurrently, run separate sessions." That explains grouped handling and is a
better answer than the README's current framing for "can the two agents work in
parallel".

### 4. Guard the meta-key rule in code

Meta keys must be `[A-Za-z0-9_]`; hyphenated keys are **silently dropped**. Our
keys (`request_id`, `thread_id`, and Amp's new `timeout_ms`) are all valid, but
nothing in the repo stops someone adding `amp-thread` and losing it without an
error. A test over the meta map is cheap insurance.

### 5. Status line

Status line scripts receive a JSON payload that has grown steadily
(`rate_limits` in 2.1.80, `prompt_cache` and `rate_limits.spend_limit` in
2.1.251). An `amp-bridge statusline` segment showing pending inbound requests
would make an in-flight Amp question visible while Claude works. Needs a way to
read the live pending count out of the serve process — the registry file does not
carry it today.

## Not viable

Replacing the development flag. See
[[Claude Code channel gating: --channels vs --dangerously-load-development-channels]]
— the docs are explicit that a self-published channel plugin still needs the flag
while channels are in research preview.
