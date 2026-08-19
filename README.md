# amp-bridge

Let an [Amp](https://ampcode.com) thread and a live [Claude Code](https://claude.com/claude-code)
session talk to each other, in both directions, on your machine.

```
Amp ──unix socket──▶ amp-bridge ──notifications/claude/channel──▶ Claude session
Amp ◀──unix socket── amp-bridge ◀────── reply tool ────────────── Claude session
```

Amp asks a question and blocks until Claude answers. Claude can start the
conversation too, by calling `ask_amp`, which runs a turn in the Amp thread.

No server, no API key, no network: one ~4 MB Go binary, a Unix socket, and a
registry file under `/tmp`. The module has **zero dependencies** and is built
that way deliberately — see [Three things that look like bugs](#three-things-that-look-like-bugs).

It works by registering as a Claude Code **channel**: an MCP server permitted to
push unsolicited events into a running session. Channels are an experimental,
undocumented extension point, which is why the launch flag below says
`--dangerously-load-development-channels`.

> **Status:** experimental, and built against undocumented internals of Claude
> Code `2.1.235`. It can break on any Claude Code release. `amp-bridge doctor`
> exists to tell you when it has.

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
make setup       # build, install, and register in this repo's .mcp.json
```

### Register it

Once installed, register the bridge for **every** project, and install the skill
that teaches Claude how to use it:

```bash
amp-bridge init --global
```

Or for one project only, from that project's root:

```bash
amp-bridge init
```

Then start Claude Code **with the channel flag** — this is the step everyone
misses, and without it nothing is delivered:

```bash
claude --dangerously-load-development-channels server:amp-bridge
```

The flag is required on every session; there is no config-file equivalent. Most
people alias it:

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

Every failing line carries the command that fixes it. Run `doctor` first whenever
the bridge seems dead — the failure modes here are quiet ones, and it is built to
name them out loud.

Three states: `FAIL` is broken and exits non-zero; `warn` is a state you may be
in on purpose (no session started yet is the expected result of a pre-flight
check) and exits 0. `amp-bridge doctor --strict` treats warnings as failures too,
for use as a gate.

`doctor` compares against reality rather than configuration: it executes the
configured binary, and compares the build fingerprint of every live session
against the installed one — so "installed but never restarted" is reported rather
than passing as green.

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

### Pairing a thread with a session

Pairing is symmetric — either side can establish it, and neither needs to know
the other's identifier up front.

**From Amp**, name your thread once. The id travels with the message, so Claude
sees it on the `<channel>` tag and can answer that thread specifically:

```bash
amp-bridge --thread T-abc123 --ask "take a look at the failing test"
```

```
<channel source="amp-bridge" request_id="amp-…-2" thread_id="T-abc123">
```

**From Claude**, name the thread once and it is remembered for the rest of the
session:

```
ask_amp(text="…", thread_id="T-abc123")   # binds the pair
ask_amp(text="…")                         # goes to the same thread
```

Use `amp-bridge --list` to find the Claude session name, and `amp threads list`
to find the Amp thread id. With exactly one of each, both are optional.

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

## Development

```bash
make check                # tidy, skill drift, format, vet, 29 linters, both test tiers under -race
make test                 # fast unit pass
make test-integration     # spawns a real bridge process and drives both ends
make doctor               # diagnose the installed bridge
make help                 # every target
```

Neither test tier needs the network or a live Claude session. 121 tests.

`make build` alone changes nothing a running session sees: the MCP config points
at the *installed* binary, and a live bridge keeps its original inode regardless.
New code takes effect on `make install` **plus** a session restart — which is
exactly what `doctor`'s fingerprint check reports.

### Three things that look like bugs

They are load-bearing. The reasoning, and how each was found, is in
[`.claude/research/2026-08-19-amp-claude-code-bridge.md`](.claude/research/2026-08-19-amp-claude-code-bridge.md).

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
