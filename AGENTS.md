# amp_claude — Amp ↔ Claude Code bridge

This project contains **amp-bridge**: a Go binary that lets an Amp thread and a
live Claude Code session talk to each other, in both directions, on this machine.

If you are Amp working in this repo, this file tells you how to use it.

## What it is

`amp-bridge` runs as a Claude Code **channel** — an MCP server that can push
unsolicited events into a running Claude session and receive answers back. It
listens on a per-session Unix socket under `/tmp/amp-bridge-<uid>/`.

```
Amp ──unix socket──▶ amp-bridge ──notifications/claude/channel──▶ Claude session
Amp ◀──unix socket── amp-bridge ◀────── reply tool ────────────── Claude session
```

Claude can also initiate requests. `ask_amp` waits for an Amp turn; `send_amp`
returns a handle immediately and pushes the result back as a later channel event.
Both use the live inbox plugin when available and otherwise shell out to
`amp threads continue <id> --execute`. The CLI fallback starts an unheld thread;
the plugin can append into an open thread but cannot wake it while idle.

## Using it

`amp-bridge` is installed on PATH (`make install`), so these work from any
directory — you do not need to be in this repo:

```bash
amp-bridge --list                          # which Claude sessions are reachable
amp-bridge --ask "your message"            # send + block until Claude answers
amp-bridge --session <name> --ask "..."    # target a specific session
amp-bridge --thread <amp-thread-id> --ask "..."   # let Claude reply into your thread
amp-bridge --timeout 10m --ask "..."        # request a longer server deadline
```

`--ask` prints Claude's answer on stdout and exits 0. **Always run `--list`
first** — it names the live sessions and confirms the channel is loaded. If
exactly one session is live, `--session` is optional.

Pass `--thread` with your own Amp thread id when you want Claude to be able to
call an outbound tool back into *this* thread. The id rides along on the event Claude
receives (as a `thread_id` attribute on the `<channel>` tag), so Claude can
address the right thread even when several are talking to one session. Without
it `ask_amp` falls back to whichever thread messaged it last — fine for a single
thread, wrong as soon as there are two. `send_amp` always requires an explicit id
and never changes that remembered pair.

## Timing

`--ask` blocks while Claude thinks. Typical round trips are 5–30 s; the timeout
is 180 s. This is Claude's turn latency, not bridge overhead — the bridge's own
hops are under a millisecond. An idle session answers immediately; a busy one
delivers the event between tool calls, which is where the variance comes from.

Run parallel asks as background jobs if you need concurrency. Note that **they
do not arrive in launch order** — `&`-launched jobs race to the socket. Never
assert "the first job got the first answer"; each response carries its own
`request_id`, and that is the only correct way to pair question with answer.

For Claude→Amp concurrency, use `send_amp(text=…, thread_id=…)`. It returns an
`amp-async-…` handle; Claude must end that turn, then receives success or failure
as a `<channel async_id="…" status="…">` completion event. No `reply` call is
needed for a completion. Outstanding completions are in memory and are lost if
the MCP server restarts. Against an open idle thread, expect a `no-turn` error:
the inbox appended the request, but Amp exposes no plugin wake primitive.

## Limits, and what they mean

| Limit | Default | Behaviour when exceeded |
|---|---|---|
| In-flight requests | 8 | `too many requests in flight` — Claude hasn't answered earlier ones |
| Async Amp turns | 8 | `too many send_amp requests in flight` — wait for a completion |
| Message size | 64 KB | `message too large` — send a summary or a file path instead |
| Reply timeout | 180 s (15 min max) | timeout returns an id; retrieve a late reply with `--result` |
| Retained late replies | 1 h / 64 | expiry or bridge restart removes the in-memory result |
| ask_amp turn timeout | 120 s | `ask_amp` fails; the message may still be in the thread |
| send_amp turn timeout | 10 min | completion arrives as `pending`, not `error`, when it was delivered |

All are env-tunable (`AMP_BRIDGE_MAX_INFLIGHT`, `AMP_BRIDGE_MAX_BYTES`,
`AMP_BRIDGE_TIMEOUT`, `AMP_BRIDGE_MAX_TIMEOUT`, `AMP_BRIDGE_RESULT_TTL`,
`AMP_BRIDGE_MAX_RESULTS`, `AMP_BRIDGE_AMP_TIMEOUT`, `AMP_BRIDGE_SEND_TIMEOUT`)
but the defaults exist to stop a
runaway loop from flooding someone's session. Raise them deliberately, not
reflexively; `doctor` warns if the Amp timeout leaves 30 s or less before the
Claude reply deadline.

## Failure triage

Start with `amp-bridge doctor`. It checks the binary, `.mcp.json`, the runtime
directory, live sessions, the Amp CLI and the log, prints the fix for anything
broken, and exits non-zero if a check actually failed. It is faster than reading
the table below and it catches the case the table cannot: a `.mcp.json` pointing
at a stale build, where everything reports healthy and nothing is delivered.

| Exit / message | Meaning |
|---|---|
| `no live amp-bridge sessions` | No Claude session has the channel loaded — it must be started with `claude --dangerously-load-development-channels server:amp-bridge`. Nothing else will work until then. |
| exit 2, `cannot reach <name> at <path>` | Registry entry exists but the socket is dead; the bridge process died. |
| exit 1, `timed out waiting for Claude` | Socket fine and event delivered, but no timely `reply`. Use the printed `--result` command; a late answer is retained in memory until expiry unless the bridge restarts. |
| exit 1, `timed out waiting for Claude` *after* Claude appeared to answer | Claude replied without a `request_id` while several requests were in flight. The bridge refuses to guess and tells Claude so; if Claude does not retry with the id, your call just times out. You never see the guard message itself — it goes to Claude, not to you. |
| `another CLI fallback turn … is already in flight` | The same idle thread is already running a bridge-started turn. Wait, or enable its inbox when turns need to queue. |
| exit 137 | The binary was replaced with `cp`, invalidating its macOS code signature. Rebuild, don't work around it. |

Server-side view: `~/.local/state/amp-bridge/amp-bridge.log` — look for `EVENT_PUSHED`,
`REPLY_TOOL`, `AMP_REPLIED`, `AMP_TIMEOUT`, `AMP_ABANDONED`. It logs frame shape
only, not message content.

## Etiquette

Claude's session runs with the user's full permissions on their machine. Ask it
for information and analysis freely; for consequential actions — deleting,
pushing, sending, spending — say what you want and let Claude confirm with its
user rather than phrasing it as a command. Don't ask the other side to do
something your own permissions blocked; both agents act as the same user, and
routing around a denial defeats the point of it.

Keep messages self-contained. Claude sees the text and nothing else — not your
thread history, not your files, not your tool output.

`ask_amp` and `send_amp` can reach a thread you have open **only if that thread's
inbox is enabled**. Amp permits one executor per thread and your interactive
session holds it, so `threads continue --execute` cannot attach a second; the
inbox plugin gets around that by appending through the executor your own session
already holds. Appending does not start an idle turn. The user must start the
next activity; never resend after `no-turn`, because the request is already there
and that later activity may pick it up.
Install it with `amp-bridge init --amp-plugin --global`, reload plugins, then
`Ctrl+O` → `amp-bridge: Enable Claude inbox for this thread`. It is off by default
on every new thread, and only the user can turn it on. For a managed/background
thread with no palette of its own, use `Ctrl+O` → `amp-bridge: Enable Claude inbox
for another thread` from a local Amp thread and paste the target URL/id. That
local thread becomes the explicit controller; unrelated hosts cannot claim the
consent. Once enabled, the target stays opted in across plugin reloads and returns
after local Amp process restarts when the controller thread reopens, until the
matching Disable command revokes it. Reopening the managed target alone does not
transfer ownership. Named enablement always makes the target addressable and
delivers automatically while it is running. An idle target cannot be awakened by
the current Amp plugin API; its request stays queued and the controller is
notified to continue it. If the controller is deleted, the Disable command from
another local thread can explicitly revoke the pairing after confirmation.

Without an inbox, the inbound direction is the only one that works for a thread
you are sitting in — you call `amp-bridge --ask`, Claude answers with `reply`.
Claude gets a diagnostic naming the pid holding the thread and pointing at the
plugin, rather than Amp's `Unexpected error inside Amp CLI`.

**If Claude started the exchange with `ask_amp`, do not call `amp-bridge --ask`
in that same turn.** Claude's session is blocked inside the tool call until your
turn ends, so it cannot answer an inbound event; your `--ask` would block until
it timed out at 180 s. Just answer in your turn output — that is what Claude
receives. `amp-bridge --list` is fine either way, since it only reads the
registry.

The Claude-turn deadlock does not apply to `send_amp`: it returns before Amp
runs. Its message must be self-contained, because there is no synchronous
follow-up or polling tool; the result arrives as a later event. This does not
solve Amp-side wake-up for an open idle thread.

## Building and testing

Toolchain is pinned in `.tool-versions` (mise): Go 1.27.0,
golangci-lint 2.13.1 and Bun 1.4.0 — Bun only to typecheck the Amp plugin. Run
`mise install` once; `make tools` reports the active versions and warns if they
have drifted from the pins.

```bash
mise install           # once — pins Go and golangci-lint from .tool-versions
make setup             # build + install to ~/.local/bin + register in ./.mcp.json
make check             # tidy, both drift gates, plugin typecheck, format, vet,
                       #   lint, govulncheck, both test tiers — the gate
```

The Makefile and `go.mod` live at the repo root; the Go sources are in
`amp-bridge/` and build to `bin/amp-bridge`. Run every `make` target from the
root.

`make setup` is build, install and `amp-bridge init` in one step; the pieces are
still available separately as `make build` / `make install` / `make doctor`.
`make setup PROJECT=$HOME/somewhere` registers the bridge in a different
project's `.mcp.json` instead of this repo's, and `make setup-global` registers
it for every project at once (user-scope MCP entry plus the skill).

`amp-bridge doctor --strict` treats warnings as failures, for use as a gate
rather than a report.

**The module has no dependencies and must not gain one.** `make outdated` will
always be empty. An MCP SDK would implement `server/discover`, which wins the
modern handshake negotiation and silently kills channel delivery — the reason
the transport is hand-rolled. `go.mod` carries this warning too.

`make install` is what the running channel uses: the MCP config points at the
installed binary, not the one in `bin/`. Building alone changes nothing a
live session sees — install, then restart the Claude session.

Individually: `make test` (unit, race detector on), `make test-integration`
(spawns a real bridge and drives both ends), `make lint`, `make cover`.
Neither tier needs the network or a live Claude session.

Rebuilding never disturbs a running session — the live bridge keeps its original
inode until that session restarts.

Run `make check` before handing a rebuilt binary to a live session. It is the
same set CI would run: `go mod tidy -diff`, `golangci-lint fmt --diff`, `go vet`
on both build tags, `golangci-lint run` (29 linters), and both test tiers under
`-race`.

## Before changing the protocol

Three things look like bugs and are deliberate. The reasoning is in
`.claude/research/2026-08-19-amp-claude-code-bridge.md`; read it first.

1. **`server/discover` is intentionally unimplemented.** Answering it negotiates
   the modern MCP era, which has no delivery path for unsolicited custom
   notifications. Channels then silently stop working while every health check
   still passes.
2. **Capability must be `experimental: {"claude/channel": {}}`** — not a
   top-level capability key.
3. **The notification field is `meta`, not the MCP-standard `_meta`.** Keys must
   be identifiers, values render as attributes on the `<channel>` tag, and
   `source` is reserved because Claude sets it itself.
