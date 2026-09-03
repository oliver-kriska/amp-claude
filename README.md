# amp-bridge

<p align="center">
  <img src="docs/assets/amp-bridge-mascot.png" width="320" alt="amp-bridge mascot connecting two local agent endpoints">
</p>

<p align="center"><strong>Let an Amp thread and a live Claude Code session talk directly on your machine.</strong></p>

```text
Amp thread  ◀──── Unix socket ────▶  amp-bridge  ◀──── Claude channel ────▶  Claude Code
```

No copy-paste relay. No additional server, API key, or network hop. The bridge is
a small, dependency-free Go binary using a per-user Unix socket and local
registry under `/tmp`.

It registers as a Claude Code [channel](https://code.claude.com/docs/en/channels-reference):
an MCP server allowed to push an event into a running session and receive a
correlated reply.

> **Status:** experimental and last live-tested against Claude Code `2.1.246`
> on 2026-08-26.
> Custom channels still require a development launch flag, and their contract
> may change between Claude Code releases. `amp-bridge doctor` detects the
> breakages that otherwise fail silently.

## Quick start

The complete machine-wide setup is four commands:

```bash
curl -fsSL https://raw.githubusercontent.com/oliver-kriska/amp-claude/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
amp-bridge init --global               # Claude channel + skill
amp-bridge init --amp-plugin --global  # Amp inbox for the return direction
```

The export makes the installed binary available immediately. If the installer
reports that `~/.local/bin` was not already on `PATH`, add the same export to
your shell profile so future shells find it too.

Then start the Claude Code session you want Amp to reach:

```bash
cd ~/Projects/my-app
claude --name my-app-review \
  --dangerously-load-development-channels server:amp-bridge
```

The flag is required for every Claude session that should receive bridge events;
it does not loosen either agent's tool permissions. [Alias it](#register-it) if
you use the bridge regularly.

Test Amp → Claude from any shell:

```bash
amp-bridge --ask "Reply with exactly PONG"
```

`PONG` means the full request/reply path works. There is no `--thread` in this
smoke test because it starts in a shell rather than an Amp thread. If it fails,
run `amp-bridge doctor`; every failing check prints its own fix.

That is all you need for **Amp → Claude**. To let Claude ask the same open Amp
thread a question, enable that thread's inbox once:

```text
Ctrl+O → amp-bridge: Enable Claude inbox for this thread
```

If Amp was already open when you installed the plugin, first run `Ctrl+O` →
`plugins: reload`. A new Amp session loads it automatically. A fresh Amp process
has no thread until you send its first message.

If **Enable Claude inbox** is unavailable:

- the matching **Disable Claude inbox** command means it is already enabled;
- no active thread means you need to send the session's first message;
- a remote Amp executor cannot host this local inbox; and
- a missing amp-bridge command means the plugin is not loaded—reload plugins.

`amp-bridge doctor` confirms the enabled thread under `plugin inboxes`.

If Amp created or manages a target thread with no palette of its own, use
`amp-bridge: Enable Claude inbox for another thread` from a local Amp thread and
paste the target URL. That local thread becomes its explicit inbox controller;
this does not auto-enable other threads.

The smoke test covers one direction. The first practical exchange, both ways,
is walked through
[below](#your-first-two-way-conversation).

## Your first two-way conversation

The channel, the skill and the inbox plugin are separate on purpose — the two
`init` commands in the quick start install them, and that is the whole setup
cost.
Once the pair is established, normal use is two plain-language prompts:

```text
you → Amp:     Use amp-bridge to ask Claude session my-app-review to review auth.go.
you → Claude:  Ask Amp whether the running app reproduced the race.
```

You do not handle sockets, request ids or bridge protocol in daily use. Omit the
session name when only one Claude session is live. When Amp includes its thread
id in the first request, Claude can use that same explicit id for later questions.

| Frequency | Action |
|---|---|
| Once per machine | Install the binary and run the two `init` commands above |
| Each Claude session | Start it with the channel flag—or use the `claude-amp` alias below |
| Each Amp thread, once | Enable the inbox only if Claude must append into a held thread; use the named-thread command when Amp manages it in the background |
| Each question | Ask in ordinary language; wait for a consultation or say “in the background” for independent work |

| Part | What it does | What it does not do |
|---|---|---|
| Claude channel | Carries Amp's request into a running Claude session and correlates its reply | Does not teach Claude when to use the bridge |
| Claude skill | Teaches Claude to answer with `reply`, consult with `ask_amp`, or delegate with `send_amp` | Is not a transport and does not enable an Amp inbox |
| Amp inbox plugin | Appends Claude's request through the executor that already owns an open thread and captures its eventual answer | Cannot wake an idle Amp thread; Amp must start the queued turn |

You do not invoke the skill yourself. Claude loads it when an amp-bridge event
arrives or when you ask Claude to "ask Amp".

### 1. Start the Claude session

Run this from the project whose context you want Claude to use:

```bash
cd ~/Projects/my-app
claude --name my-app-review \
  --dangerously-load-development-channels server:amp-bridge
```

`--name` is optional, but useful when several Claude sessions are live. If
Claude was already running when you installed or rebuilt the bridge, restart or
resume it with the channel flag; a running session cannot acquire the channel
later.

### 2. Prepare the Amp thread

Open the target thread in a local Amp session and send its first message. A
fresh `amp` process has no thread until then.

Then enable the inbox for that thread:

```text
Ctrl+O → amp-bridge: Enable Claude inbox for this thread
```

For a managed/background thread that has no command palette, stay in any local
Amp thread and run:

```text
Ctrl+O → amp-bridge: Enable Claude inbox for another thread
```

Paste the target thread URL or `T-…` id and confirm. The local thread becomes
the controller for that consent. Use the matching `Disable Claude inbox for
another thread` command there to revoke it. Binding consent to a named
controller lets the pairing survive plugin reloads and return after process
restarts without allowing every local Amp process to claim every enabled thread.
If the original controller is later deleted, the same Disable command from any
local Amp thread can explicitly revoke the pairing after confirmation.

If the command is missing from the palette, Amp was already running when you
installed the plugin: run `Ctrl+O` → `plugins: reload` first. If **Enable** is
present but unavailable, search for **Disable** first—its presence means the
inbox is already enabled. Otherwise send the first message in a new session;
remote-executor threads cannot host the local inbox.

The inbox is off by default and enabled per thread. That one explicit enable is
remembered until you disable it. After an Amp restart, ordinary consent returns
when that exact local target thread opens; managed consent returns when its
controller thread opens. It is needed only for **Claude → an Amp thread that is
currently open**; Amp → Claude and Claude → a thread nobody has open work
without it.

Copy the Amp thread id from the thread URL — its final `T-…` segment — or find
it with `amp threads list`.

### 3. Amp asks Claude

In the Amp thread, ask Amp to run the following (or run it from another local
shell):

```bash
amp-bridge --list
amp-bridge --session my-app-review \
  --thread T-01234567-89ab-cdef-0123-456789abcdef \
  --ask "Inspect auth.go and tell me whether the refresh path can race logout."
```

`--session` chooses the Claude project that should answer. `--thread` tells
Claude which Amp thread originated the request, so it can follow up later.
**Always include it when calling from Amp**, even when only one thread is using
the Claude session. Without it Claude can answer this request, but it cannot
reliably initiate a later request back to a managed thread.
Claude receives the question as a channel event; the installed skill tells it
to call `reply`, and the answer is printed back into Amp's turn.

With exactly one live Claude session, omit `--session`. To make Amp discover the
bridge without being told each time, add the short `AGENTS.md` snippet under
[Tell Amp it exists](#tell-amp-it-exists).

### 4. Claude asks the same Amp thread

Now tell Claude:

```text
Ask Amp thread T-01234567-89ab-cdef-0123-456789abcdef whether the running app has
reproduced that logout race.
```

The skill makes Claude call:

```text
ask_amp(
  text="Has the running app reproduced the logout race? Check runtime evidence.",
  thread_id="T-01234567-89ab-cdef-0123-456789abcdef"
)
```

Because the inbox is enabled, this appends the question through the executor
already owned by the open Amp session. If Amp is currently running, the question
steers its next turn and the answer returns to Claude. If Amp is idle, the plugin
cannot wake it: Claude receives `no-turn`, and the request remains visible in
Amp. Switch to that thread and send a new instruction such as “Answer Claude's
queued bridge request.” Do **not** resend the original question.

Claude should retain and reuse the explicit `thread_id` from Amp's channel event;
a mutable “last thread” fallback is unsafe once several threads share one Claude
session.

When Claude starts the exchange with `ask_amp`, Amp should answer normally. It
must not call `amp-bridge --ask` back during that turn: Claude is blocked inside
the tool call and cannot answer a second inbound event until Amp finishes.

For independent work, make that intent explicit:

```text
Send Amp thread T-01234567-89ab-cdef-0123-456789abcdef a background review of the
retry code. Keep working here; tell me when Amp's result arrives.
```

The skill makes Claude call
`send_amp(text=…, thread_id="T-01234567-89ab-cdef-0123-456789abcdef")`. It
returns an `amp-async-…` handle immediately, Claude ends the current turn, and
Amp works without holding Claude inside a tool call. Success or failure later
wakes Claude as a channel completion event. `send_amp` always requires an
explicit thread id; background work must never rely on a mutable “last thread”
default.

For a thread nobody has open, the CLI fallback starts Amp immediately. For an
open thread, the inbox can append the request but Amp's current plugin API has no
operation that wakes an idle executor. If no Amp turn starts, the completion
reports `status="error"`; the request is already visible in the Amp thread and
must not be resent. Later activity in that thread may process the queued request.

### Scope, reloads and restarts

| Action | Scope | Existing processes |
|---|---|---|
| `amp-bridge init --global` | Claude channel registration for every project + user-wide Claude skill | Restart Claude with the channel flag; a running session cannot acquire the channel or a rebuilt binary |
| `amp-bridge init` | Claude channel registration in the current project's `.mcp.json`; no skill install | Start or restart Claude in that project with the channel flag |
| `amp-bridge init --amp-plugin --global` | Plugin file loaded by every Amp process; still disabled per thread | Reload Amp processes that were already open; new processes load it automatically |
| `amp-bridge init --amp-plugin /path/to/project` | Plugin file in one repository | Reload already-open Amp processes in that project |
| `Enable Claude inbox for this thread` | One local thread | No reload; consent survives reloads and returns when that thread reopens |
| `Enable Claude inbox for another thread` | One managed target, controlled by the current local thread | No reload; consent returns when the controller reopens, but an idle target still needs human activity |

Amp's `load_plugin` tool can update only
`~/.config/amp/plugins/amp-bridge-inbox.ts` without restarting unrelated
plugins. `Ctrl+O` → `plugins: reload` rediscovers all plugin files and restarts
every plugin.

An enabled thread answers one question at a time and holds at most four more
waiting; past that, `ask_amp` or `send_amp` is told the inbox is busy rather than
queued indefinitely. Enabled inboxes survive plugin reloads and local Amp process
restarts: same-host reloads restore their registrations immediately. A normal
consent returns when its target thread reopens; a managed consent returns only
when its controller reopens. This prevents target and controller hosts from
racing to serve one inbox. Never-enabled threads stay closed. Do not reload while
an Amp turn is in flight: the old plugin aborts that request before the new copy
re-arms the inbox, and delivery may already have reached Amp. Check the thread
before retrying so you do not append it twice.

Named enablement always fixes **addressability**: Claude can target a managed
thread that has no palette of its own. It completes delivery immediately when
that target is already running, because the request can steer its next turn. Amp
currently provides no plugin API for waking an idle thread, so an idle target
keeps the request queued. The controller receives a notification telling the
user where to continue, while Claude receives a `no-turn` result that explicitly
says not to resend.

Choose project or global scope rather than both. If both copies are present,
the load guard keeps one inert, but Amp will still show two installed files.

## How it works

Amp → Claude is a synchronous request/reply. Claude → Amp offers both shapes:
`ask_amp` waits for Amp's answer, while `send_amp` returns a handle and delivers
the result later as a channel completion event. Both are local and in-memory;
neither is a durable message queue.

<p align="center">
  <img src="docs/assets/amp-bridge-flow.png" width="900" alt="Local amp-bridge flows. Amp asks Claude synchronously through a Unix socket and Claude returns an answer with reply. Claude reaches Amp through a live plugin inbox or CLI fallback, either synchronously with ask_amp or in the background with send_amp and a later completion event.">
</p>

The plugin path in the diagram is delivery, not a wake primitive. A request to
an idle open thread waits until Amp starts another turn; the bridge reports that
state rather than pretending background work began.

The inbox check happens before the CLI fallback. An open thread without an
enabled inbox therefore reaches the fallback and is refused because Amp will
not attach a second executor. The error identifies the owning process and tells
you how to enable the inbox.

## Why bridge them?

Two coding agents on one machine usually leave you copy-pasting between two
terminals. The bridge removes you from the middle. Which threads each direction
can reach, and what it takes to reach a thread you have open yourself, is set out
in [Reaching a thread you have open](#reaching-a-thread-you-have-open).

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

Amp gets the answer inside its turn and keeps working. Once the Claude session is
live, this direction works whether that session is idle or busy. The first real
use of this bridge was exactly that: Amp code-reviewed this repository through
it, and the findings are in the git history.

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

### Claude hands work to Amp

`ask_amp` and `send_amp` request a full turn in an Amp thread: Amp's model,
Amp's tools, that thread's workspace. Choose whether Claude needs the result now:

```
you → Claude:  Before we merge, have Amp review the diff. Use
               T-01234567-89ab-cdef-0123-456789abcdef.

Claude calls:  ask_amp(text="Run git diff main..fix/retry in
               /Users/you/Projects/my-app and review it for concurrency
               bugs. Reply with findings only.",
               thread_id="T-01234567-89ab-cdef-0123-456789abcdef")

or, for independent work:

Claude calls:  send_amp(text="Review the retry diff for concurrency bugs.",
               thread_id="T-01234567-89ab-cdef-0123-456789abcdef")
```

`ask_amp` is synchronous — Claude blocks until Amp's turn ends, up to two
minutes. `send_amp` returns immediately; Claude must end that turn so Amp can run,
then a completion event brings Amp's answer back. Use it when the two agents have
independent work, not when Claude needs Amp's answer for its next step.

That start is automatic only through the CLI fallback, when no interactive Amp
executor has the thread open. An enabled inbox solves delivery into an open
thread, but not wake-up: Amp must already be active or the user must start the
next activity. Until Amp exposes a supported wake method, do not treat an open
idle thread as an unattended background worker.

CLI fallback has no queue: overlapping turns for the same idle thread fail fast
instead of racing two executors. Enable that thread's inbox when same-thread
turns need to queue; the plugin owns the per-thread FIFO.

A thread nobody has open is always reachable. A thread you are sitting in needs
its inbox enabled first — see
[Reaching a thread you have open](#reaching-a-thread-you-have-open).

### Not only Amp

`amp-bridge --ask` is a plain binary that blocks and prints, so anything that can
run a process can query a live session. From a stray tmux pane:

```bash
amp-bridge --ask "One line: what are you working on, and are you blocked?"
```

### What it is not

It is not a durable task queue: nothing is persisted, retried or resumed.
`send_amp` permits concurrent independent work, but an MCP restart loses any
outstanding completion event. There is intentionally no poll or cancel API. And
it is not a way around a permission prompt: both agents run as you, and both
sides are told to refuse work the other was denied.

## Requirements

- **macOS or Linux.** The transport is a Unix socket; there is no Windows support.
- **[Claude Code](https://claude.com/claude-code)** — last verified against
  `2.1.246` on 2026-08-26. `amp-bridge doctor` catches protocol drift that would
  otherwise fail silently.
- **The [Amp CLI](https://ampcode.com) on PATH** — needed only for the
  Claude→Amp direction (`ask_amp` and `send_amp`). Everything inbound works
  without it.
- **Both agents on the same machine.** A remote Amp worker cannot reach a local
  socket.

## Installation options

The quick start uses the **prebuilt binary** for macOS or Linux, amd64 or arm64:

```bash
curl -fsSL https://raw.githubusercontent.com/oliver-kriska/amp-claude/main/install.sh | sh
```

The script verifies the download's SHA-256 against the published checksums before
installing to `~/.local/bin`. Read it first if you'd rather not pipe to a shell —
it is short. `AMP_BRIDGE_PREFIX=/usr/local` overrides the destination;
`AMP_BRIDGE_VERSION=v0.2.0` is an example of pinning a release.

Release archives carry signed build provenance, so you can check where a download
was actually built rather than taking the checksum's word for it:

```bash
gh attestation verify amp-bridge_*.tar.gz -R oliver-kriska/amp-claude
```

**With Go** (1.27+):

```bash
go install github.com/oliver-kriska/amp-claude/amp-bridge@latest
```

**From source:**

```bash
git clone https://github.com/oliver-kriska/amp-claude
cd amp-claude
mise install       # optional: pins Go + golangci-lint from .tool-versions
make setup-global  # build, install, and set up both directions machine-wide
```

For one project instead, `make setup PROJECT="$HOME/Projects/my-app"` registers
only that project's Claude channel; install its Amp plugin separately with
`amp-bridge init --amp-plugin "$HOME/Projects/my-app"`.

### Register it

Registering is what tells Claude Code to spawn the bridge. For one user-wide
setup across every project, run this once; it also installs the skill that
teaches Claude how to use the bridge:

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

For two-way use, install the Amp inbox plugin, which is what lets Claude's
outbound tools reach a thread while you have it open — see
[Reaching a thread you have open](#reaching-a-thread-you-have-open):

```bash
amp-bridge init --amp-plugin --global
```

Then start Claude Code **with the channel flag**. This is the step everyone
misses, and without it nothing is delivered:

```bash
claude --dangerously-load-development-channels server:amp-bridge
```

The flag is required on every session; there is no config-file equivalent. Alias
it in your shell profile (`~/.zshrc`, `~/.bashrc`, or equivalent):

```bash
alias claude-amp='claude --dangerously-load-development-channels server:amp-bridge'
```

### Check it

```bash
amp-bridge doctor
```

```
  [ok  ] binary                 /Users/you/.local/bin/amp-bridge (build 0c24be6bb9ab39f2)
  [ok  ] mcp config             user config, all projects: /Users/you/.local/bin/amp-bridge
  [ok  ] runtime dir            /tmp/amp-bridge-501
  [ok  ] live sessions          amp-claude-32 (claude_pid=78531)
  [ok  ] plugin inboxes         T-9f21c3ab-6d0e-4c11-b2a7-5f3e0c9d81aa (plugin pid 78002)
  [ok  ] amp plugin             installed for this project and all Amp sessions
  [ok  ] amp cli                /Users/you/.amp/bin/amp
  [ok  ] timeout ordering       ask_amp worst-case 2m10s leaves 50s before the 3m0s Claude reply deadline
  [ok  ] log                    ~/.local/state/amp-bridge/amp-bridge.log (last write 9s ago)
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
amp-bridge --list --verbose                      # include build, handshake and socket details
amp-bridge --ask "what does this repo do?"       # send, wait, print the answer
amp-bridge --session payments-lib-4 --ask "..."  # target one of several
amp-bridge --thread T-01234567-89ab-cdef-0123-456789abcdef --ask "..."
amp-bridge --timeout 10m --ask "..."              # longer bounded consultation
```

`--ask` blocks while Claude thinks. Typical round trips are 5–30 s: an idle
session answers immediately, a busy one delivers the event at its next turn
boundary, which is where the variance comes from. The silence is not breakage.
The default server deadline is 3 minutes. `--timeout` requests a longer deadline
from the live server, capped at 15 minutes by default; `--list --verbose` shows
both effective limits. If it still times out, the CLI prints a request id and
the exact `--result` command that retrieves a later answer.
Against an older live server that advertises no timeout limits, `--timeout`
refuses before sending and asks you to restart that Claude session.

**From Claude** — three tools appear in the session:

- `reply(request_id=…, text=…)` answers an event Amp sent. The `request_id` is on
  the `<channel>` tag of the event being answered.
- `ask_amp(text=…, thread_id=…)` starts a turn in an Amp thread and returns what
  Amp says.
- `send_amp(text=…, thread_id=…)` starts independent work, returns a handle, and
  later receives Amp's result as a completion event. `thread_id` is mandatory.

An inbound message reaches Claude as an event in its transcript, between turns:

```
<channel source="amp-bridge" request_id="amp-1787127436157248000-2" thread_id="T-01234567-89ab-cdef-0123-456789abcdef">
  Is hostLimiter the right place for a per-host cap?
</channel>
```

Claude's own transcript output never reaches Amp. Only `reply`, `ask_amp` and
`send_amp` cross the bridge — anything Claude "says" without calling one of
them is not sent.

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

The Claude session name chooses **which Claude project** should answer. The Amp
thread id chooses **which Amp conversation** Claude may call back into.

**From Amp**, include the complete thread id. It travels with the message, so
Claude sees it on the `<channel>` tag and can answer that thread specifically:

```bash
amp-bridge --thread T-01234567-89ab-cdef-0123-456789abcdef \
  --ask "take a look at the failing test"
```

**From Claude**, pass the same explicit id:

```
ask_amp(text="…", thread_id="T-01234567-89ab-cdef-0123-456789abcdef")
```

`ask_amp` remembers an explicitly supplied id as a convenience for a later
serial call, but explicit routing is safer when several threads share one Claude
session. `send_amp` always requires `thread_id` and never changes the remembered
route.

`amp-bridge --list` finds the Claude session name, `amp threads list` finds the
Amp thread id. `--session` is optional when exactly one Claude session is live;
the thread id is required on the first request if Claude needs to call Amp back.

### Reaching a thread you have open

Amp permits **one executor per thread**. An interactive `amp` session sitting in
a thread holds that slot, so the CLI fallback — `amp threads continue --execute`
— is refused with `EXECUTOR_ALREADY_CONNECTED`. That is a limit on attaching a
second executor, not on the thread: it can take another message perfectly well,
just not down that path.

The **inbox plugin** takes the other path. It runs inside your Amp session and
appends the message through the executor that session already holds, then captures
the answer if Amp runs that queued turn. Both outbound tools use it whenever the
thread has a live inbox and fall back to the CLI otherwise:

| Thread state | Amp → Claude | Claude → Amp |
|---|---|---|
| Nobody has it open | `amp-bridge --ask` ✓ | `ask_amp` / `send_amp` ✓ — via the CLI |
| Open, inbox enabled and Amp starts the queued turn | `amp-bridge --ask` ✓ | `ask_amp` / `send_amp` ✓ — via the plugin |
| Open, inbox enabled but idle | `amp-bridge --ask` ✓ | Request is queued, but no automatic turn; caller gets `no-turn`, and later activity may pick it up |
| Open, no inbox | `amp-bridge --ask` ✓ | `ask_amp` / `send_amp` ✗ |

Install the plugin once:

```bash
amp-bridge init --amp-plugin --global
```

In an Amp process that was already open during plugin installation, reload
plugins (`Ctrl+O` → `plugins: reload`). New processes load it automatically.
Send one message if the session is new — a fresh `amp` has no thread until you
write to it — then `Ctrl+O` →
`amp-bridge: Enable Claude inbox for this thread`.

Enabling is **per thread and off by default**. Enable an interactive thread from
its own local session; for a managed/background target, use **Enable Claude inbox
for another thread** from a local controller. A remote-executor thread cannot
host this local inbox. `amp-bridge doctor` lists live inboxes. When **Enable** is
unavailable, search for the matching **Disable** command—if it exists, the inbox
is already enabled.

Without an inbox, the inbound direction is the whole bridge for that thread: Amp
asks, Claude answers with `reply`. The bridge reports the refusal precisely,
naming the pid holding the thread and pointing at `init --amp-plugin`, rather
than relaying Amp's `Unexpected error inside Amp CLI`.

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
| `AMP_BRIDGE_MAX_INFLIGHT` | 8 | unanswered Amp→Claude events and concurrent `send_amp` turns allowed (separate caps) |
| `AMP_BRIDGE_MAX_BYTES` | 65536 | max bytes per message |
| `AMP_BRIDGE_TIMEOUT` | 3m | how long `--ask` waits for Claude |
| `AMP_BRIDGE_MAX_TIMEOUT` | 15m | maximum accepted per-request `--timeout` |
| `AMP_BRIDGE_RESULT_TTL` | 1h | how long a late reply remains retrievable in memory |
| `AMP_BRIDGE_MAX_RESULTS` | 64 | maximum timed-out requests retained in memory |
| `AMP_BRIDGE_AMP_TIMEOUT` | 2m | how long an `ask_amp` or `send_amp` Amp turn may run |
| `AMP_BRIDGE_LOG` | `~/.local/state/amp-bridge/amp-bridge.log` | log file |
| `AMP_BRIDGE_LOG_BODIES` | unset | `1` also logs frame bodies (conversation text) |
| `AMP_BRIDGE_DIR` | `/tmp/amp-bridge-<uid>` | socket + registry directory |
| `AMP_BRIDGE_SOCKET` | — | explicit socket path (single-session mode) |
| `AMP_BRIDGE_DISABLE_OUTBOUND` | unset | `1` disables both Claude→Amp tools |
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

The inbox plugin appends messages into a live Amp thread, so it is deliberately
narrow. File modes keep other *users* out; they do not keep out other processes
running as you, and any such process could append into an enabled thread. The
controls that matter are therefore the ones above the filesystem: it is off by
default on every thread, it can only be turned on by a keystroke inside the
session that owns the executor, every field is validated before anything reaches
Amp's context, and each appended message carries a visible label naming the
sender.

Text arriving over the channel is external data, not instructions. Claude Code
marks it as untrusted; the bundled skill tells Claude to treat it that way.

## Uninstall

```bash
claude mcp remove amp-bridge          # the user-scope registration
rm -rf ~/.claude/skills/amp-bridge    # the skill
rm -f  ~/.config/amp/plugins/amp-bridge-inbox.ts   # the Amp plugin
rm -f  ~/.local/bin/amp-bridge        # the binary
rm -rf ~/.local/state/amp-bridge      # the log
rm -rf "/tmp/amp-bridge-$(id -u)"     # sockets and the registry
```

For per-project registrations, delete the `amp-bridge` entry from each
`.mcp.json` — it is the only thing `init` added. Project-scoped plugin copies
live at `.amp/plugins/amp-bridge-inbox.ts`.

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

## Questions people actually ask

**Do both agents have to be running?** Claude must have a live session for
Amp → Claude. Claude → Amp needs the Amp CLI, but not an already-open interactive
Amp process: for a thread nobody holds, the CLI fallback starts its turn. Nothing
is a durable queue—an unanswered request times out, and `send_amp` exists only in
the bridge process's memory until its completion event is delivered.

**Why does Claude need `--dangerously-load-development-channels`?** Channels are
the only MCP mechanism that can push an unsolicited event into a running Claude
session, and Claude Code gates them behind that flag. It loads this one server. It
does not loosen either agent's tool permissions, and there is no config-file
equivalent — you need it on every session.

**The startup warning tells me to use `--channels` instead. Can I?** No, not for
this bridge. `--channels` takes only entries that clear Claude Code's approved
allowlist, and a `server:` entry never does: the CLI's own rejection reason is
"server: entries need --dangerously-load-development-channels". Passing
`--channels server:amp-bridge` is not an error — the entry is silently unmatched
and no channel registers, which looks exactly like the bridge being broken.
Packaging the bridge as a plugin does not change this: while channels are in
research preview the approved allowlist is the Anthropic-curated set in
`claude-plugins-official`, and [the channels
reference](https://code.claude.com/docs/en/channels-reference#package-as-a-plugin)
states that a channel published to your own marketplace still needs the
development flag. The one alternative is a Team/Enterprise admin listing it in
`allowedChannelPlugins`, a managed-org setting that replaces the default
allowlist. Note also that the bypass is per entry — combining both flags does not
extend it to the `--channels` entries. So the warning is expected here, not a sign
of misconfiguration.


**A send_amp timeout does not mean the message was lost.** `ask_amp` is bounded
at 2 minutes because it holds a Claude turn open; `send_amp` waits 10 minutes
because it blocks nobody. When a send does time out, the completion event says
which kind of timeout it was: `pending` means the message reached the thread and
Amp is still working — resending duplicates it — while `not-delivered` means
nothing was appended and a retry is safe. `unknown` means the bridge could not
tell, so check the thread. Set `AMP_BRIDGE_SEND_TIMEOUT` to change the send
budget; `amp-bridge doctor` reports it under `send budget`.
**Can the two agents work in parallel?** Yes when the target Amp thread is free
for CLI fallback: tell Claude to use `send_amp`, then end its current turn. Amp
works in the background and a completion event wakes Claude with the result. An
open interactive Amp thread is different: its inbox can accept the request, but
cannot wake an idle executor, so the user must start the queued turn. Use
`ask_amp` for a consultation whose answer Claude needs before it can continue.
Background work is bounded, in-memory and not resumed after an MCP restart.

**Why is the inbox off by default, when turning it on is the whole point?**
Because "on" is a decision about a live thread you are sitting in. File modes keep
other users out but not other processes running as you, so the meaningful control
is explicit palette confirmation in a local Amp session—either the target itself
or the named controller for a managed thread. See [Security](#security).

**Can Amp reach a Claude session in another project?** Yes. `amp-bridge --list`
names every live session and `--session N` picks one. The answer comes from the
project that session has open, which is usually why you are asking it.

**What happens if an answer takes longer than the timeout?** Use `--timeout 10m`
when asking for work that genuinely needs longer; the request carries that
deadline to the already-running server. On timeout, the CLI prints a request id
and an exact `--result` command. A later reply is retained for up to an hour by
default, bounded to 64 in-memory results. The mailbox intentionally does not
persist conversation bodies: a bridge restart or expiry makes the result
unrecoverable. Check the Claude transcript before resending.

**Does it work over SSH, or with Amp's remote executor?** No. The transport is a
Unix socket, so both agents must be on the same machine, and the inbox only offers
itself when the Amp session holds that thread's executor locally.

**Is any of this sent anywhere?** Only where you send it. The bridge is a local
socket between two processes owned by you. It logs frame shapes, not message
content, unless you set `AMP_BRIDGE_LOG_BODIES=1`.

**Something is broken and the errors look fine.** Run `amp-bridge doctor`. It
executes the configured binary rather than trusting the config, and compares every
live session's build fingerprint against the installed one — which catches the
failure with no symptom, where everything reports healthy and nothing is
delivered.

## Further reading

- [`AGENTS.md`](AGENTS.md) — how Amp should drive the bridge
- [`.claude/skills/amp-bridge/SKILL.md`](.claude/skills/amp-bridge/SKILL.md) — how Claude should
- [the research log](.claude/research/2026-08-19-amp-claude-code-bridge.md) — the protocol archaeology, in full

## Licence

MIT © Oliver Kriska
