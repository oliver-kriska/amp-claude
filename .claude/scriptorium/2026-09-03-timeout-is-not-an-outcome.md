---
scriptorium: true
action: create
title: "A timeout is not an outcome"
type: pattern
domain: general
tags: [agent-protocols, timeouts, idempotency, error-design, api-semantics]
---

When a request to an agent times out, "timeout" is the *observer's* state, not
the *system's*. The message either reached the far side or it did not, and the
correct response differs by 180 degrees:

- **Not delivered** — safe to resend. Failing to resend loses the work.
- **Delivered, still running** — resending duplicates it. The work is fine;
  only your view of it timed out.

Collapsing both into one `status: "error"` guarantees the caller is wrong half
the time, and an LLM caller reliably picks "retry" because that is what "error"
means everywhere else. In amp-bridge this turned into real duplicate messages
and real lost ones from the same code path.

### The fix has three parts

1. **A classification, not a flag.** `unknown | not-delivered | pending |
   failed`, carried out with the error rather than reconstructed from its text
   by whoever catches it. Default to `unknown`: only a path that has *proven*
   the message stayed out of the queue may say "safe to retry".

2. **Have the side that knows say so.** The far end already knows whether it
   appended before its clock ran out. It just wasn't saying. One additive
   boolean on the existing timeout response (`delivered: req.appended`), not a
   new error code — an old peer omits it, a new peer reads it, neither has to be
   upgraded first. Represent it as a pointer/optional so *absent* stays distinct
   from *false*; guessing "not delivered" on absence invites the exact duplicate
   the field exists to prevent.

3. **Say the action, not the condition.** "the Amp turn did not finish in time"
   tells the caller nothing it can act on. "the message IS in thread T and Amp
   is still working; do not resend, that would duplicate it" does.

### Where the bug comes from

Usually one shared timeout constant. In amp-bridge a synchronous call and a
fire-and-forget call shared a bound that existed only because the synchronous
one holds a caller's turn open. Nothing waited on the async one, yet it was
being killed at the same wall — 21 of 22 failures landed within a millisecond
of it. A censored distribution masquerading as a failure distribution: if your
failures cluster on a constant rather than spreading out, the constant is the
bug.

Related: [[An append is not a send]] — the same system, one layer down.
