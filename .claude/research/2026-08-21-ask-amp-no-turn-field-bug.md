# ask_amp hung for the full timeout when Amp never started a turn

Date: 2026-08-21
Reported from: Claude Code session "Enaia Expo server contract and mobile route
audit" (`6fa3c5bd-7f6d-45a6-894b-918d157cc81a`) — first field report against the
v0.1.0 release.

## Symptom

`ask_amp` returned a timeout after ~4m50s. Two attempts, both silent for the
whole budget. Nothing in the reply, nothing actionable for the caller, and the
question *had* in fact arrived in the Amp thread — the user could see it sitting
there unanswered.

## What the plugin log showed

```
APPEND_OK req=<id> thread=<T>
   (no agent.start at all — not even START_OTHER)
TIMEOUT req=<id> appended=true       at 290s  (300s budget − 10s inboxTimeoutLead)
ORPHAN  req=<id>                     at 2× budget (ORPHAN_FACTOR)
```

Compare a working exchange from the day before:

```
APPEND_OK req=<id> thread=<T>
START_MATCHED req=<id>               1.1s later
```

So the append succeeded and the marker was never seen, because no turn ran at
all — as opposed to a turn running for a *different* message, which logs
`START_OTHER`.

Thread state at the two attempts differed, and that difference is the whole
diagnosis:

| attempt | `thread.state.get()` | meaning |
|---|---|---|
| 1 | `running` | Amp was mid-turn; the appended message sat queued behind it |
| 2 | `idle` | Amp was doing nothing and still never picked the message up |

## Root cause

`appendUserMessage` **appends a message to a thread. It does not start a turn.**
`AppendUserMessageOptions` has exactly one field:

```ts
{ steer?: boolean }  // "Prefer this message when it is queued behind in-progress work."
```

The bridge was calling it with no options, so:

- **Busy thread** — the message went to the back of the queue and waited for
  whatever Amp was already doing. Sometimes that outlasted the budget.
- **Idle thread** — nothing was in progress to finish, so nothing ever triggered
  a turn. The message simply sat there. No amount of waiting would have helped;
  the caller burned the entire timeout on a message that was already delivered
  and would never be answered.

Both look identical from the caller's side: silence, then a timeout that says
nothing about which of the two happened or what to do next.

## Fix

Three parts, all in `plugin/amp-bridge-inbox.ts` plus the Go error mapping.

**1. `steer: true` on every append.** This is the documented lever for exactly
the queued-behind-work case, and it costs nothing when the thread is idle.

```ts
await amp.threads.get(threadID).appendUserMessage(
  { type: 'user-message', content },
  { steer: true },
)
```

**2. A no-turn watchdog.** After appending, poll `thread.state.get()` until a
turn starts. `running` or an unreadable state means keep waiting — the caller's
own timeout is the bound. Any other state (`idle`) with no turn started means
the message will never be answered, so fail *now* rather than at 290s:

```ts
function noTurnDelay(req: ReqState): number {
  return Math.min(NO_TURN_GRACE_MS, Math.max(250, Math.floor(req.timeoutMs / 4)))
}
```

The delay scales with the caller's budget, so a 10s ask does not wait 20s to
learn its answer will never come, and a 5m ask does not poll needlessly.
`STATE_PROBE_MS = 500` bounds the probe itself so a wedged state call cannot
become the new hang.

**3. An error the caller can act on.** The failure carries `code: 'no-turn'`,
and Go turns it into advice rather than a stack trace:

> the message was appended to thread `<T>` but Amp did not start a turn for it
> (thread state: idle). It is sitting in the thread unanswered — open that
> thread and reply, or send it again from there — **do not resend the same
> question, it is already there**

That last clause matters more than it looks. The natural response to a timeout
is to retry, and retrying here posts the same question into the thread a second
time. The error has to say so explicitly, and the SKILL.md table now carries the
same row so the agent reads it before deciding to retry.

## Tests

`plugin/turn.test.ts`, 7 tests, each verified by mutation:

| mutation | tests that went red |
|---|---|
| drop `steer` | the append asks Amp to steer |
| remove the watchdog | idle thread fails fast; reported as already delivered (2006ms — the field symptom reproduced) |
| treat `running`/`null` as stalled | busy thread left alone; unreadable state treated as busy |
| drop pid from log lines | log lines name the pid |

The pid in the log line is not decorative: diagnosing this meant reading
interleaved output from a plugin that had been reloaded, and without the pid
there was no way to tell which process wrote which line.

## Still open

The *why* behind an idle thread ignoring an appended message is unconfirmed with
Amp — the thread used for consultation was unreachable while this was fixed. It
may be intended (an append is not a send), or it may be a bug on that side. The
watchdog is correct either way: it converts an unbounded silence into a fast,
specific error, which is the part the caller needed regardless of the answer.
