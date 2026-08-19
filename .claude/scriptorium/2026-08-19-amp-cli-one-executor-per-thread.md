---
scriptorium: true
action: create
title: "Amp CLI: one executor per thread, and where the real error hides"
type: tool
domain: general
tags: [amp, cli, automation, headless, failure-modes, debugging]
---

# Amp CLI: one executor per thread, and where the real error hides

Two facts about driving Amp headlessly that cost an afternoon and produced a
confident wrong diagnosis on the way.

## 1. `amp threads continue --execute` cannot touch a thread you have open

Amp permits **one executor per thread**. An interactive `amp` session sitting in
a thread holds that slot. Any attempt to attach a second — which is what
`threads continue --execute` does — is rejected, non-retryably:

```json
{"level":"ERROR","message":"[thread-client] Executor handshake rejected by server",
 "code":"EXECUTOR_ALREADY_CONNECTED",
 "existingExecutorInfo":{"capabilities":{"workspaceId":"…","pid":50778}}}
```

The payload names the PID holding it, which is how you confirm it rather than
guess.

**The implication for automation:** scripting Amp works on *quiescent* threads
and never on the one you are working in. Any design that assumes "I can push a
message into the thread the human is using" is wrong on Amp. Push has to travel
the other way — the interactive session initiates, and the automation answers.
Discover this before building on the assumption, not after.

## 2. Amp's stderr is not where the error is

On failure the CLI prints only:

```
Error: Unexpected error inside Amp CLI.
Use 'amp threads report T-… ' to generate a diagnostic report for the Amp team.
```

That message is identical for unrelated causes, so it invites — and survives —
misdiagnosis. The actual reason is in the structured JSON log:

- default: `~/.cache/amp/logs/cli.log`
- or wherever you point `amp --log-file <path> threads continue …`

For automation, **always pass `--log-file` to a private temp file and parse it on
non-zero exit.** The shared log has other `amp` processes interleaved into it.
Grep for `"level":"ERROR"` and take the first one — later errors are usually
cascades of it. Report nothing rather than a guess when the log has no ERROR, so
the CLI's own stderr still shows through.

`--log-file` is a global option and works before the subcommand:
`amp --log-file X threads continue <id> --execute <text>`.

## 3. The same call fails differently depending on stdin

With a terminal or an inherited pipe on stdin, the failure is
`Timeout while reading from stdin`. With `stdin < /dev/null` (or Go's
`cmd.Stdin = nil`) you get the real underlying error instead. When reproducing an
automation failure by hand, **match the stdin the automation used** — otherwise
you reproduce a different bug and conclude the automation is fine, or isn't.

## The meta-lesson

"The thread is wedged" fit every observation available and was wrong. It survived
because no experiment was run that could contradict it — no second thread tried,
no log read. When a tool reports its own error as *unexpected*, that is a
statement about the tool's self-knowledge, not about the cause; the next move is
to find where it writes what it does know. See
[[A gate that cannot run must not report a verdict]] for the same shape one level
down, in checks rather than diagnoses.
