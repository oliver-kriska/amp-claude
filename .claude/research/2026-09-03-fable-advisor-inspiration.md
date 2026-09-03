# fable-advisor — what transfers to amp-bridge, and what doesn't

Source: <https://github.com/DannyMac180/fable-advisor> (read at commit on `main`,
2026-09-03). Prompted by Oliver: "maybe we can have some inspiration there".

## What it actually is

A Claude Code **plugin**, not a program: ~400 lines of markdown and nothing else.

```
agents/codex-implementer.md   124 lines   routine lane  → GPT-5.6 Luna via `codex exec`
agents/sol-implementer.md     125 lines   hard lane     → GPT-5.6 Sol via `codex exec`
agents/fable-advisor.md        39 lines   reviewer      → Fable 5.1, read-only
skills/orchestration/SKILL.md  93 lines   the routing doctrine
```

A Fable 5.1 session is the architect: it owns decomposition, specs, routing, and
verification, and "should almost never type implementation code". Implementation
goes to one of two Codex lanes; every deliverable gets a clean-context Fable
review before the architect reports done.

## Why it is not a template for amp-bridge

Different layer. fable-advisor delegates to `codex exec` — a **one-shot
subprocess** that starts, writes files, and exits. It has no live peer, no
socket, no inbound direction, no session that outlives the call. Every hard
problem amp-bridge exists to solve (reaching a thread an interactive Amp session
already holds, a bidirectional channel, delivery ambiguity, orphaned lanes) is
absent by construction.

So: no architectural inspiration for the transport. Real inspiration for the
**contract and the reporting**, which is exactly where amp-bridge's UX is weakest.

## What transfers

### 1. An empty answer is a refusal, not a success — a live bug in amp-bridge

The strongest single idea in the repo:

> **An empty diff is never `complete`.** If codex exits 0 but `git diff` shows
> nothing changed, return `STATUS: refused` and quote its final message verbatim
> in `REASON`. A clean exit code is not evidence that work happened.

amp-bridge has the same hole and does not guard it. Both delivery paths return an
empty answer as success:

- `amp.go:203` — `return out, nil` after the stderr fallback; `out == "" &&
  errOut == ""` is a successful empty answer.
- `inbox.go:393` — `return r.Reply, nil` with no length check.

There *is* a guard on outbound text (`errEmptyText`, `mcp.go:417`) — we refuse to
send Claude's blank message to Amp — but none on the answer coming back. Claude
therefore cannot distinguish "Amp considered it and had nothing to add" from
"Amp never really ran a turn".

**Their diagnosis of the cause is the part worth stealing.** `codex exec` loads
`~/.codex/AGENTS.md` on *every* invocation, so a machine-wide rule written for
one project can make a lane decline — and it comes back `exit 0`, empty diff,
polite refusal in the final message. They observed it live (2026-08-04) and
mitigate it two ways: a scoped opt-out preamble in the spec, and the empty-diff
check as the actual catch ("belt-and-braces, not a substitute").

**amp-bridge has the identical hazard, untested.** The CLI fallback runs `amp
threads continue`, which loads Amp's own user-level config the same way. A
machine-wide Amp rule could make our turn a no-op that we report as a success.
Worth an experiment.

### 2. A fixed report envelope instead of free prose

Every lane returns the same shape:

```
CODEX REPORT
LANE: codex-implementer (gpt-5.6-luna, effort: <as run>)
STATUS: complete | partial | timeout | unavailable | refused
OBJECTIVE / CHANGES / VERIFIED / CODEX SAID / GAPS
```

amp-bridge just gained this on the *async* side — completion events now carry
`done | pending | not-delivered | unknown | error`. The **synchronous** `ask_amp`
return is still raw Amp prose with no envelope, so a caller cannot tell an
answer from a clarifying question from a refusal without reading it.

Note `GAPS` in particular: a structured place for "the spec was ambiguous, here
is where". amp-bridge has nowhere for Amp to say "I need more context" except
inside the prose.

### 3. The six-part spec contract

> Implementers share none of your conversation context.

Objective, files, interfaces, constraints, verification, reasoning effort. And
the sharp corollary:

> A spec you can't finish writing is a signal the decision isn't made yet —
> that's architect work, not a reason to hand the ambiguity to a cheaper model.

`send_amp` documents its text as "self-contained" and stops there. A named shape
in the skill is nearly free and would raise the quality of what Amp receives.
This is the cheapest win on the list.

### 4. Never silently substitute — independent arrival at our own conclusion

> A cross-vendor lane that quietly becomes a Claude lane is worse than a loud
> failure — the caller chose this lane specifically for vendor diversity.

And: "Both lanes fail loudly on a missing or unauthenticated codex CLI — there is
no Claude fallback inside a lane by design." Same principle as the delivery
taxonomy shipped in `0ed553f`: a status the caller can act on beats a plausible
substitution. Corroboration, not a new idea.

### 5. Timeout keeps what landed

> On timeout, report `STATUS: timeout` with whatever landed.

Their wall clock is `600` seconds — the same number as our `sendTimeout` default
and the plugin's `MAX_TIMEOUT_MS`, arrived at independently. But they *return the
partial result*; amp-bridge returns an error and discards whatever the turn
produced. The plugin knows the partial turn output at timeout and could send it.

## What does not transfer

- **The lane/effort routing table.** amp-bridge has one peer, not a model menu.
- **The architect/advisor session split.** Orthogonal — a way to run a session,
  not a property of the bridge.
- **Their honest caveat** ("the advisor and the architect are the same model")
  is a limitation amp-bridge does not have: Amp genuinely is a different agent.

## Prior art in the KB

`scriptorium/patterns/cross-model-readonly-review-protocol.md` (2026-08-25)
already recorded amp-bridge's central failure mode, before the code knew it:

> Bridge windows to interactive sessions (e.g. a 3-minute request window) can
> expire while the other model is still revising; **the request usually did run,
> so do not duplicate it.**

That is exactly the bug fixed in `0ed553f`, and it recurred in this very session
— the report to Amp was dropped with "no pending Amp request matches
request_id …" while `make check` ran.

## Verdict

- **Do:** the empty-answer-is-a-refusal check, both paths. It is a real bug, it
  is small, and it is the one item here that fixes a way the tool can currently
  mislead its caller.
- **Do:** document a spec shape for `send_amp`/`ask_amp` in the skill. Cheap.
- **Consider:** a status envelope on the synchronous `ask_amp` return, so the
  sync and async paths stop disagreeing about how an outcome is reported.
- **Consider:** returning partial turn output on plugin timeout.
- **Test:** whether a user-level Amp config can make `threads continue` a silent
  no-op, the way `~/.codex/AGENTS.md` can for `codex exec`.
- **Skip:** lanes, effort tables, the architect pattern.

## Amp's verdicts (consulted 2026-09-03, thread T-01a042ab-f21a-73dc-a12a-d710bd174124)

The consult itself timed out at 110.0s on the pre-fix build — the message landed
(`thread state: running`) and Amp answered in the thread; recovered with
`amp threads markdown <id>`, which is read-only and starts no turn. Worth knowing
as a recovery path.

Amp corrected me on three of five points. Verified each against the source
before accepting.

### 1. The AGENTS.md hazard is real but does not produce the codex failure mode

`amp threads continue --execute` starts a new process and does reload
`$HOME/.config/amp/AGENTS.md`, `$HOME/.config/AGENTS.md`, system guidance, and
`AGENTS.md` from the invocation cwd and its parents. **But** a global rule
producing a refusal should still emit *nonempty* text, and explicit bridge
instructions outrank conflicting AGENTS.md guidance. So "exit 0 + empty stdout
because a rule declined" is not Amp's refusal contract, unlike codex's.

One genuinely new hazard Amp raised that I had not considered: **the CLI
fallback inherits amp-bridge's cwd**, so its project guidance reflects the
*Claude session's* workspace, not the target thread's original workspace.

Better fix than sniffing for empty output: invoke the fallback with
`--stream-json` and parse the terminal `result` event — `result.subtype`,
`is_error`, `num_turns`, `result`, and the preceding assistant `stop_reason`
(which includes `refusal`). A terminal success with `result == ""` then *proves*
the run reached a terminal state and produced nothing, which is exactly the
distinction we lack. (`--stream-json` confirmed present in `amp threads continue
--help`.)

Test design Amp proposed: a temp repo whose AGENTS.md demands an exact sentinel,
a disposable thread, change the sentinel, continue from the same cwd, assert on
`system.init.cwd` and the terminal result. Test instruction *reloading*
separately from empty output; unit-test empty stream results with a fake amp.

### 2. The inbox hole is narrower than I claimed — and I had the cause wrong

I said `inbox.go:393` returns `r.Reply` with no length check. True, but the real
defect is upstream in the plugin, and the guard already exists on the wrong side
of normalization. **Verified in `amp-bridge-inbox.ts`:**

- `:534` `if (texts.length === 0)` → `turn-error`. Correct, and it already
  distinguishes no-turn (`checkTurnStarted` → `no-turn`), turn failed/cancelled
  (`event.status`), and done-with-no-text (`END_NO_TEXT`).
- `:547` `const reply = texts.join('\n').split(req.marker).join('').trim()` —
  an empty text block, whitespace-only text, or marker-only text survives the
  `texts.length > 0` gate and normalizes to `""`, which `respond(req, { reply })`
  then ships as a success.

**The fix is to move the blank check after joining, marker removal, and
trimming.** The CLI path needs the equivalent after stdout/stderr selection.

### 3. Code should be `empty-answer`, not `refused`

Empty output does not prove refusal — it could be a model or output-contract
defect. `empty-answer` means: delivered, turn reached a terminal state, no usable
answer, do not auto-resend, inspect the thread. A legitimate terse "ack" stays
successful because it is nonempty. Agreed; `refused` was borrowed vocabulary that
asserts more than we can observe.

### 4. Do NOT put an envelope on synchronous ask_amp — reversed my "consider"

Raw prose is right for a synchronous consultation, and wrapping it breaks every
existing caller. Async needs an envelope only because it arrives *outside* the
initiating tool call and must carry lifecycle state. Unify the **semantics and
error codes**, not the presentation:

- synchronous success → raw reply
- synchronous failure → structured code rendered as a tool error
- asynchronous completion → status envelope plus reply/error

This is the better answer and I am dropping the idea of a unified envelope.

### 5. Do NOT promise partial output on timeout — reversed my "consider"

Plugins receive no assistant token/delta event; `agent.end.messages` is the first
authoritative turn-scoped transcript. At timeout the plugin *could* call
`thread.messages()` for already-persisted assistant messages, but it cannot
recover text still streaming, and the snapshot may not represent the final
answer. Best-effort only. Verify what `thread.messages()` exposes while state is
`running` before building anything; if ever added, expose it as an optional
`partial` field on a timeout, never as the successful reply.

## Revised plan

1. **Do now:** post-normalization blank check on both paths, distinct
   `empty-answer` code, classified as delivered-but-no-answer.
2. **Do next:** migrate the CLI fallback to `--stream-json` and parse the
   terminal result. Bigger, and it subsumes the CLI half of (1) properly.
3. **Investigate:** whether the CLI fallback inheriting amp-bridge's cwd loads
   the wrong project's AGENTS.md for the target thread.
4. **Dropped:** unified status envelope on `ask_amp`; timeout partials.
5. **Still worth doing, independent of Amp:** a documented spec shape for
   `send_amp` text.

## Item 1 implemented (2026-09-03)

Post-normalization blank check on both paths, plus a distinct `empty-answer`
code classified as `deliveryFailed` (delivered, terminal, resend is a judgement
call and never the reflex). `make check` green.

Three places notice it, one wording via `errEmptyAnswer(how, threadID)`:

- **Plugin** (`amp-bridge-inbox.ts`, after `.join('\n').split(marker).join('').trim()`)
  — the real fix, where Amp said it belonged.
- **Go inbox** (`askViaInbox`, after the error branch) — a guard against a plugin
  too old to make that report. Not belt-and-braces: picking up the plugin check
  needs a manual reload in each Amp session, so *every* currently-loaded plugin
  is old. Same reasoning as the `delivered` pointer.
- **Go CLI** (`deliverToAmp`, after the stderr fallback) — a clean exit says the
  process ended, not that the agent spoke.

### Incidental finding: marker stripping leaves its brackets

Writing the test for a marker-only answer exposed that
`.split(req.marker).join('')` removes the request id but not the `[...]` around
it, so such an answer normalizes to `"[]"` and stays a non-empty (if strange)
success. Left alone deliberately: widening the strip is a decision about marker
syntax, not part of the blank check, and a marker-only assistant message is
contrived — the marker goes into the *user* message we append. Noted here so the
next person who sees `"[]"` in a reply knows where it comes from.
