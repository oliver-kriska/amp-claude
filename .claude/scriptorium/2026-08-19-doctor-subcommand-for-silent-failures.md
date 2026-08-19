---
scriptorium: true
action: create
title: "Ship a doctor subcommand when the failure modes are silent"
type: pattern
domain: general
tags: [cli, tooling, failure-modes, observability, dx, diagnostics, go]
---

# Ship a `doctor` subcommand when the failure modes are silent

Some tools fail *loudly*: they crash, exit non-zero, print a stack trace. Those do
not need a diagnostic — the error message is the diagnostic.

The tools that need one fail *quietly*. The process runs. The logs look ordinary.
Every component reports healthy. And nothing arrives. For that class, a `doctor`
subcommand is not a nicety; it is the only thing standing between the user and an
afternoon of bisecting their own environment.

## How to know you are in that class

Ask: **can every individual part be fine while the whole thing is broken?** If
yes, ship `doctor`.

Concrete instances from `amp-bridge` (a Claude Code ↔ Amp bridge, Aug 2026), all
of which present identically as "I send a message and nothing happens":

| What is wrong | Why nothing reports it |
|---|---|
| The client was launched without the flag that loads the plugin | The plugin is simply never asked to run. There is no error because there was no attempt. |
| `.mcp.json` points at a build from three commits ago | That binary starts fine, handshakes fine, and serves the old behaviour. |
| The binary was replaced with `cp`, invalidating its macOS signature | The kernel SIGKILLs it at exec. The parent reports only "server not found". |
| The registry entry was never published | The server runs, healthy and undiscoverable. Clients report "nothing is running." |
| The runtime directory is a symlink another user planted | Discovery succeeds — into the wrong directory. |

Note the shape, the same one as [[A gate that cannot run must not report a verdict]]:
a *precondition* failure is being rendered as a *content* result. "No sessions
found" is reported when the truth is "I was never able to look."

## What the subcommand does

One line per check, in dependency order, so the first failure is usually the
cause and not a symptom:

```
[ok  ] binary          /Users/you/.local/bin/amp-bridge
[FAIL] mcp config      points at /nonexistent/amp-bridge, which does not exist
       fix: amp-bridge init
[ok  ] runtime dir     /tmp/amp-bridge-501
[warn] live sessions   none — start one with `claude --dangerously-…`
```

Four rules that make the difference between a real diagnostic and decoration:

1. **Compare against reality, not configuration.** The highest-value check was
   `.mcp.json`'s target path versus `os.Executable()` — "what you configured" vs.
   "what is actually running." Configuration validated only against its own schema
   catches nothing; drift is where the bug lives.
2. **Every failing line carries the command that fixes it.** A diagnostic that
   names a problem without naming its remedy has moved the user from *confused*
   to *informed and still stuck*.
3. **Exit non-zero on a real failure.** Then it works as a CI gate and a setup
   post-check, not just something a human squints at.
4. **Three states, not two.** `ok` / `warn` / `FAIL`. "No `.mcp.json` yet" is a
   warn — you may not have run setup. "Points at a file that does not exist" is a
   FAIL. Collapsing these makes the output either alarmist or useless.

## Test it by breaking things

**A diagnostic that always reports green is worse than no diagnostic**, because
it converts "I do not know what is wrong" into "I have confirmed nothing is
wrong." Every check must be verified against a deliberately broken system, not
only a working one:

- no config file → warn, exit 0
- config pointing at a deleted path → FAIL, exit 1
- run the repair command → recheck green

This is the same discipline as mutation-checking a test: break the thing, confirm
the check goes red, restore. See [[A gate that cannot run must not report a verdict]].

## Pair it with `init`

`doctor` names the problem; something must fix it. A companion `init` that writes
the correct configuration closes the loop, and a `make setup` that runs
build → install → init means the diagnostic is usually only needed when something
drifted later.

`init` rules: **merge, do not overwrite** (other entries in a shared config file
must survive), **refuse a file you cannot parse** rather than clobbering it, and
**resolve paths at runtime** — a checked-in config with an absolute path from
someone else's home directory is exactly the drift `doctor` then has to catch.
