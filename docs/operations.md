# Operations

Running amp-bridge day to day: how the scopes fit together, every tunable, the
security model, uninstalling, and the questions that come up most.

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
| `AMP_BRIDGE_AMP_TIMEOUT` | 2m | how long a synchronous `ask_amp` turn may run |
| `AMP_BRIDGE_SEND_TIMEOUT` | 10m | how long an asynchronous `send_amp` turn may run |
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
or the named controller for a managed thread. See [Security](#security) above.

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