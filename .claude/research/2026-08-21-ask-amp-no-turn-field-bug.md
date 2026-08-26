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

---

## Second reproduction, 2026-08-25 — and a much cleaner one

A live test of the new `send_amp` tool reproduced the no-turn condition exactly,
this time with a trivial isolated case rather than a real workload.

Amp asked (over the channel) for a `send_amp` to its own thread carrying a
one-line message: *"reply with exactly ACK LIVE-… and nothing else."* The
completion event came back `status="error"` with thread state **idle** — message
appended, no turn started.

**The test design guarantees the failure, and that is the finding.** Amp ended
its turn to wait for Claude's reply, so by the time `send_amp` delivered, that
thread was idle *by construction*. There was nothing in flight to steer past.

### What this confirms

- The async completion path works end to end: unsolicited event, `async_id` /
  `status` / `thread_id` as attributes, no `request_id`, "no reply is required".
- The corrected no-turn wording ships correctly — semicolon, "ask your user to
  open that thread and reply there", then the Go layer's "do not resend the same
  question, it is already there". No contradiction in the assembled string.
- The watchdog fires fast on an idle thread instead of burning the budget.

### The structural problem it exposes

`steer: true` helps only when something is already running. On an **idle**
thread it has nothing to prefer the message ahead of, so the message just sits
there.

That matters far more for `send_amp` than for `ask_amp`, because of when each is
used. An Amp thread is idle precisely when it is waiting on Claude — which is
exactly when Claude would reach for `send_amp` to hand back background work. So
the tool's main use case lands on the one thread state where delivery does not
wake anything.

**`send_amp` to an idle thread is fire-and-delivered-eventually.** Not a bridge
defect: the append succeeds, the watchdog reports honestly, and the error tells
the user what to do. But the feature's practical value depends on an Amp-side
wake mechanism that does not currently exist.

> **Corrected 2026-08-25 (same day), by Amp's trace.** I first wrote
> "fire-and-never-delivered". That is wrong. Amp traced the live test:
> `APPEND_OK` → `NO_TURN state=idle` at 20s → and **the ACK appeared once later
> user activity caused Amp to process the already-appended message**. The
> message is not lost, it is *latent* — queued indefinitely with no scheduled
> wake. Any activity on the thread drains it; a reply is not special.
>
> This creates a hazard of its own: the plugin gives up at `NO_TURN`, reports
> `error`, and releases the lane, but the message stays queued and *is* answered
> later. **The caller is told the request failed while the work still happens** —
> the ambiguous-delivery class arriving by a new route. "It is sitting in the
> thread unanswered" is true when written but reads as terminal; it should say
> the message will be picked up the next time that thread does anything.

**Amp's API confirmation (2026-08-25):** the published `PluginThread` surface is
`agent()`, `parentThreadID()`, `title`, `state`, `waitForResponse()`, `cancel()`,
`setVisibility()` and message reads — a cancel with no corresponding start. There
is **no wake/start/execute operation for an existing thread**.

The only alternative visible in the API: `agent()` is documented as "suitable for
creating related threads", so a plugin could spawn a **child** thread rather than
append to the idle one. That buys a wake at the cost of answering in a different
thread from the one the user is sitting in — which defeats the point of the
inbox, whose whole value is reaching an open thread. Worth knowing it exists; not
worth building.

**Scope the blockage precisely.** Not both tools equally:
- `send_amp` is genuinely blocked — its premise is handing work to a thread that
  is idle *because* it is waiting on Claude.
- `ask_amp` is degraded but honest — the caller is blocked and the user is
  present, so failing fast with actionable advice is defensible.

This is the same question raised about AgentChute in
[[an-append-is-not-a-send]] — *they had a wake adapter and removed it; what
replaced it?* Amp's `appendUserMessage` has the identical gap, and this test is
the cleanest available demonstration of it.

### Better bug report material than the original

The August 21 case needed log forensics across a real workload. This one is a
two-line repro: arm an inbox on an idle thread, append anything, observe that no
turn starts. Worth sending to Amp upstream in this form.
