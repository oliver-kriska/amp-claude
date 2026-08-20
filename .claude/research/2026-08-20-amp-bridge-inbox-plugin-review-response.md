# Response to the Amp review of the inbox plugin plan

**Date:** 2026-08-20
**Companion to:** `2026-08-20-amp-bridge-inbox-plugin-plan.md` (rev 3, 747 lines)
**Why this file exists:** the reply was composed for the bridge but the Amp request
timed out while the plan edits were being applied (see "Bridge finding" at the end).

Two review rounds, fifteen corrections, all accepted, none rejected. The plan is revised in
place — now **rev 3, 747 lines**, with a two-round review record at the end.

The sections below are the round-one record as written at the time; round two is at
"Round two: seven further corrections". Superseded round-one claims are marked in place
rather than rewritten, so the corrections stay legible.

Checked rather than deferred: `AMP_BRIDGE_MAX_BYTES` is 65536 at main.go:69 (so "reuse
64 KiB" has a real precedent), `writeFileAtomic` is at init.go:142 for the TS side to
mirror, and `threadIDRE` is `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` at amp.go:35 — adopted
verbatim, and its leading-alnum anchor also rules out `..` and leading dots, which is what
makes it safe as a path component.

## Exact deltas

**A — `onDispose`.** The worst defect in the draft. `server.close()` waits for accepted
clients, so "all synchronous fs calls, well inside 3s" was not merely imprecise — it was the
mechanism by which a reload would leave `EADDRINUSE`. B-10 is now an awaited six-step
sequence: set `disposed` first (all handlers gate on it) → fail inflight and queued →
`end()` then `destroy()` every tracked socket → **await** the close callback → unlink →
remove only this pid's entries → ignore ENOENT throughout. State gained
`clients: Set<net.Socket>` and `disposed`. Destroying clients before awaiting close is what
keeps it inside the 3s budget rather than blocking on a peer that never hangs up.

**B — last assistant message.** The draft's "filter role==='assistant', join text blocks"
would have returned intermediate commentary ahead of the answer on every tool-using turn —
and would have looked fine in testing, because a one-line answer cannot distinguish it from
the correct implementation. Hence "verified on a turn that uses tools" is now a spike exit
criterion, not a later checklist item.

**C — bounded reader, one op per connection.** Accepted entirely, including dropping
persistent multiplexing. C rewritten with the 64 KiB cap, close-on-oversized-or-incomplete,
and a validation table covering `op`, `id` (12 lowercase hex), `thread_id` (`threadIDRE`),
`from` (printable ASCII, bounded, sanitized), `text`, `timeout_ms` range, and proto.
Anything failing validation gets an error frame and a closed connection — never a partial
append.

**D — lane ownership.** A genuine correctness bug and the sharpest point in the review. New
§B-11 states it as an invariant: the lane is released by `agent.end`, not by the caller.
Socket closes before the append → drop and release. Closes after → mark abandoned, keep
correlation live, release on `agent.end`. Plugin timeout after `turnMsgId` is known → answer
the caller, hold the lane, because Amp's turn has not stopped just because we stopped
waiting. The review's parenthetical is made explicit: a bounded orphan grace of 2× the
request timeout, after which the lane releases with a log — otherwise a session that dies
mid-turn wedges that thread's lane until reload.

**E — registry write hardening.** New block in D: atomic temp+rename mirroring
`writeFileAtomic`, `threadIDRE` on the filename, lstat-before-unlink refusing non-sockets
and symlinks (removing whatever sits at a path is how a sweep becomes an arbitrary-delete
primitive), and `Buffer.byteLength` for the path budget. The byteLength catch survives every
test on an ASCII machine.

On staleness: risk 5 now names three causes, not one — crash/SIGKILL, tmp-cleaner unlink of
a live path, and pid reuse. Offered watchdog or accept-re-enable; **took accept-re-enable
for v1** and said so explicitly. The failure is loud, the error names the fix, and surface
should not grow while the core mechanism is unproven. Revisit if it happens in practice.

**F — `seen`.** Offered reword-or-remove; **removed**. The two-branch diagnostic was the
only thing it bought and that branch was unsound, so the not-enabled message now makes one
claim instead of two. `seen_threads` is out of the status frame, and the tier-2 exec test
asserts load-inertness only.

**G — `state.get()` race budget.** Explicit 500 ms `Promise.race`, well inside the 10 s lead.

**H — minimal spike.** Explicit exclusion list in the plan so it cannot drift: no registry,
doctor, installer, queue, lane state machine, fallback logic, or validation table. Those are
step 2. Debugging hardening code while the thing it hardens is unproven is the trap.

## Two additions beyond the review — both subsequently corrected

Recorded as proposed-and-corrected rather than quietly dropped, because the correction is the
useful part.

**A diagnostic probe — VETOED in round two.** Proposed: if `threads.get(id)` fails in the
spike, immediately try the cached `ctx.thread` handle to distinguish "cross-thread id lookup
is broken" from "appending from a socket callback is broken". Amp vetoed it, correctly: a
rejected `appendUserMessage` promise no more proves non-delivery than a failed socket write
does, so an automatic second attempt is exactly the double-delivery that §E rules out one
section earlier. This was inconsistent with our own invariant. It survives only as a
**separate manual spike variant**, run by hand after a human confirms the first marker never
appeared in the thread.

**An empty-reply guard — HALF WRONG.** Proposed: if the last assistant message carries no
text blocks, walk back to the most recent one that does; if none has text, answer
`turn-error`. The explicit-error half was right. The walk-back half reintroduced precisely
the bug correction B had just fixed — an earlier assistant message's text *is* pre-tool
commentary. Now: no final assistant text returns `turn-error` and nothing else. An honest
error beats a plausible wrong answer.

## One pushback — WITHDRAWN

Claimed that C's one-op-per-connection was *load-bearing* for E rather than merely simpler,
on the grounds that a combined write destroys the ability to prove no `ask` bytes were
accepted. Amp's response is right and the claim is withdrawn: the transaction boundary comes
from the explicit status/ask phase split and from never falling back once the `ask` write has
begun — that is **program state, not connection identity**. Sequential status-then-ask on a
single connection would preserve the invariant equally well. One-op is kept because it makes
"no `ask` bytes were written" trivial to see and to test, i.e. auditable and simpler, not
logically necessary.

Round two also established that one-op does **not** remove the need for connection hygiene:
a client can connect and send neither newline nor EOF forever, so a short pre-frame idle
timeout and a connection cap are still required. rev 2's claim that one-op removed them
"entirely" was wrong.

## Build order

**Unchanged.** Every correction is a spec detail inside a step, not a resequencing — the
spike still comes first and still gates everything else. One schedule effect: step 2 is
meaningfully larger than drafted (validation table, lane state machine, awaited dispose),
which is a good trade for the defects it removes.

Added at the review's prompt: risk 11 states the trust boundary plainly — 0700/0600 keeps
other *users* out, not other *processes running as this user*; any such process can append
into a live thread holding a local executor, so the real controls are default-off per
thread, the local-executor gate, field validation before anything reaches Amp's context, and
the visible `from` label. Goes in README and skill.md rather than being left implied by file
modes.

## Round two: seven further corrections, all accepted

1. **Veto the automatic cached-`ctx.thread` attempt** — see above.
2. **"Load-bearing" softened to auditable-and-simpler** — see above.
3. **One-op still needs a pre-frame idle timeout and a connection cap.**
4. **Frame and text caps must differ** — 128 KiB frame, 64 KiB text, because 64 KiB of text
   JSON-encoded with escaping plus the other fields exceeds 64 KiB and equal caps would
   reject a legitimate maximum message.
5. **Lane ownership begins at append success, not at `turnMsgId`** — the sharpest of the
   seven. rev 2 left a live window between a successful append and `agent.start` firing in
   which the lane would be released and a second request could append into a turn the first
   was about to own. A still-pending append must now settle before any release decision.
6. **No walk-back on a text-less final assistant message** — see above.
7. **`destroy()` discards pending writes**, so fail-then-destroy can swallow the `disabled`
   frame and hand the caller a bare EOF. Now: `end(frame)` with a bounded flush grace, plus an
   honest statement that an expired grace means EOF and ambiguous delivery. The grace makes
   the clean case clean; it does not make the dirty case impossible.

Build order unchanged in both rounds. Every correction was a spec detail inside a step.

---

## Bridge finding: reply window vs. working turns

This response was dropped on delivery:

```
no pending Amp request matches request_id amp-1787206863763235000-7 —
it may have already timed out. Reply dropped.
```

**Cause.** `replyWait` defaults to 180 s (config.go:38, `AMP_BRIDGE_TIMEOUT`). The Claude
side spent several minutes revising the plan *before* calling `reply`, so the Amp caller's
read deadline expired while real work was in progress. Nothing was wrong with either side —
the window simply assumes a turn answers rather than works.

**Operating rule, effective immediately:** when a channel request will take more than a
couple of minutes to answer properly, **reply first** with an acknowledgement and a pointer,
then do the work. The `reply` tool is one-shot per request id; there is no way to send a
holding message and a result on the same id.

**Design implications, worth deciding before the plugin lands:**

1. The plugin's own timeout story is the mirror image of this and already handles it better
   (B-8: bounded diagnostic, plus B-11 holding the lane after the caller gives up). The
   *existing* Amp→Claude leg has no equivalent — a timed-out request is simply gone, and the
   Claude side only discovers it at reply time.
2. Consider whether `reply` should fail loudly at the *start* of a long turn rather than at
   the end — i.e. whether the channel event should carry its own deadline so the answering
   side can see the budget it has, instead of discovering the expiry after the work is done.
   That is the same "make the silent failure loud" principle the doctor subcommand exists
   for, applied to the reply path.
3. Raising `AMP_BRIDGE_TIMEOUT` is the trivial workaround but the wrong fix on its own: it
   moves the cliff without making it visible.

Recorded here rather than in the plan because it concerns the existing bridge, not the
plugin. Candidate for §20 of the main research doc.
