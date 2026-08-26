# amp-bridge: concurrency, async, and introspection — three-way design review

Date: 2026-08-24
Reviewers: Amp (proposal + primary-source checks), Claude (this session), Fable
(independent second opinion). Herdr-inspired, prompted by Oliver.

Status: implemented and live-validated on 2026-08-25. This records the agreed
plan and the reasoning behind the rejections so the discarded options are not
re-proposed later.

## Endorsed order

1. **Fix timeout defaults + doctor warning.**
2. **Scope `askMu` to the CLI fallback branch.**
3. **`send_amp` with an explicit `thread_id`, completion pushed as a channel event.**
4. **Revisit pairing semantics** only if async calls should ever support implicit routing.
5. **Introspection last**, limited to states observable without a plugin protocol change.

Explicitly not building: Go-side per-thread lanes, a global semaphore, polling
`wait_amp`, `abandon_amp`.

## 1. The timeout ordering

    config.go:41   ampTimeout  AMP_BRIDGE_AMP_TIMEOUT  300s   Claude -> Amp
    config.go:38   replyWait   AMP_BRIDGE_TIMEOUT      180s   Amp -> Claude

Claude blocked in a synchronous MCP tool call cannot service an inbound channel
event — the transport is fine (tool calls run off the read loop, mcp.go:304-315);
it is Claude's *turn* that is held. A default `ask_amp` can hold that turn for
300s while an inbound request expires at 180s.

**Correction to my first statement of this:** I said any inbound request during
an `ask_amp` was "guaranteed" to expire. Wrong — the bound *permits* the loss,
it does not ensure it, since most turns finish well inside 300s. Amp caught the
overstatement.

The real invariant, and note it is about the *remaining* block at the moment the
event arrives, not the ask's total duration:

    max synchronous Claude->Amp block + recovery margin < replyWait

Recovery margin is not small: Claude must finish the tool call, receive the
injected event, and take a model turn to call `reply`. Budget ~30s.

**Decision: reduce `ampTimeout`, do not raise `replyWait`.** Raising the other
side is compensation — it leaves the harmful quantity untouched and makes
"Claude is not answering" take six minutes to discover. 300s of synchronous
blocking is also a bad default on its own merits: it stalls the session behind
one tool call.

**Dropped from my recommendation:** admission-time enforcement. I argued for
checking a caller-supplied per-request timeout against `replyWait`. Amp pointed
out the public `ask_amp` tool exposes only `text` and `thread_id` — there is no
per-request timeout to violate. `timeoutMs` on the wire is derived from config,
not caller-supplied. Doctor warning suffices; revisit only if `send_amp` later
exposes a timeout.

## 2. Concurrency — do not build lanes

The proposal was to replace the global `askMu` with keyed per-thread lanes. My
recommendation was lanes plus a global semaphore to preserve the subprocess
bound. **Both were wrong; Fable's is better.**

`askMu` (mcp.go:108) is taken at amp.go:97, *before* the `viaInbox` branch at
:100, so one global mutex serialises both transports across all threads. Head-of-
line blocking is real: an inbox ask holds it for up to `ampTimeout` + 10s,
blocking asks to other threads and the CLI path.

But same-thread FIFO **already exists one layer down**, twice over:

- inbox path — the plugin's `lanes` Map with inflight+queue, `QUEUE_MAX=4`
  (plugin:81, 127, 595-608)
- CLI path — Amp itself arbitrates server-side; a concurrent second
  `threads continue` fails `EXECUTOR_ALREADY_CONNECTED`

Also: `askMu` is **per-process**, so two Claude sessions racing one thread were
never guarded by it at all. The plugin queue and Amp's executor lock are what
actually arbitrate today.

Go-side lanes would put a second queue *in series* with the plugin's: two
"queued" states, two timeout regimes, and `busy` becomes ambiguous about which
queue filled. The semaphore is unnecessary because keeping the existing global
mutex for the CLI branch alone preserves the subprocess bound for free.

**Decision: move `askMu` inside the CLI branch. A few lines in amp.go:97-106,
zero new machinery, delivers the proposal's stated goal.**

## 3. Async — push, do not poll

Split-phase (`send_amp` / `wait_amp` / `abandon_amp`) fixes re-entrancy **only
if Claude ends its turn between send and wait**. A blocking `wait_amp` called
right after `send_amp` recreates the identical block. My correction was "make
`wait_amp` a short poll." Fable's is better: **don't poll at all.**

`pushEvent` (channel.go:21-61) already pushes an unsolicited
`notifications/claude/channel` to Claude with `meta.request_id`. So: `send_amp`
returns a handle, a goroutine parks on the inbox connection (correlation *is*
the connection — zero protocol change), and on completion the bridge pushes
"request N completed" as a channel event. Claude gets woken instead of polling.

`wait_amp` and `abandon_amp` both become unnecessary. Abandon-after-delivery was
only ever forgetting a handle locally — the plugin keeps the lane until
`agent.end` regardless — so shipping it as a tool would manufacture an illusion
of cancellation that does not exist.

**Refinement from Amp:** extract a fire-and-forget notification rather than
reusing `pushEvent` unchanged, since `pushEvent` allocates a pending reply slot
and counts against `maxInFlight`. Include request/thread IDs and success/failure
in the completion event.

**Known limitation to document in the tool description:** a mid-session MCP
restart loses in-memory handles, so a completed answer is lost even though Amp
did the work. Say so rather than pretending otherwise.

## 4. Concurrency breaks pairing — the find neither Amp nor I made

`rememberThread` is last-writer-wins (amp.go:45-52). With concurrent asks to
different threads, `lastThread` becomes nondeterministic and a later
`thread_id`-less `ask_amp` silently goes to the wrong thread.

**Amp's correction to Fable, verified:** `rememberThread` is called at amp.go:77,
*before* `lookupInbox` (:88), the `askMu` lock (:97), and any delivery. So
`lastThread` is the last ask to **start**, not to finish. Fable said "finished
last". More precise, same hazard.

**Decision: sidestep rather than solve.** `send_amp` requires an explicit
`thread_id`. Revisit implicit routing only if async ever needs it.

`pushEvent`'s own comment already flags "whichever messaged us last" as a
weakness in the other direction — the codebase half-knew this.

## 5. Introspection — half the proposed states are not observable

The proposal listed accepted/queued/running/completed/failed/expired/abandoned.

**Correction to my own answer:** I called the inbox states "genuinely observed."
They are observed by the **plugin**, not by the **bridge** that would render
them. The wire is one-op-per-connection with a single terminal reply
(plugin:682-683, inbox.go:286-295), and the `status` op carries only
pid/proto/enabled_threads — no per-request state. So queued-vs-running is
plugin-internal, and exposing it is a protocol change disguised as visibility.
`running` is inferred on **both** transports, not just the CLI one.

Per direction:

- **Claude→Amp inbox** — observed: dispatched, terminal outcome, deadline.
- **Claude→Amp CLI** — observed: subprocess spawned, subprocess exited. No
  queued state exists. `running` means "subprocess alive", *not* "Amp started a
  turn" — conflating those re-creates the no-turn bug class in the UI.
- **Amp→Claude** — observed: slot accepted, answered, expired, caller hung up.
  **Not** observed: whether Claude ever saw the event. `EVENT_PUSHED` means
  "flushed to stdout" (mcp.go:234-248), and mcp.go:284-292 documents the case
  where the listener never registered and every health check still passes. Label
  it "written to transport", nothing stronger.

Do not render inferred states in the same visual vocabulary as observed ones —
that is how a status display starts lying. Much of the proposed
"inbox/plugin/build state" already lives in `doctor`.

Note `serveAmpConn` has no op discrimination today — any JSON with `text` is an
ask (channel.go:180-210) — so any new socket op must be versioned carefully.

## Rejected outright (all three reviewers agreed)

Terminal scraping · hash-derived call-signs · auto-retry after ambiguous
delivery · durable Markdown inbox · generic Claude/Codex/Pi orchestration ·
built-in fan-out.

## Process note

Each reviewer corrected the one before it. Amp caught my "guaranteed"
overstatement and my unnecessary admission enforcement; Fable overturned my
lanes-plus-semaphore recommendation and my claim that inbox states were
observable; Amp then corrected Fable's start-vs-finish detail on
`rememberThread`. Three of my five positions changed. The loop is worth its cost
specifically because the reviewers disagree with each other, not because they
ratify.

Related: [[an-append-is-not-a-send]] — the deeper pattern under item 3.
