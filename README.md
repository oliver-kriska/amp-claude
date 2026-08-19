# amp_claude

**amp-bridge** lets an [Amp](https://ampcode.com) thread and a live Claude Code
session talk to each other, in both directions, on your machine.

```
Amp ──unix socket──▶ amp-bridge ──notifications/claude/channel──▶ Claude session
Amp ◀──unix socket── amp-bridge ◀────── reply tool ────────────── Claude session
```

Amp asks a question and blocks until Claude answers. Claude can start the
conversation too, by calling `ask_amp`, which runs a turn in the Amp thread.
Neither side needs a server, an API key, or a network connection — it is one
~4 MB Go binary, a Unix socket, and a registry file under `/tmp`.

It works by registering as a Claude Code **channel**: an MCP server permitted to
push unsolicited events into a running session. Channels are an experimental,
undocumented extension point, which is why the launch flag below says
`--dangerously-load-development-channels`.

## Install

Requires Go 1.26+ ([mise](https://mise.jdx.dev) will fetch the pinned toolchain
from `.tool-versions`) and, for the outbound direction, the `amp` CLI on PATH.

```bash
mise install                # once, at the repo root — pins Go + golangci-lint
cd amp-bridge
make setup                  # build, install to ~/.local/bin, register in .mcp.json
```

`make setup` registers the bridge in the repo root's `.mcp.json`. To use it from
another project instead:

```bash
make setup PROJECT=~/Projects/other-app
```

Then start Claude Code **with the channel flag** — this is the part that is easy
to miss, and without it nothing is delivered:

```bash
claude --dangerously-load-development-channels server:amp-bridge
```

Verify:

```bash
amp-bridge doctor
```

```
  [ok  ] binary          /Users/you/.local/bin/amp-bridge
  [ok  ] mcp config      /Users/you/.local/bin/amp-bridge
  [ok  ] runtime dir     /tmp/amp-bridge-501
  [ok  ] live sessions   amp-claude-32 (claude_pid=78531)
  [ok  ] amp cli         /Users/you/.amp/bin/amp
  [ok  ] log             ~/.local/state/amp-bridge/amp-bridge.log (last write 9s ago)
```

`doctor` exits non-zero if anything is actually broken, and every failure line
carries the command that fixes it. Run it first whenever the bridge seems dead —
the failure modes here are quiet ones, and it is built to name them out loud.

## Use it

**From Amp** (or any shell) — send a message and block until Claude answers:

```bash
amp-bridge --list                                # which sessions are reachable
amp-bridge --ask "what does this repo do?"       # send, wait, print the answer
amp-bridge --session amp-claude-32 --ask "..."   # target one of several
amp-bridge --thread T-abc123 --ask "..."         # let Claude reply into this thread
```

**From Claude** — two tools appear in the session:

- `reply(request_id=…, text=…)` answers an event Amp sent. The `request_id` is on
  the `<channel>` tag of the event being answered.
- `ask_amp(text=…, thread_id=…)` starts a turn in an Amp thread and returns what
  Amp says.

Claude's transcript output never reaches Amp. Only these tools cross the bridge.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `AMP_BRIDGE_MAX_INFLIGHT` | 8 | unanswered events allowed at once |
| `AMP_BRIDGE_MAX_BYTES` | 65536 | max bytes per message |
| `AMP_BRIDGE_TIMEOUT` | 3m | how long `--ask` waits for Claude |
| `AMP_BRIDGE_AMP_TIMEOUT` | 5m | how long `ask_amp` waits for Amp |
| `AMP_BRIDGE_LOG` | `~/.local/state/amp-bridge/amp-bridge.log` | log file |
| `AMP_BRIDGE_LOG_BODIES` | unset | `1` also logs frame bodies (conversation text) |
| `AMP_BRIDGE_DIR` | `/tmp/amp-bridge-<uid>` | socket + registry directory |
| `AMP_BRIDGE_SOCKET` | — | explicit socket path (single-session mode) |
| `AMP_BRIDGE_DISABLE_OUTBOUND` | unset | `1` removes the `ask_amp` tool |
| `AMP_BIN` | `amp` | Amp CLI to invoke |

The in-flight and size caps exist so a runaway agent loop cannot flood someone's
session. Raise them deliberately.

## Development

```bash
make check                # tidy, format, vet, 29 linters, both test tiers under -race
make test                 # fast unit pass
make test-integration     # spawns a real bridge process and drives both ends
make doctor               # diagnose the installed binary
```

Neither test tier needs the network or a live Claude session.

`make build` alone changes nothing a running session sees: `.mcp.json` points at
the *installed* binary, and a live bridge keeps its original inode regardless.
New code takes effect on `make install` **plus** a session restart.

**The module has no dependencies and must not gain one.** An MCP SDK would
implement `server/discover`, which wins handshake negotiation and silently kills
channel delivery — which is why the transport is hand-rolled. See `go.mod`.

### Three things that look like bugs

They are load-bearing. The reasoning, and how each was found, is in
[`.claude/research/2026-08-19-amp-claude-code-bridge.md`](.claude/research/2026-08-19-amp-claude-code-bridge.md).

1. **`server/discover` is deliberately unimplemented.** Answering it negotiates
   the modern MCP era, which has no delivery path for unsolicited custom
   notifications. Channels then stop working while every health check still
   passes.
2. **The capability is `experimental: {"claude/channel": {}}`**, not a top-level
   capability key.
3. **The notification field is `meta`, not the MCP-standard `_meta`.** Keys must
   be identifiers, values become attributes on the `<channel>` tag, and `source`
   is reserved — Claude sets it itself.

## Further reading

- [`AGENTS.md`](AGENTS.md) — how Amp should drive the bridge
- [`.claude/skills/amp-bridge/SKILL.md`](.claude/skills/amp-bridge/SKILL.md) — how Claude should
- [`.claude/research/2026-08-19-amp-claude-code-bridge.md`](.claude/research/2026-08-19-amp-claude-code-bridge.md) — the protocol archaeology, in full

## Security notes

The runtime directory is per-uid and 0700; the socket and registry entries are
0600. Both the server and the client refuse the directory if it is a symlink —
`/tmp` is world-writable, and a planted link would otherwise redirect your
prompts into another user's socket. The bridge also refuses to bind over a
socket another live bridge owns.

Both agents run as you, with your permissions. Neither side should route work
through the bridge that its own permission prompt denied.
