# Plugin reload deletes the user's opt-in: intent must key to host, not worker

Date: 2026-08-25
Status: review response. Amp asked for a design check on its proposed fix; the
reply could not be delivered (see "Delivery note" at the end), so it is recorded
here instead.

## The bug

Amp runs each plugin in its own **worker process**, and reload replaces that
worker's PID — observed live: 60392 → 82943.

Our intent files are keyed to `process.pid` + plugin start time. So
`rearmFromIntents` sees a recorded pid that is not the current pid, finds it
dead, and deletes the intent:

    plugin/amp-bridge-inbox.ts:979-985
    if (pid !== process.pid) { if (!pidAlive(pid)) removeIntent(threadID) }

After reload the opt-in is gone and doctor loses the thread.

**The category error is the real defect:** a USER DECISION is deleted on the
strength of a PROCESS fact. My original design assumed reload meant *same
process, new module instance* — Amp replaces the worker instead, so the
liveness test asks about the wrong process and answers "dead" for a host that
is very much alive.

## Verdict on the proposed fix

Proposal: key intent ownership to `process.ppid` (the interactive Amp host) plus
a parent start token from `ps -o lstart= -p <ppid>`; keep `plugin_pid` for live
socket registrations only.

Direction is sound. Three problems.

### 1. Portability — `ps -o lstart=` is the weak point

- Not POSIX. BusyBox `ps` does not support it (Alpine).
- Output is **locale-dependent** (`LC_TIME`). A locale difference between write
  and read produces a token mismatch, which under the proposed rule *sweeps a
  live opt-in* — turning a portability gap into the exact data loss being fixed.

Use POSIX **`etime`** instead: it is in POSIX `ps`, works on macOS and Linux, and
is locale-free. Host start ≈ `now − etime`, compared with the tolerance already
in the file (`START_TOLERANCE_MS`, :124). It also keeps the mechanism consistent
with `PROCESS_STARTED_AT` (:123), which is derived from uptime the same way.

On Linux prefer `/proc/<ppid>/stat` field 22 when present — integer ticks, no
subprocess at all. Run any `ps` with `LC_ALL=C` and a hard timeout.

### 2. Failure mode — the most important rule in the change

When the token cannot be read or parsed there are **two distinct wrong answers**:

- fail **open** → adopt another host's intent (leak)
- fail **closed by deleting** → silently lose the opt-in (the current bug, by
  another route)

**On uncertainty, neither adopt nor delete.** Leave the file and log. Deletion
must require *positive evidence the host is gone* (pid absent), never mere
absence of evidence about it.

Security posture is otherwise fine: the runtime dir is per-uid, 0700, and
ownership-checked, so cross-user adoption is not the threat. Same-uid PID reuse
is, and only the start token addresses it.

### 3. Verify the ppid assumption empirically

It is load-bearing and cheap to check: confirm 60392 and 82943 actually shared a
ppid. If Amp uses a supervisor or a worker pool, `ppid` may be an intermediate
rather than the interactive host, and the scheme keys to the wrong thing. Also
ensure `ppid === 1` (or launchd) is never accepted as a valid owner — an orphaned
worker re-parents there.

## A simpler identity, in preference order

**(a) Ask whether Amp already exposes one.** An env var the worker inherits, or
an API field. Five minutes of dumping `process.env` in a worker and diffing
across a reload settles it: anything constant across 60392→82943 but different
between two concurrent Amp hosts beats any OS-derived identity — no subprocess,
no locale, no PID reuse, no re-parenting.

**(b) Invert the model.** Liveness is nearly self-answering: *a plugin that is
running is by definition inside a live host.* What rearm actually needs is
**ownership** ("is this thread mine?"), not **liveness** ("is some process
alive?") — and ownership is answerable at the Amp layer. On load, attempt the
rearm and let Amp decide: if `amp.threads.get(id)` does not resolve, or the
thread is not one this host knows, drop it; otherwise adopt.

Intents then become *hints*, a stale hint costs nothing, and OS identity leaves
the design entirely. Sweeping becomes garbage collection — an age-based TTL —
rather than a correctness gate. A TTL is portable, locale-free, and **cannot
silently delete a live opt-in**.

If (a) turns up nothing, this is what I would build rather than hardening the
PID scheme.

## Two smaller points

- **Keeping `plugin_pid` for live socket registrations is right.** The socket
  filename is already pid-keyed (:113) and a socket genuinely does belong to a
  process. *Process identity for sockets, host identity for intent* is the
  correct seam.
- **Define the migration end state for legacy files.** A legacy intent lacking
  host fields should be adopted **once and rewritten** in the new shape, not
  evaluated under the old rule indefinitely — otherwise anyone who opted in
  before the upgrade keeps the current bug forever, and nobody will report it
  because the symptom is silence.

## Delivery note — the bug demonstrated itself

This response could not be delivered.

1. The `reply` window (180s) expired while I was analysing. My own fault: an
   unbounded `find /` burned two minutes of it. The lesson already recorded in
   [[reply-before-long-work-on-amp-bridge]] applies — and "don't run an
   unbounded filesystem search inside a deadline" is the sharper version.
2. The fallback, `ask_amp`, then failed with `EXECUTOR_ALREADY_CONNECTED`:

       that thread is already open in an Amp session (pid 50778)

   Because the reload deleted that thread's intent, its inbox is not armed, so
   delivery fell through to the CLI — which cannot attach to a thread someone is
   sitting in.

**The reload bug severed the channel used to discuss the reload bug.** That is
the strongest available argument for fixing it: the failure is not cosmetic,
it silently removes the only path into an open thread, and the symptom is
silence rather than an error anyone sees.

## Resolution — consent belongs to the thread

The parent-host identity fix solved worker reloads but was still too narrow for
an Amp process restart: it treated the user's opt-in as permission granted to a
particular process rather than to the thread the user selected.

The final model separates three identities:

- **Consent** belongs to the exact thread and remains until explicit Disable.
- **Registration/socket** belongs to the disposable plugin worker.
- **Last serving host** is a routing hint, not the authority to delete consent.

On a plugin reload under the same Amp host, the PID plus start token re-arms all
registrations that host already served. On a different host, the plugin re-arms
only when `activeThread` at load or a `session.start` event proves that the exact
consented thread opened there. A global `threads.get(id)` lookup is deliberately
not enough: it proves visibility, not executor ownership. Intents from another
or dead host stay dormant rather than being swept.

This preserves the security property that a never-consented thread stays closed,
and a Disable remains final because it removes the intent. Legacy worker-owned
intents are migrated only after the exact thread is safely re-registered.

Verification: 18 Bun plugin tests / 45 assertions, including cross-host reclaim,
PID-reuse refusal, explicit-disable persistence, and the never-consented guard;
full `make check` passed. A live plugin reload moved this thread from worker
35954 to 48807 and logged `REARMED ... trigger=same-host`; doctor then confirmed
the new registration.

## Managed threads: consent needs a controller

A later field case exposed a different gap. Thread
`T-01a0335c-7794-769d-b5b4-f8a8b8bb2347` was created and managed by another Amp
thread, so it had no local palette in which the user could run “Enable Claude
inbox for this thread.” The target had never consented: neither its intent nor
its live registration existed. Separately, its calls to Claude omitted
`--thread`, so Claude received `request_id` but no return address and never
attempted `ask_amp` or `send_amp` to that target.

Two changes close those gaps without machine-wide auto-enable:

1. The shared skill now teaches the Amp side to always pass its current thread
   id with `--thread`. A successful synchronous reply is not evidence that
   Claude knows where a later outbound request should go.
2. A local Amp thread can explicitly enable a named managed/background thread
   by URL or id. The local thread becomes `controller_thread_id` in the durable
   intent. On restart, only `session.start` for that controller can reclaim the
   managed target; opening the target in another host is deliberately not a
   second ownership proof.

The controller is load-bearing arbitration, not UI metadata. If both the target
and controller could independently prove ownership, two Amp hosts could serve
the same target and race to overwrite `threads/<id>.json`. Amp's one-executor
invariant applies to the chosen ownership thread, so controller-only reclaim
keeps a single owner while unrelated hosts leave the consent dormant and intact.

## Independent review: addressability versus delivery

A repo-scoped Claude review approved the controller design and verified the
ownership, validation, reload and ordinary-consent guards in the implementation.
It separated the feature's two effects precisely:

- Named consent always restores **addressability** for a managed thread with no
  palette of its own.
- It completes **delivery** automatically only while that target is running.
  `appendUserMessage` cannot wake an idle Amp thread with the current plugin API.

The review found one lifecycle gap: if a managed consent was dormant and its
controller had been deleted, no UI path could revoke the saved intent. The
Disable command now recognizes a readable dormant managed intent and, after an
explicit confirmation from any local Amp thread, removes its consent and stale
registration. It still cannot revoke an ordinary thread's consent.

The same rule applies if that pairing is still live in the current Amp host:
after confirmation, another local thread may disable it. Grant remains narrow,
but revoke is deliberately broad because it can only remove access. This closes
the same-host window between controller deletion and the next Amp host restart.

The controller also became the human wake path. When a managed target remains
idle after append, the caller still receives the non-duplicating `no-turn`
diagnostic, and the plugin now notifies the controller UI with the target id and
asks the user to continue there. This does not pretend to create a turn; it puts
the actionable signal in the one local session positioned to act on it.

## Live return-path test: steering has no second start event

Thread `T-01a03d38-e137-77cc-a873-78a16e778f97` exposed a distinct correlation
bug. While that thread was blocked in a shell tool, `ask_amp` appended a marked
message with `steer: true`. Amp processed the steering message inside the
already-running turn and produced the exact requested ACK. The plugin still
returned `no-turn` because it associated requests only in `agent.start`, and an
in-turn steer does not emit a second start event.

`agent.end.messages` does contain the marked user message and the assistant
answer. The end handler now uses that transcript as the fallback correlation
point, considers only assistant messages after the marker, and returns the last
one. That preserves the existing tool-turn rule while excluding commentary from
before the steer. A regression test models a turn that starts before the bridge
request exists and proves the answer is captured at end.

The same field test revealed that client `--thread` accepted a truncated source
id because its value was carried only as notification metadata; request-id
correlation still made the forward call look successful. Client parsing now
requires Amp's complete UUID-shaped `T-…` id and rejects the typo before sending,
so a missing character is loud rather than a silently broken return address.

After reloading the fixed plugin, a second live request proved the complete
steering path. The target was already running, the bridge request was appended,
and the plugin log recorded `END_MATCHED_STEER` followed by `RESPONDED`; Claude's
`ask_amp` call received the exact `ACK RB-1787733800`. This distinguishes the
now-working running-turn path from the still-explicit idle-thread limitation.
