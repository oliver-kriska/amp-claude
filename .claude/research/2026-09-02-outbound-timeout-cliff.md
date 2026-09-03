# The Claude→Amp timeout cliff

Log analysis of `~/.local/state/amp-bridge/amp-bridge.log`, 2026-08-31 through
2026-09-02, prompted by Oliver noticing "a lot of timeouts from claude code → amp".

## Finding

The outbound direction is not failing. It is being cut off at a fixed deadline.

Pairing every `INBOX_ASK` with its `INBOX_OK` / `INBOX_FAILED`:

| outcome | n | durations (s) |
|---|---|---|
| `INBOX_FAILED` | 22 | 107, then **110.0 × 21** |
| `INBOX_OK` | 9 | 13, 23, 26, 44, 64, 67, 84, 87, 108 |

31 attempts, **71% failure**, and **22 of 22 failures are at or past 107s**.
Not one fails early. 110s is exactly `pluginBudget = ampTimeout -
inboxTimeoutLead` = 120 − 10 (`inbox.go:277`).

21 of 22 carry `code=timeout`; the odd one out is `code=turn-error`. Every
`ASK_AMP_FAILED` reads *"the Amp turn did not finish in time (thread state:
**running**)"* — the append landed and Amp was still working when the bridge
gave up.

The successes confirm it from the other side: 13s–107.8s, median 63.5s, and the
maximum sits **2.2s under the wall**. That is a censored distribution, not a
failure distribution. Every turn that fits is kept, every turn that doesn't is
discarded, and 68% don't fit.

## Contrast: inbound is healthy

Same window, Amp→Claude: 36 replied of 49 (73%), durations spread continuously
15s → 236s. The 13 timeouts spread across 180 / 300 / 600 / 900s deadlines rather
than piling on one value. That spread is what real variation looks like; outbound
has none of it.

Per-day outbound ratio is flat — 6/8, 7/12, 6/8 — so this is steady state, not a
regression from the v0.2.0 work.

`SOCKET_ACCEPT_ERROR` appears 51× and is benign: all "use of closed network
connection" at session shutdown.

## The asymmetry

v0.2.0 gave **Amp→Claude** per-request deadlines, a 15-minute ceiling, and
late-reply retrieval — the direction that was already working. **Claude→Amp**
kept a fixed 120s with no per-request override, and it is the direction losing
two thirds of its traffic. The feature went to the wrong side.

## Three fixes, in order

1. **`send_amp` should not be bounded by `ampTimeout` at all.** It is
   asynchronous and does not block the Claude turn; the only reason its budget is
   120s is that it shares a constant with the synchronous path.
2. **`ask_amp` must fit inside the inbound request's remaining budget** — which
   is what `ampTimeout` is really protecting and what `checkTimeoutOrdering`
   enforces. But that budget is no longer a constant: v0.2.0 put `timeout_ms` on
   the channel event, so `ask_amp` can derive its deadline from that minus
   elapsed. The inbound feature supplies exactly the number the outbound side
   needs.
3. **The reporting shape is what the user actually feels.** Being told "failed"
   for a message that was delivered and is being worked on is the worst available
   outcome: the safe action (don't resend, avoid a duplicate) and the useful
   action (resend, get an answer) point in opposite directions. Confirmed live —
   Oliver has a screenshot of the Amp side rendering `Claude Code session
   "enaia-85" asks via amp-bridge [amp-bridge-req-d864b1bee059]` for a send the
   Claude side had reported as failed twice. The inbound direction already solved
   this: caller times out, answer is retained, `--result` fetches it. The mirror
   image is a `send_amp` whose completion still arrives after the budget expires.

## Reproduced live while reporting it

The consultation carrying this analysis to Amp became the twentieth data point:

```
14:04:44.550778  INBOX_ASK    thread=T-01a042ab req=bfc2f369f324 pid=20977 bytes=4212
14:06:34.564334  INBOX_FAILED thread=T-01a042ab req=bfc2f369f324 code=timeout
```

110.013s. The append succeeded — the plugin holding the thread took it at
pid 20977 — and Amp was mid-turn when the bridge gave up. A second thread
(`T-01a05745`) hit the same wall at 14:02:12 in the same window.

The message is not lost; it is in the thread and Amp is answering it. It simply
cannot come back as a `send_amp` completion, because that path was already
closed. Amp can still deliver the answer via `amp-bridge --ask` — the direction
that works.

## The deeper flaw, and the rewrite

Outbound models an Amp turn as a **synchronous RPC with a deadline**. An agent
turn is open-ended work with no natural bound, so any fixed deadline produces
exactly the censored distribution above. A bigger number moves the cliff; it does
not remove it.

The fix is the shape already built inbound: the request becomes a **durable id**,
the outcome is written against that id whenever it lands, and the caller
retrieves it. Waiting becomes an optional convenience rather than the mechanism.
Under that model `ask_amp` and `send_amp` collapse into one primitive with two
waiting policies, and both directions of the bridge finally share one mental
model — a large simplification of the skill as well as the code.

Risks worth naming: retained state is memory-only, and outbound turns are longer
than inbound waits, so a restart mid-turn loses more (this likely wants state
under the runtime dir — real scope increase). Slot accounting changes meaning, as
a slot would be held for the duration of Amp's *work*, not of a *wait*. Plugin
reloads drop the thread watch, though the controllers/intent work is the
precedent for re-arming. Any new status value is a wire change the skill and
plugin must agree on.

**`send_amp` should be separately bounded, not unbounded.** Unbounded means a
stuck turn holds an `asyncSlots` slot forever; with `maxInFlight` at 8, a handful
converts a timeout bug into a total outage. 15 minutes is the natural bound —
it already matches `maxReplyWait`.

### Verdict

- **Blockers:** decouple `send_amp`'s bound from `ampTimeout`; stop reporting
  delivered-and-running as `error`.
- **Optional:** the unified request-id rewrite; retained outbound completions;
  `timeout_ms`-derived `ask_amp` deadlines (needs a cold-call fallback and a
  disambiguation rule when several inbound requests are pending).
- **Cheap alternative** if the rewrite waits: split the one constant in two —
  `ampTimeout` for the synchronous path, a larger separate bound for `send_amp`.
  No new state, no wire change; recovers the 19 `send_amp` attempts in this
  window but does not fix the reporting overload.

## Unrelated, seen in the same transcript

`UserPromptSubmit hook timed out after 10s` / `after 30s — output discarded`.
That is a Claude Code hook, not amp-bridge. Worth chasing separately.

