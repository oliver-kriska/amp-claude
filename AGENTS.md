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

Claude can also start conversations, via an `ask_amp` tool that shells out to
`amp threads continue <id> --execute`.

## Using it

`amp-bridge` is installed on PATH (`make install`), so these work from any
directory — you do not need to be in this repo:

```bash
amp-bridge --list                          # which Claude sessions are reachable
amp-bridge --ask "your message"            # send + block until Claude answers
amp-bridge --session <name> --ask "..."    # target a specific session
amp-bridge --thread <amp-thread-id> --ask "..."   # let Claude reply into your thread
```

`--ask` prints Claude's answer on stdout and exits 0. **Always run `--list`
first** — it names the live sessions and confirms the channel is loaded. If
exactly one session is live, `--session` is optional.

Pass `--thread` with your own Amp thread id when you want Claude to be able to
call `ask_amp` back into *this* thread. The bridge remembers the last thread id
it saw.

## Timing

`--ask` blocks while Claude thinks. Typical round trips are 5–30 s; the timeout
is 180 s. This is Claude's turn latency, not bridge overhead — the bridge's own
hops are under a millisecond. An idle session answers immediately; a busy one
delivers the event between tool calls, which is where the variance comes from.

Run parallel asks as background jobs if you need concurrency. Note that **they
do not arrive in launch order** — `&`-launched jobs race to the socket. Never
assert "the first job got the first answer"; each response carries its own
`request_id`, and that is the only correct way to pair question with answer.

## Limits, and what they mean

| Limit | Default | Behaviour when exceeded |
|---|---|---|
| In-flight requests | 8 | `too many requests in flight` — Claude hasn't answered earlier ones |
| Message size | 64 KB | `message too large` — send a summary or a file path instead |
| Reply timeout | 180 s | `timed out waiting for Claude` |

All are env-tunable (`AMP_BRIDGE_MAX_INFLIGHT`, `AMP_BRIDGE_MAX_BYTES`,
`AMP_BRIDGE_TIMEOUT`) but the defaults exist to stop a runaway loop from flooding
someone's session. Raise them deliberately, not reflexively.

## Failure triage

| Exit / message | Meaning |
|---|---|
| `no live amp-bridge sessions` | No Claude session has the channel loaded — it must be started with `claude --dangerously-load-development-channels server:amp-bridge`. Nothing else will work until then. |
| exit 2, `cannot reach <name> at <path>` | Registry entry exists but the socket is dead; the bridge process died. |
| exit 1, `timed out waiting for Claude` | Socket fine, event delivered, but Claude never called `reply`. The interesting failure — report it rather than retrying blindly. |
| exit 1, `request_id is required` | Concurrency guard fired; Claude replied without saying which question it was answering. |
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

**If Claude started the exchange with `ask_amp`, do not call `amp-bridge --ask`
in that same turn.** Claude's session is blocked inside the tool call until your
turn ends, so it cannot answer an inbound event; your `--ask` would block until
it timed out at 180 s. Just answer in your turn output — that is what Claude
receives. `amp-bridge --list` is fine either way, since it only reads the
registry.

## Building and testing

Toolchain is pinned in `.tool-versions` (mise): Go 1.26.6 and
golangci-lint 2.12.2. Run `mise install` once at the repo root; `make tools`
reports the active versions and warns if they have drifted from the pins.

```bash
mise install           # once, at the repo root
cd amp-bridge
make build             # rm-then-build: overwriting a Mach-O breaks its macOS signature
make install           # copy to ~/.local/bin (PREFIX= to change)
make check             # tidy, format, vet, lint, and both test tiers — the gate
```

**The module has no dependencies and must not gain one.** `make outdated` will
always be empty. An MCP SDK would implement `server/discover`, which wins the
modern handshake negotiation and silently kills channel delivery — the reason
the transport is hand-rolled. `go.mod` carries this warning too.

`make install` is what the running channel uses: `.mcp.json` points at the
installed binary, not the one in this tree. Building alone changes nothing a
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
