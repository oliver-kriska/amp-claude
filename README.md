# amp-bridge

Let an [Amp](https://ampcode.com) thread (Sourcegraph's coding agent) and a live
[Claude Code](https://claude.com/claude-code) session talk to each other, in both
directions, on your machine.

```
Amp ──unix socket──▶ amp-bridge ──notifications/claude/channel──▶ Claude session
Amp ◀──unix socket── amp-bridge ◀────── reply tool ────────────── Claude session
```

No server, no API key, no network: one ~3 MB Go binary, a Unix socket, and a
registry file under `/tmp`. The module has **zero dependencies** and is built
that way deliberately — see [Three things that look like bugs](#three-things-that-look-like-bugs).

It works by registering as a Claude Code **channel**: an MCP server permitted to
push unsolicited events into a running session. Channels are an experimental,
undocumented extension point, which is why the launch flag below says
`--dangerously-load-development-channels`.

> **Status:** experimental, and built against undocumented internals of Claude
> Code `2.1.235`. It can break on any Claude Code release. `amp-bridge doctor`
> exists to tell you when it has.

## What it's for

Two coding agents on one machine usually means one thing: you, copy-pasting
between two terminals. The bridge removes you from the middle. The workflows
below are real within the constraint described in
[One asymmetry worth knowing](#one-asymmetry-worth-knowing) — the third exists
because of it.

### A second opinion from the session that has the context

You are working in Amp. A Claude session is open on the same repo — it wrote the
code you are touching, or has been living in it all afternoon. Ask it directly
instead of re-explaining:

```
you → Amp:  Ask Claude over the bridge whether hostLimiter is the right
            place for a per-host cap, before I build it there.

Amp runs:   amp-bridge --ask "Is hostLimiter in sync/backoff.go the right
            place for a per-host request cap, or does the token bucket in
            client.go already cover that path?"

Claude:     reply(request_id="amp-…-1", text="client.go's bucket is
            per-client, not per-host, so it won't cap fan-out to one host.
            hostLimiter is the right place — note the retry path also calls
            it, so the cap applies twice on retried requests.")
```

Amp gets the answer inside its turn and keeps working. This direction — Amp asks,
Claude answers — works whatever state either side is in. The first real use of
this bridge was exactly that: Amp code-reviewed this repository through it, and
the findings are in the git history.

### Cross-project questions

Sessions register machine-wide, so Amp in one repo can ask a Claude session
sitting in another:

```bash
$ amp-bridge --list
payments-lib-4    claude_pid=78531  cwd=/Users/you/Projects/payments-lib
my-app-7          claude_pid=80112  cwd=/Users/you/Projects/my-app

$ amp-bridge --session payments-lib-4 --ask "Does Client.Charge stay
  idempotent if ctx is cancelled mid-request? Answer from the source."
```

The answer comes from the project that session has open, not from wherever the
question was asked.

### Claude hands work to Amp — with one rule

`ask_amp` runs a full turn in an Amp thread: Amp's model, Amp's tools, that
thread's workspace. The rule is that the thread must not be open in an
interactive `amp`, because Amp permits one executor per thread and an open
session holds it. Tell Claude the id once and it can send work across models:

```
you → Claude:  Before we merge, have Amp review the diff. Use T-9f21c3ab,
               I don't have it open.

Claude calls:  ask_amp(text="Run git diff main..fix/retry in
               /Users/you/Projects/my-app and review it for concurrency
               bugs. Reply with findings only.", thread_id="T-9f21c3ab")
```

`ask_amp` is synchronous — Claude blocks until Amp's turn ends, up to five
minutes — so this is a consultation, not a way to run both agents in parallel.

### Not only Amp

`amp-bridge --ask` is a plain binary that blocks and prints, so anything that can
run a process can query a live session. From a stray tmux pane:

```bash
amp-bridge --ask "One line: what are you working on, and are you blocked?"
```

### What it is not

It is not a task queue: both directions block the caller, and nothing is
persisted — an unanswered ask times out and is gone. It cannot make Claude
message the Amp thread you are actively typing in; that thread's executor slot is
taken, and only the Amp→Claude direction reaches it. And it is not a way around a
permission prompt: both agents run as you, and both sides are told to refuse work
the other was denied.

## Requirements

- **macOS or Linux.** The transport is a Unix socket; there is no Windows support.
- **[Claude Code](https://claude.com/claude-code)** — built against `2.1.235`.
  `amp-bridge doctor` tells you when a release has broken it.
- **The [Amp CLI](https://ampcode.com) on PATH** — needed only for the
  Claude→Amp direction (`ask_amp`). Everything inbound works without it.
- **Both agents on the same machine.** A remote Amp worker cannot reach a local
  socket.

## Install

**Prebuilt binary** (macOS and Linux, amd64 and arm64):

```bash
curl -fsSL https://raw.githubusercontent.com/oliver-kriska/amp-claude/main/install.sh | sh
```

The script verifies the download's SHA-256 against the published checksums before
installing to `~/.local/bin`. Read it first if you'd rather not pipe to a shell —
it is short. `AMP_BRIDGE_PREFIX=/usr/local` and `AMP_BRIDGE_VERSION=v0.4.0`
override the destination and the release.

**With Go** (1.26+):

```bash
go install github.com/oliver-kriska/amp-claude/amp-bridge@latest
```

**From source:**

```bash
git clone https://github.com/oliver-kriska/amp-claude
cd amp-claude
mise install     # optional: pins Go + golangci-lint from .tool-versions
make setup       # build, install, and register in this checkout
```

`make setup` registers this checkout only. Follow it with `amp-bridge init
--global` to cover your own projects.

### Register it

Registering is what tells Claude Code to spawn the bridge. Do it once, for every
project, along with the skill that teaches Claude how to use it:

```bash
amp-bridge init --global
```

Or for one project only, from that project's root:

```bash
amp-bridge init
```

`init` registers the server and nothing else; the skill is installed by
`--global`, user-wide at `~/.claude/skills/amp-bridge/`. The bridge works without
it — Claude just gets less guidance.

Then start Claude Code **with the channel flag**. This is the step everyone
misses, and without it nothing is delivered:

```bash
claude --dangerously-load-development-channels server:amp-bridge
```

The flag is required on every session; there is no config-file equivalent. Alias
it:

```bash
alias claude-amp='claude --dangerously-load-development-channels server:amp-bridge'
```

### Check it

```bash
amp-bridge doctor
```

```
  [ok  ] binary          /Users/you/.local/bin/amp-bridge (build 0c24be6bb9ab39f2)
  [ok  ] mcp config      user config, all projects: /Users/you/.local/bin/amp-bridge
  [ok  ] runtime dir     /tmp/amp-bridge-501
  [ok  ] live sessions   amp-claude-32 (claude_pid=78531)
  [ok  ] amp cli         /Users/you/.amp/bin/amp
  [ok  ] log             ~/.local/state/amp-bridge/amp-bridge.log (last write 9s ago)
```

Every failing line carries the command that fixes it. `FAIL` exits non-zero;
`warn` is a state you may be in on purpose — no session started yet is the
expected result of a pre-flight check — and exits 0, with `--strict` turning
warnings into failures for use as a gate.

`doctor` compares against reality rather than configuration: it executes the
configured binary, and compares the build fingerprint of every live session
against the installed one, so "installed but never restarted" is reported rather
than passing as green.

## Use it

**From Amp** (or any shell) — send a message and block until Claude answers:

```bash
amp-bridge --list                                # which sessions are reachable
amp-bridge --ask "what does this repo do?"       # send, wait, print the answer
amp-bridge --session payments-lib-4 --ask "..."  # target one of several
amp-bridge --thread T-abc123 --ask "..."         # let Claude reply into this thread
```

`--ask` blocks while Claude thinks. Typical round trips are 5–30 s: an idle
session answers immediately, a busy one delivers the event at its next turn
boundary, which is where the variance comes from. The silence is not breakage.

**From Claude** — two tools appear in the session:

- `reply(request_id=…, text=…)` answers an event Amp sent. The `request_id` is on
  the `<channel>` tag of the event being answered.
- `ask_amp(text=…, thread_id=…)` starts a turn in an Amp thread and returns what
  Amp says.

An inbound message reaches Claude as an event in its transcript, between turns:

```
<channel source="amp-bridge" request_id="amp-1787127436157248000-2" thread_id="T-abc123">
  Is hostLimiter the right place for a per-host cap?
</channel>
```

Claude's own transcript output never reaches Amp. Only `reply` and `ask_amp`
cross the bridge — anything Claude "says" without calling one of them is not
sent.

### Tell Amp it exists

The skill teaches the Claude side. The Amp side learns the way Amp learns
anything: from your project's `AGENTS.md`. A paste-able minimum:

```markdown
## amp-bridge

A live Claude Code session on this machine may be reachable over amp-bridge.
`amp-bridge --list` names the sessions; `amp-bridge --ask "<question>"` sends
one message and blocks until Claude answers. Add `--session <name>` when
several sessions are live, and `--thread <this-thread-id>` so Claude can
answer this thread later. Keep messages self-contained — Claude sees the text
and nothing else.
```

This repo's own [`AGENTS.md`](AGENTS.md) is the long version.

### Pairing a thread with a session

Pairing is symmetric — either side can establish it, and neither needs the
other's identifier up front.

**From Amp**, name your thread once. The id travels with the message, so Claude
sees it on the `<channel>` tag and can answer that thread specifically:

```bash
amp-bridge --thread T-abc123 --ask "take a look at the failing test"
```

**From Claude**, name the thread once and it is remembered for the rest of the
session:

```
ask_amp(text="…", thread_id="T-abc123")   # binds the pair
ask_amp(text="…")                         # goes to the same thread
```

`amp-bridge --list` finds the Claude session name, `amp threads list` finds the
Amp thread id. With exactly one of each, both are optional.

### One asymmetry worth knowing

Amp permits **one executor per thread**. An interactive `amp` session sitting in
a thread holds that slot, so `ask_amp` — which shells out to
`amp threads continue --execute` — cannot attach to it:

| Thread state | Amp → Claude | Claude → Amp |
|---|---|---|
| Open in an interactive session | `amp-bridge --ask` ✓ | `ask_amp` ✗ |
| Nobody has it open | `amp-bridge --ask` ✓ | `ask_amp` ✓ |

For the thread you are actively working in, the inbound direction is the whole
bridge: Amp asks, Claude answers with `reply`. The bridge reports this precisely,
naming the pid holding the thread, rather than relaying Amp's
`Unexpected error inside Amp CLI`.

## One binary, many projects

Three layers, three scopes. Keeping them apart is the whole mental model.

**The binary is per-machine.** One copy, on PATH — `~/.local/bin` by default.
Every registration points at it by absolute path, which is why `init` exists: it
resolves the path so you never type it.

**Registration is per-project or per-user.** It lives either in a project's
`.mcp.json` (written by `amp-bridge init`) or in the user-scope entry in
`~/.claude.json` (written by `amp-bridge init --global`, through
`claude mcp add`). Registration alone loads an ordinary MCP server; the launch
flag is what makes the session treat it as a channel. Both are needed, every
session.

**Live sessions are per-machine again.** Each Claude session started with the
flag runs its own bridge process with its own socket, and publishes itself in
`/tmp/amp-bridge-<uid>/` — one registry for your whole user, not per project. The
published name is the Claude session's own name, which `--list` prints beside
each session's working directory.

What follows from that:

- `--list` sees every live session on the machine, from any directory. The
  caller's cwd never matters; only the session you address does.
- With one live session, `--ask` needs no flags. With several it refuses and
  names them — pick one with `--session <name>`.
- The answer comes from the session's project. With two open,
  `--ask "what does this repo do?"` is about whichever session answered; say
  which with `--session`.
- `doctor` is the one project-relative command. It reports the sessions serving
  the directory it runs in — or one you name, `amp-bridge doctor ~/Projects/app` —
  and counts sessions in other projects separately rather than as green.

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

## Security

The runtime directory is per-uid and 0700; sockets and registry entries are 0600.
Both the server and the client refuse that directory unless it is a real
directory, owned by the current user, with no group or other access — `/tmp` is
world-writable, and a directory planted there would otherwise receive your
prompts. The bridge also refuses to bind over a socket another live bridge owns,
and `init` refuses to write through a symlinked `.mcp.json`.

Both agents run as you, with your permissions. Neither side should route work
through the bridge that its own permission prompt denied.

Text arriving over the channel is external data, not instructions. Claude Code
marks it as untrusted; the bundled skill tells Claude to treat it that way.

## Uninstall

```bash
claude mcp remove amp-bridge          # the user-scope registration
rm -rf ~/.claude/skills/amp-bridge    # the skill
rm -f  ~/.local/bin/amp-bridge        # the binary
rm -rf ~/.local/state/amp-bridge      # the log
rm -rf "/tmp/amp-bridge-$(id -u)"     # sockets and the registry
```

For per-project registrations, delete the `amp-bridge` entry from each
`.mcp.json` — it is the only thing `init` added.

## Development

```bash
make check                # the gate: tidy, skill drift, format, vet, lint, both test tiers under -race
make test                 # fast unit pass
make test-integration     # spawns a real bridge process and drives both ends
make doctor               # diagnose the installed bridge
make help                 # every target
```

Neither test tier needs the network or a live Claude session.

A rebuild does not change what a running session is executing — that is what
`doctor`'s fingerprint check reports. [`AGENTS.md`](AGENTS.md) has the details.

### Three things that look like bugs

They are load-bearing. The reasoning, and how each was found, is in
[the research log](.claude/research/2026-08-19-amp-claude-code-bridge.md).

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

- [`AGENTS.md`](AGENTS.md) — how Amp should drive the bridge
- [`.claude/skills/amp-bridge/SKILL.md`](.claude/skills/amp-bridge/SKILL.md) — how Claude should
- [the research log](.claude/research/2026-08-19-amp-claude-code-bridge.md) — the protocol archaeology, in full

## Licence

MIT © Oliver Kriska
