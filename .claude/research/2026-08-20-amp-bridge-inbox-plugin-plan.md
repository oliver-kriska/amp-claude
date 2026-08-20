# amp-bridge inbox plugin — implementation spec

**Date:** 2026-08-20
**Status:** rev 4 — reviewed by the Amp development thread over the bridge in two rounds,
revised, and **the H-1 spike has passed** (2026-08-20). Assumption 1 is settled
affirmatively: `threads.get(id).appendUserMessage()` works from a socket callback on a thread
the plugin never received through an event. Full results in §20 of
`2026-08-19-amp-claude-code-bridge.md`. Steps 2–8 are cleared to proceed.
Verdict: plugin route necessary, design correct. The gate — append plus
`agent.start`/`agent.end` correlation proved live — **has been passed**;
Amp's ruling on the result: *"Proceed with Step 2 as scoped; do not add another subsystem."*
**Base:** clean tree at `efec084`, `make check` green
**Author:** `plugin-planner` (Fable 5 subagent), commissioned after Oliver authorised the build
**Problem it solves:** §18 of `2026-08-19-amp-claude-code-bridge.md` — Amp permits one
executor per thread, so `ask_amp` cannot reach a thread that is open in an interactive
session. The plugin gives that session an inbox so Claude can knock on the door.

## Research basis

- `amp plugins show-docs` dump (Amp `0.0.1787203428-g9220f3`, 1887 lines). `[docs L…]`
  references below are line numbers in that dump.
- Both installed cmux plugins read; all five Go files under `amp-bridge/` read; §17–§19 read.
- An `amp plugins exec` probe which independently reproduced the Amp-side finding
  (`No handler for plugin request method: thread.state`) and additionally verified:
  initializer runs under exec, `node:net` Unix sockets bind, `executor.kind` is `'unknown'`
  under exec, `onDispose` runs on graceful exit.

Code anchors spot-checked against the tree before filing: identity.go:270 (`capSocketName`,
`pathLimit = 100`), identity.go:342 (`listBridges` globs `*.json`), amp.go:54/75/79
(`askAmp`, `rememberThread`, `askMu`), amp.go:176 (`EXECUTOR_ALREADY_CONNECTED`),
doctor.go:344 (`checkLiveSessions`), channel.go:164 (`serveAmpConn`), client.go:91
(`replyWait + 10s`), global.go:23 (`//go:embed skill.md`). All confirmed.

---

## A. Feasibility

The settled design holds against the docs and both installed example plugins. No
contradictions. Confirmations, so the code can cite them:

- long-lived plugin process — [docs L64–65]; probe: initializer runs once at load
- socket outside `session.start` — it fires on every open/switch [docs L1301–1306]
- unsteered `appendUserMessage` — [docs L1184]; steer optional [docs L1203–1210];
  `UserMessage = {type:'user-message', content}` [docs L1196–1201]; returns
  `Promise<void>` — the marker is the only pre-`agent.start` handle
- no `waitForResponse` — docs confirm the critique verbatim: "current **or next** agent
  turn… **last** assistant message" [docs L1130–1139]
- marker correlation — `AgentStartEvent {thread, message, id}` [docs L1449–1459];
  `AgentEndEvent {thread, message, id, status, messages}` where `messages` = "All messages
  since the agent.start event" [docs L1476–1491] — marker matchable in BOTH events
- commands + revocable activation — `registerCommand` [docs L144–148];
  `PluginCommandContext.thread?` [docs L1662–1683]; `CommandSubscription.setAvailability`
  with enabled/disabled/hidden [docs L1613–1617, L1649–1656]
- project install, single file, no manifest — [docs L23]; `cmux-session.ts` is one file
- executor gate — `kind: 'local'|'remote'|'unknown'` [docs L875–881, L967]
- `agent.end` returns nothing — `AgentEndResult = {action:'continue';…} | void`
  [docs L1497–1500]. Both cmux plugins return `undefined` from `agent.start` without harm;
  ours always does, never an object (an `AgentStartResult.message` would append content to
  the user's message [docs L1464–1470])
- `onDispose` — [docs L196–212]: ~3s total budget, concurrent, does NOT run on crash/SIGKILL
- runtime — Bun executes plugins [docs L66]; `node:net`/`node:fs`/`node:path` available

Taken as given (established earlier, not re-litigated here): `exec` covers imports,
registration and pure handler wiring only — thread RPCs are not testable outside a live
session; `executor.kind` is `'unknown'` under exec so the gate correctly fails closed
there; commands are Ctrl+O palette entries, not slash commands; `load_plugin` reloads one
plugin in seconds, running `onDispose` on the old instance; Claude-initiated messaging is
unproven in BOTH the held and unheld case.

### The two assumptions only a live session can settle

1. **Cross-thread append.** The request path has no event context, so appends go through
   `amp.threads.get(id)` [docs L1188–1191]. `parentThreadID`'s "Only supported for the
   plugin's current thread" caveat [docs L1120–1126] implies the other methods DO work
   cross-thread — but that is inference, not documentation. **There is no reliable fallback.**
   A cached `ctx.thread` handle is probably the same host RPC proxy and would fail for the
   same reason, so it is not a mitigation. Spike failure here is a BLOCKER requiring
   API/source investigation, not a branch to the plan-B path.

   The cached handle survives only as a **separate manual spike variant** — never an
   automatic second attempt. A rejected `appendUserMessage` promise does not prove the append
   didn't land, so retrying through another handle is the same double-delivery that E rules
   out one section later. It is run by hand, only after a human has confirmed the first
   marker never appeared in the thread, and then it does narrow the question: if the cached
   handle succeeds where `threads.get(id)` failed, the problem is cross-thread id lookup; if
   both fail, it is appending from a socket callback at all.
2. **Queued-append turn shape.** Whether a bridge message queued behind a human turn gets
   its own turn or is coalesced. Undocumented; the marker match works either way (risk 4).

Both are isolated by build step 1, before anything durable is built on them.

---

## B. Plugin design

### Files and install layout

```
amp-bridge/plugin/amp-bridge-inbox.ts      # source of truth, in this repo
amp-bridge/plugin.go                       # //go:embed plugin/amp-bridge-inbox.ts
<project>/.amp/plugins/amp-bridge-inbox.ts # installed copy — the WHOLE install; no
                                           # manifest, Amp auto-loads every *.ts there
```

This repo has no `.amp/` yet. `amp-bridge init --amp-plugin [dir]` creates
`<dir>/.amp/plugins/` (0755 — project files, not runtime secrets) and writes the file,
then prints: *"run the `load_plugin` tool or `plugins: reload` (Ctrl+O) in Amp, then
Ctrl+O → 'amp-bridge: Enable Claude inbox for this thread'"*.

For this repo, commit the installed copy and add a `make check` drift gate comparing it
byte-for-byte with the source — the convention `skill.md` already uses (global.go:20–23).

Dependencies: Amp's API plus `node:net`, `node:fs`, `node:path`, `node:crypto`. No npm
packages. `import type {…} from '@ampcode/plugin'` is type-only, erased at runtime.

### Process state — all in memory, nothing persisted

```ts
enabled   = new Map<ThreadID, true>()
perThread = new Map<ThreadID, {
  inflight: Req | null,   // Req = {id, marker, from, conn|null, timer, turnMsgId?, abandoned}
  queue: Req[]            // FIFO, cap 4
}>()
server:  net.Server | null = null   // lazy
clients: Set<net.Socket>   = new Set()   // every ACCEPTED socket — dispose must destroy them
disposed = false                         // set first in onDispose; gates all handlers
```

### Commands (Ctrl+O palette)

`enable-claude-inbox` — "Enable Claude inbox for this thread", category `amp-bridge`.
`disable-claude-inbox` — the inverse.

Conditional availability removes the invalid states entirely: keep both
`CommandSubscription`s and let one `refreshAvailability()` hide Enable when the active
thread is already enabled (and vice versa). Called from both handlers, from
`session.start`, and from a subscription to `amp.activeThread` [docs L280–291]. On a
`'remote'`/`'unknown'` executor, Enable is set to
`{type:'disabled', reason:'needs a local executor (running on: <kind>)'}` at load — visible
but unselectable, so the failure explains itself in the palette.

### Lifecycle

1. **Load** — register commands and handlers, set availability. ZERO filesystem effects. A
   repo with the plugin installed but unused leaves no runtime trace.
2. **Enable** — handler gets `ctx.thread` [docs L1682]; `undefined` → notify. **Observed
   2026-08-20:** a freshly started `amp` CLI has *no thread at all* until the user sends a
   first message, so this is the normal first-run experience, not an edge case. The message
   must therefore say what to do — "send a message in this session first, then run this
   command again" — not the bare "no active thread", which reads as a malfunction. Consider
   also setting the command's availability to `disabled` with that reason while no thread
   exists, so the palette explains itself before it is pressed. Gate on `amp.system.executor.kind === 'local'` (fail closed on both
   other values). Then, lazily on first enable:
   - ensure runtime dir + `inbox/` + `inbox/threads/` per section D (inspect-first);
   - `net.createServer(handleConn).listen(sockPath)`; on `'listening'`,
     `fs.chmodSync(sockPath, 0o600)`; on `'error'`, notify and abort the enable — never
     half-enable;
   - refuse loudly (notify with path and limit) if the socket path exceeds the 100-byte
     budget.
   Then `enabled.set(id)`, write `inbox/threads/<id>.json`, `refreshAvailability()`,
   notify.
3. **`session.start`** — `refreshAvailability()` only. There is deliberately no `seen` set:
   `session.start` has no matching close event in the documented API, so `seen` could only
   ever mean "seen since this plugin loaded", not "open now". An unsound diagnostic is worse
   than none, so the not-enabled message makes one claim instead of two.
4. **Request arrives** — one operation per connection (see C): read one bounded frame,
   answer it, close. Not enabled → immediate `code:"not-enabled"` error, never a timeout.
   Enabled → serialize: `inflight` present → enqueue (cap 4, beyond → `code:"busy"`). Each
   request's timeout timer starts AT ARRIVAL, so a queued request can expire in the queue
   saying it was queued behind another request.
5. **Append** — `amp.threads.get(threadID).appendUserMessage({type:'user-message',
   content}, undefined)` — no steer. A rejected promise → `code:"append-failed"` with the
   error text; request resolved, next dequeued.
6. **`agent.start`** — if the thread has an inflight whose marker is a substring of
   `event.message`, record `inflight.turnMsgId = event.id`. A turn starting WITHOUT our
   marker while our append is pending is the human's turn — do nothing; ours is still
   queued. Return `undefined` always.
7. **`agent.end`** — match `event.id === inflight.turnMsgId || event.message.includes(marker)`
   (the second clause covers a missed `agent.start`). On match: `status 'done'` → reply is
   the text of the **LAST** assistant message in `event.messages` (filter
   `role === 'assistant'` [docs L1032], take the last, join *its* `type:'text'` block texts
   [docs L984]). **Not** every assistant message joined: a tool-using turn contains several
   assistant completions, and joining them returns intermediate commentary ahead of the
   answer. If the last assistant message carries no text blocks (a turn ending on
   `tool_use`), respond `code:"turn-error"` — "the turn produced no final assistant text" —
   and nothing else. **Do not walk backwards** to an earlier assistant message that does have
   text: that returns pre-tool commentary, which is the exact failure last-message selection
   exists to prevent. An honest error beats a plausible wrong answer. Strip literal marker
   occurrences, respond.
   `'error'`/`'cancelled'` → `code:"turn-error"|"turn-cancelled"`. Clear inflight, dequeue.
   Handler returns nothing. Entire body in try/catch — a throw in a turn-ender must never
   surface into the turn.
8. **Timeout** (plugin-side timer) — respond `code:"timeout"`, enriched with
   `threads.get(id).state.get()` [docs L1129] raced against an explicit **500 ms** budget
   (`Promise.race`), well inside the 10 s lead over Go's read deadline, so the richer
   diagnostic can never itself lose that race. "Thread is awaiting-approval — the human must
   approve a tool call first" beats a bare deadline, but only if it arrives in time.
   **The lane is NOT released here** once the append has succeeded — see 11. Keying that on
   `turnMsgId` would leave the append-succeeded-but-`agent.start`-not-yet-fired window
   unprotected.
9. **Disable** — fail inflight + queue with `code:"disabled"`, `enabled.delete`, remove the
   thread's registry file, `refreshAvailability()`. If `enabled` is now empty: close server,
   unlink socket — fully quiescent again.
10. **`onDispose` — HOT PATH, and NOT all synchronous.** `load_plugin` runs it on every
    reload, so it executes constantly during development. `net.Server.close()` is
    **asynchronous and waits for accepted clients to disconnect** — a plan that treats it as
    a synchronous call can leave a listening socket and `EADDRINUSE` on the next load while
    claiming complete cleanup. The correct order, awaited:

    1. set `disposed = true` **first** (every handler checks it and refuses);
    2. fail all inflight and queued requests with `code:"disabled"` ("plugin
       reloading/unloading");
    3. write that frame and `end()` each socket in `clients`, allow a **small bounded flush
       grace** for it to drain, then `destroy()` — `destroy()` discards pending writes, so
       failing and destroying in one motion can swallow the `disabled` frame and hand the
       caller a bare EOF. Stated honestly: **if the grace expires the caller does see EOF and
       must treat delivery as ambiguous.** The grace makes the clean case clean; it does not
       make the dirty case impossible. Destroying after the grace is what lets close finish
       promptly rather than blocking on a peer that never hangs up;
    4. `await` the `server.close()` callback;
    5. `unlinkSync(sock)`, then remove only `threads/*.json` whose
       `plugin_pid === process.pid`;
    6. ignore ENOENT throughout; the whole sequence is idempotent.

    Well inside the 3 s budget [docs L200] *because* step 3 precedes step 4. A reload
    therefore cannot leak a listening socket or a stale registry entry.

11. **Lane ownership begins at append success and survives the caller.** The per-thread lane
    exists so that only one marked turn is ever in flight — that is what makes correlation
    unambiguous. Ownership is keyed on **the append succeeding**, NOT on `turnMsgId` being
    known: between a successful append and `agent.start` firing there is a live window in
    which the turn is about to exist but has no id yet, and releasing the lane there would let
    B append into a turn A is about to own. The lane is released by `agent.end`, not by the
    caller going away:

    - Caller socket closes *before* the append is issued → drop the request, release
      immediately.
    - **Append still pending** when the caller disconnects or times out → the append must
      **settle before any release decision is made**. A rejected promise releases; a resolved
      one retains. Deciding while it is in flight is deciding without knowing whether a turn
      exists.
    - Caller gone *after* a successful append → mark `abandoned`, keep the marker/turn
      correlation live, release only when `agent.end` matches — whether or not `agent.start`
      has fired yet.
    - Plugin timeout after a successful append → answer the caller `timeout`, but hold the
      lane: Amp's turn has not stopped just because we stopped waiting.
    - Bounded orphan grace: if no `agent.end` matches within 2× the request timeout (the Amp
      session died mid-turn), release the lane and log it. Without this cap a dead session
      wedges the thread's lane until reload.

Post-reload state is empty by design: re-enable via Ctrl+O after every `load_plugin`. There
is deliberately **no development auto-enable** — not even env-gated — because it would
weaken the explicit per-thread opt-in that is the design's main control. If the throwaway
spike needs one, it is hard-coded in the spike file and never carried into v1.

---

## C. Wire protocol

`channel.go`/`client.go` style: newline-delimited JSON over a Unix stream socket, one object
per line. Go uses `json.Encoder`/`json.Decoder` as in client.go:88–100.

**One operation per connection in v1** — one `status` or one `ask`, then the plugin responds
and closes. This is chosen because it is **auditable and simpler, not because it is logically
necessary**: the transaction boundary of E comes from the explicit status/ask phase split and
from never falling back once the `ask` write has begun, which is program state rather than
connection identity. Sequential status-then-ask on a single connection would preserve the
invariant equally well. One-op simply makes "no `ask` bytes were written" trivial to see and
to test.

It does **not** remove the need for connection hygiene. A client can connect and send neither
a newline nor EOF forever, so v1 still needs a **short pre-frame idle timeout** (a connection
that has not delivered a complete frame within it is closed) and a **connection cap**. One-op
bounds what a connection can do, not how long it can sit there doing nothing.

**The reader is bounded, with two separate limits.** A naive split-on-newline buffer grows
without limit if a client never sends `\n`. The caps are:

- **text: 64 KiB**, matching `AMP_BRIDGE_MAX_BYTES` (main.go:69, default 65536);
- **frame: 128 KiB**, because a 64 KiB text field JSON-encoded with escaping, plus
  `thread_id`, `from`, `id` and `timeout_ms`, exceeds 64 KiB. Equal caps would reject a
  legitimate maximum-size message — the frame envelope must be allowed to be larger than the
  payload it carries.

An oversized or incomplete frame closes the connection, with an error frame where one can
still be written.

**Every field is validated before use**, because any local process running as this user can
connect (see the trust note in G):

| field | rule |
|---|---|
| `op` | exactly `"ask"` or `"status"` |
| `id` | exactly 12 lowercase hex |
| `thread_id` | `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` — the same `threadIDRE` the Go side uses (amp.go:35); the leading-alnum anchor is also what makes it safe as a filename |
| `from` | printable ASCII, bounded length, sanitized — it is interpolated into a message that enters Amp's context, so it is never passed through unchecked |
| `text` | size-bounded (64 KiB frame cap) |
| `timeout_ms` | integer, bounded range, capped at 10 min |
| protocol | `proto` mismatch answered loudly, never misparsed |

Anything failing validation gets an error frame and a closed connection — never a partial
append.

**Requests (Go → plugin):**

```json
{"op":"ask","id":"9f3a2c1e8b4d","thread_id":"T-01a01877-…","text":"…","from":"amp-claude-c1","timeout_ms":290000}
{"op":"status"}
```

- `id`: 12 lowercase hex from Go `crypto/rand`. Doubles as the marker id.
- `from`: the bridge's registry name (`resolveIdentity`) — with four live sessions across
  three projects, the label must say WHICH Claude is asking.
- `timeout_ms`: Go's `ampTimeout` minus 10s, so the plugin's richer timeout diagnostic
  always wins the race against Go's bare read deadline.

**Responses (plugin → Go):**

```json
{"id":"9f3a2c1e8b4d","reply":"…"}
{"id":"9f3a2c1e8b4d","error":"…","code":"not-enabled|busy|disabled|turn-error|turn-cancelled|timeout|append-failed"}
{"pid":63274,"proto":1,"enabled_threads":["T-…"],"started_at":"2026-08-20T…"}
```

`status` is side-effect-free and serves doctor plus stale-entry verification. `proto:1`
lets a future Go side refuse an incompatible plugin loudly instead of misparsing. It carries
no "seen threads" count — see B-3 for why that number would be unsound.

Go dials with 5s (client.go:73 pattern) and sets a read deadline of `ampTimeout + 10s`
(client.go:91's "slightly past the server's own deadline" rationale). The plugin caps
`timeout_ms` at 10 minutes.

**The appended message:**

```
Claude Code session "amp-claude-c1" asks via amp-bridge [amp-bridge-req-9f3a2c1e8b4d]:

<text>

(Reply normally — your answer is returned to Claude Code automatically.)
```

The header is deliberately both the correlation token and the UX — the human reading the
thread sees exactly who is asking and can quote the id when something goes wrong. `from` and
`id` are validated per the table above before interpolation; arbitrary client strings never
reach this header unchecked.

The marker `amp-bridge-req-<12hex>` is one bare word: no whitespace (survives collapsing),
no markdown metacharacters (survives rendering), fixed prefix plus random hex (no
collision, no accidental match). In practice no normalisation applies at all —
`agent.start.message`/`agent.end.message` carry the raw prompt string [docs L1454, L1481] —
the token's shape just makes that not load-bearing. Matched by plain `String.includes`.

Stripping: the label and marker live in the USER message, not the reply, so structurally
nothing needs removal; defensively the plugin deletes literal `amp-bridge-req-<id>`
occurrences from the assistant text (models sometimes echo headers) and trims.

---

## D. Plugin-side registry

Location `<runtime>/inbox/`, where `<runtime>` = `$AMP_BRIDGE_DIR || /tmp/amp-bridge-<uid>`
— the same base `identity.go` uses (identity.go:70–75).

**The subdirectory is load-bearing:** `listBridges` globs `<runtime>/*.json`
(identity.go:342) and would parse sibling inbox files as phantom bridge sessions; glob
doesn't descend. A regression test pins this.

```
/tmp/amp-bridge-501/            drwx------ (exists now — trustedRuntimeDir predicate)
  inbox/                        0700, owned by uid
    plugin-63274.sock           0600, one per plugin process
    threads/                    0700
      T-01a01877-….json         0600, one per ENABLED thread
```

**Socket naming arithmetic** (`sockaddr_un` ~104 bytes incl NUL; budget constant 100, same
as `capSocketName`, identity.go:271):

```
/tmp/amp-bridge-501/ (20) + inbox/ (6) + plugin- (7) + pid (≤7) + .sock (5) = ≤45 bytes
```

59 bytes of headroom at the default location; a 10-digit uid adds 6. `capSocketName` is NOT
reused: it shortens a *name*, and a pid is not shortenable — and doesn't need to be.
Instead the plugin ports the constant as a guard: if `len(sockPath) > 100` (only reachable
via a long `AMP_BRIDGE_DIR`), the ENABLE fails loudly in the UI naming the path and the
limit — pre-empting the opaque bind failure `capSocketName`'s comment warns about
(identity.go:266–269). Thread registry filenames (43 chars) are regular files; no such
limit applies.

**Entry shape:**

```json
{"thread_id":"T-01a01877-…","socket":"/tmp/amp-bridge-501/inbox/plugin-63274.sock",
 "plugin_pid":63274,"proto":1,"enabled_at":"2026-08-20T…",
 "note":"amp-bridge plugin inbox; consumed by ask_amp"}
```

Reused from `identity.go`, unchanged in substance: per-uid base dir; 0700 dirs / 0600
files; inspect-before-mutate creation — lstat first, refuse symlink and foreign uid, never
mkdir-p through the final component (identity.go:81–105); read-side refusal of
symlinks/foreign-ownership/group-other access (identity.go:195–241 — the Go reader applies
the same predicate to `inbox/` and `threads/`, and **the refusal is not relaxed**:
everything the plugin writes must satisfy it, which is why the plugin's TS creation code
mirrors the discipline exactly); dial-based liveness plus dead-entry sweep
(identity.go:302, 355).

**Writes are hardened to the same standard as reads** — an asymmetry here would let the
plugin create exactly the states the Go reader is written to refuse:

- **Atomic** temp + rename, mirroring `writeFileAtomic` (init.go:142). A half-written entry
  must never be observable.
- **Filename validation**: the thread id is matched against `threadIDRE` before it is used
  as a path component. The leading-alnum anchor rules out `..` and any leading dot.
- **lstat before unlink**: sweeping a stale socket refuses anything that is not a socket, and
  refuses symlinks outright. Removing whatever happens to sit at a path is how a cleanup
  routine becomes an arbitrary-delete primitive.
- **Never unlink a socket owned by another live process.** Observed in the spike (§20,
  finding 2): two Amp sessions in one project run two plugin hosts, and the second one's
  `listen()` on a *fixed* path unlinked and stole the first's socket. Per-pid naming
  (`plugin-<pid>.sock`) makes this inherent rather than a rule to remember — which is a
  reason not to "simplify" the name later. The unlink-if-stale step still applies only to a
  path bearing *this* process's pid.
- **`Buffer.byteLength`, not `String.length`**, for the socket path budget. Go's `len()`
  counts bytes; JS `.length` counts UTF-16 code units. A non-ASCII `AMP_BRIDGE_DIR` would
  pass a naive length check and still overflow `sockaddr_un`.

**Divergences, explicit:**

1. The writer is TypeScript — `fs.lstatSync` + `stats.uid !== process.getuid()` +
   `mkdirSync({mode:0o700})`, behaviourally identical, cross-referenced by comment to
   `identity.go` since code can't be shared.
2. Entries are keyed by THREAD ID (the lookup key `askAmp` has), not session name — a
   re-enable after a crash overwrites, which IS the self-heal.
3. Two-tier staleness: dial (as `identity.go`) PLUS a `status` handshake confirming the
   thread appears in `enabled_threads` — because `onDispose` doesn't run on SIGKILL and a
   later plugin process could conceivably reuse the pid-named socket path. The handshake
   turns even that into a loud `not-enabled`, never a misdelivery.

   Crash and SIGKILL are **not** the only ways state goes stale: a tmp cleaner can unlink a
   live socket path while the listener is still running, leaving a registry entry pointing at
   nothing though the plugin is healthy. v1 explicitly accepts **re-enable as the recovery**
   rather than adding a path watchdog — the failure is loud (dial fails, entry swept, the
   error names the fix) and the extra surface is not worth it while the core mechanism is
   still unproven. Revisit if it happens in practice.

---

## E. Go-side changes

**New `amp-bridge/inbox.go`:**

```go
type inboxEntry struct { ThreadID, Socket string; PluginPID, Proto int; EnabledAt, Note string }
func trustedInboxDir() (string, error)     // trustedRuntimeDir predicate, extended to inbox/ and threads/
func lookupInbox(threadID string) (inboxEntry, bool, error)
func askViaInbox(ctx context.Context, cfg config, e inboxEntry, id, threadID, text, from string) (string, error)
```

`lookupInbox`: missing file → `(_, false, nil)`; dead socket → sweep the file, log
`INBOX_STALE`, `(_, false, nil)`; live but `status.enabled_threads` lacks the thread → treat
as absent (log `INBOX_MISMATCH`); live and confirmed → hit; untrusted dir → error
(propagates — the §17 finding-3 rule: *a refusal is not "nothing running"*).

**`askAmp` (amp.go:54)** — after validation and `rememberThread` (amp.go:75), before the CLI
machinery:

```go
entry, ok, err := lookupInbox(threadID)
if err != nil { return "", err }                 // trust refusal, never a fallback
if ok {
    b.askMu.Lock(); defer b.askMu.Unlock()       // same serialization as today
    return b.askViaInbox(entry, threadID, text)  // FINAL — no CLI fallback on error
}
// … existing CLI path, byte-for-byte unchanged  // absence degrades to exactly today
```

**The fallback boundary is a transaction invariant, not a preference.** CLI fallback is
permitted only where *no `ask` bytes could have been accepted*:

| situation | behaviour |
|---|---|
| no registry entry | fall back to CLI (today's behaviour exactly) |
| entry present, socket dead **before any write** | sweep, fall back to CLI |
| `status` says the thread is not enabled | **not-enabled**, never fallback — a disable race is a real answer, not an absence |
| trust or protocol refusal | loud error, never fallback |
| **any `ask` frame bytes may have reached the socket** | never fall back, never auto-retry; report delivery as **ambiguous** |

The CLI would hit `EXECUTOR_ALREADY_CONNECTED` anyway (the thread is by construction open),
and once the frame is out the message may already be appended — a retry risks
double-delivery, which is strictly worse than a loud error.

This is why `status` runs on **its own bounded connection first**, then `ask` on a second
one (C: one operation per connection). Probing and delivering on one connection would blur
the boundary the invariant depends on: after a combined write you can no longer prove
nothing was accepted. `askMu` stays global in v1; per-thread loosening is future work.

**`ampDiagnosis` (amp.go:156)** — extend the `EXECUTOR_ALREADY_CONNECTED` branch
(amp.go:176–184): append *"…or install the inbox plugin (`amp-bridge init --amp-plugin`)
and press Ctrl+O in that session → 'amp-bridge: Enable Claude inbox for this thread' — then
`ask_amp` reaches it directly."* When `askAmp` swept a stale entry for this very thread this
call, substitute: *"a plugin inbox for this thread existed but its socket is dead — the Amp
session likely restarted or reloaded plugins; re-enable the inbox there (Ctrl+O)."*

**`doctor` (doctor.go:55)** — new check `plugin inboxes` after `checkLiveSessions`
(doctor.go:344):

- no `inbox/` → OK, "none (no thread has enabled its Claude inbox)"
- per entry: dial + status; live and confirmed → "T-… (plugin pid N)"; dead → warn and
  sweep, "stale entry for T-… removed — re-enable in that Amp session"; live but mismatched
  → warn
- `<dir>/.amp/plugins/amp-bridge-inbox.ts` present but sha256 ≠ embedded → warn, "installed
  plugin differs from this build — `amp-bridge init --amp-plugin`, then reload it in Amp";
  absent → OK (optional component; doctor's no-false-alarms rule, doctor.go:139–144)
- Never `statusFail` — absence is a legitimate operator state.

**`init --amp-plugin [dir]`** — new arm in `parseSubcommand` (main.go:123–148, beside
`--global`): create `.amp/plugins/`, write the embed, refuse to overwrite a divergent
existing file without `--force`, print the reload-and-enable instructions.

**Failure modes → user-facing `ask_amp` results (complete):**

| Condition | What the user sees |
|---|---|
| untrusted runtime/inbox dir | the `trustedRuntimeDir` refusal verbatim (identity.go:230–241 wording) |
| stale entry swept | CLI → executor conflict → the extended diagnosis above |
| not-enabled (incl. disable race) | "thread T-… has not enabled its Claude inbox — press Ctrl+O in that Amp session and run 'amp-bridge: Enable Claude inbox for this thread'" |
| busy | "the thread's Claude inbox already has 4 requests queued — Amp has not finished the earlier turns" |
| turn-error / turn-cancelled | "Amp's turn errored / was cancelled before answering — check the thread" |
| timeout | the plugin's message including thread state when available |
| EOF mid-request | "the plugin inbox closed mid-request (the Amp session may have exited or reloaded plugins). The message may or may not have reached the thread — check it before resending" |
| proto mismatch | "the installed plugin speaks protocol vN, this bridge expects v1 — `amp-bridge init --amp-plugin` and reload it in Amp" |

---

## F. Test strategy

Three tiers, honestly separated.

**Tier 1 — Go, in `make check`** (the bulk; all `-race`, existing gofmt/vet/29-linter gates):

- `inbox.go` against a pure-Go fake plugin socket speaking protocol C (fixture style of
  `setup_test.go`/`client_test.go`): lookup hit/miss; stale sweep; status mismatch; every
  code mapping; read-deadline expiry; EOF mid-request; untrusted-dir refusals (0777 dir,
  foreign symlink — reuse `identity_test.go` fixtures); proto mismatch.
- `askAmp` routing: entry present → CLI never spawned (assert via `AMP_BIN` → fail-fast
  script); entry absent → CLI path invoked with today's argv. **The existing `amp_test.go`
  tests continuing to pass UNMODIFIED is itself the degradation guarantee.**
- `listBridges` ignores `inbox/` (the phantom-entry regression test).
- Doctor check table tests; `init --amp-plugin` (creates, embeds byte-identical, refuses
  divergent overwrite); the drift gate in `make check`.
- Golden test: the `ask` frame is one line of JSON with exactly the C fields.
- **Fallback-boundary table tests** (E), one case per row — in particular that a
  `not-enabled` status answer does NOT reach the CLI, and that a post-write failure reports
  ambiguous delivery rather than retrying.
- Frame-cap enforcement: a 64 KiB+ frame and a never-terminated line both close the
  connection instead of growing a buffer.

Pure-TS helpers that carry real logic get `bun test` coverage against fixtures — **last**
assistant-message extraction from a tool-using `AgentEndEvent`, marker matching, the lane
state machine of B-11 — soft-gated on `command -v bun`, never in the required `make check`
path.

**Tier 2 — `amp plugins exec`, wiring only** (established fact, not aspiration): the plugin
imports cleanly, registers its commands, and creates NO socket and NO file at load
(load-inertness assert: run exec, assert `inbox/` untouched). Nothing that touches a thread
RPC. **Anything beyond this via exec is explicitly not relied
on anywhere in this plan.** Optionally, exported pure helpers (marker match, reply
extraction from an `AgentEndEvent` fixture, queue state machine) get `bun test` coverage,
soft-gated on `command -v bun`, never in the required `make check` path.

**Tier 3 — live Amp session plus a human** (the honest list; everything end-to-end is here):

1. cross-thread append via `threads.get(id)` lands in the thread, labelled
2. marker appears verbatim in `agent.start.message`; `agent.end` same id; assistant text
   extractable from `messages` — **verified on a turn that uses tools**, so the last-assistant
   selection of B-7 is exercised rather than a one-line answer that cannot distinguish it
   from the naive join
3. unsteered append during a running human turn queues and gets its own turn (or is
   coalesced — either way observed and recorded)
4. palette enable/disable and conditional availability behave; re-enable after `load_plugin`
5. reload mid-request: the old instance's `onDispose` fails the inflight loudly, no
   socket/entry leak
6. background-thread turn (enabled thread not currently focused)
6b. **two Amp hosts in the same project** — both load the plugin; prove their sockets and
   thread registries stay isolated and neither unlinks the other's socket. Added at Amp's
   request after §20 finding 2 showed the spike's fixed path collided.
7. full round trip through a real Claude session's `ask_amp`
8. **separately and first:** one `ask_amp` against an UNHELD thread via today's CLI path —
   because Claude-initiated messaging is unproven in both cases, this isolates "is
   `ask_amp`'s CLI leg sound at all" from "does the plugin path work", so the first live day
   cannot surface two tangled bugs

The `load_plugin` reload loop makes tier-3 iteration seconds-per-cycle, but it remains
human-driven; the plan does not pretend otherwise. Each check is one falsifiable claim, run
one at a time with `~/.cache/amp/logs/cli.log` open (§18's lesson).

---

## G. Risks, by severity

1. **Two unproven mechanisms colliding on day one.** Neither unheld `ask_amp` (CLI leg) nor
   the append path has ever been observed succeeding. Mitigations are structural: tier-3
   check 8 tests the CLI leg alone, first; build step 1 drives the plugin socket with
   `nc`/a stub client, NOT `ask_amp`, so each mechanism is observed in isolation before they
   meet in check 7. The spike logs each stage separately so a failure names its stage.
2. **Cross-thread append doesn't work from the request path.** Invalidates the core
   mechanism; probability low (the API shape argues for it — `PluginThreads.get(threadID)`
   takes an arbitrary id, and `amp.ai` accepts a threadID outside thread-bound handlers),
   impact total. Settled by build step 1 before anything durable exists. **There is no
   mitigation** — a cached `ctx.thread` handle is probably the same host RPC proxy and would
   fail identically. If the spike fails here the plan stops and the next step is API/source
   investigation, not a plan-B branch.
3. **Ambiguous mid-flight failure → possible unanswered append.** Timeout/EOF after the
   frame was sent may leave a visible turn with no listener. Never silent: the message is
   labelled with session name and request id, and the Go error says "check the thread before
   resending". Never auto-retry after send.
4. **Human and bridge message coalesced into one turn** (undocumented). The marker substring
   still matches; the reply may answer both; the human sees everything. Observed explicitly
   in tier-3 check 3 and recorded, not assumed away.
5. **Stale socket and registry entries.** Three causes, not one: crash/SIGKILL
   (`onDispose` doesn't run — [docs L202]); a tmp cleaner unlinking a live socket path while
   the listener stays alive; and pid reuse pointing a path at a different process. Two-tier
   staleness (dial + `status` handshake) turns every variant into sweep-and-fallback or a
   loud `not-enabled`, never a misdelivery; doctor reports and sweeps. v1 accepts re-enable
   as the recovery rather than running a watchdog (D). Reload leaks are separately designed
   out by the awaited dispose sequence of B-10; tier-3 check 5 verifies.
6. **Two Claude sessions, one thread — normal here** (4 live sessions across 3 projects
   right now). Per-thread FIFO gives each its own turn; distinct `from` labels and markers
   make interleaving visible and correlation-proof. Worst case is model confusion, never
   misrouting.
7. **Phantom registry parsing** — designed out via the `inbox/` subdir; pinned by a tier-1
   test.
8. **`sockaddr_un` overflow** — impossible at the default path (45/100 bytes); a long
   `AMP_BRIDGE_DIR` gets an explicit refusal at enable time naming path and limit.
9. **Installed-plugin drift** (this project's signature failure). Doctor hash check, proto
   handshake, and the repo drift gate in `make check`.
10. **Env mismatch:** Amp launched with `AMP_BRIDGE_DIR` set differently from the Claude
    session → plugin and bridge look at different trees; manifests as not-enabled/absence,
    never corruption. One line in README and skill.md.
11. **Same-UID trust is the real boundary — state it plainly.** 0700 dirs and 0600 sockets
    keep *other users* out. They do not keep out *other processes running as this user*. Any
    such process can connect to the inbox socket and append a message into a live Amp thread,
    which is a prompt-injection path into a session that holds a local executor. The controls
    that actually matter are therefore: default-off per thread (nothing is reachable until a
    human presses Ctrl+O in that specific thread), the `local`-executor gate, strict
    validation and sanitization of every wire field before it reaches Amp's context (C), and
    the visible `from` label making the source legible in the thread. This must be said out
    loud in README and skill.md, not left implied by the file modes.

---

## H. Build order

Cut for the seconds-scale `load_plugin` cycle: more, smaller, independently verifiable
steps. Step 1 isolates the never-observed mechanism before anything is built on it.

1. **Vertical-slice spike** — the smallest plugin proving append-and-correlate live. One
   throwaway file (`.amp/plugins/amp-bridge-spike.ts`, ~120 lines): palette command "enable
   spike" → bind one fixed private socket; one enabled thread; accept one `ask`; append one
   labelled marker message via `threads.get(id)`; match `agent.start`; capture `agent.end`;
   write the assistant text back; dispose. **Explicitly NOT in the spike:** registry, doctor,
   installer, queue, lane state machine, fallback logic, validation table. Those are step 2
   and later; putting any of them here would mean debugging hardening code while the thing it
   hardens is still unproven. Driven by `printf '{"op":"ask",…}' | nc -U <sock>` — no Go
   changes, no `ask_amp`, so the plugin mechanism is observed with nothing unproven in front
   of it. Internally it logs each stage to a file — `APPEND_OK` / `START_MATCHED id=…` /
   `END_CAPTURED status=…` / `RESPONDED` — so a failure names its stage instead of
   presenting as a hung `nc`. It may hard-code an auto-enable for iteration speed; that code
   never leaves the spike file. Also run tier-3 check 8 (unheld `ask_amp`) in the same
   session, separately.

   **Exit criteria:** (a) assistant text arrives on the `nc` client; (b) it arrives correctly
   from a turn that **used tools**, proving last-assistant extraction rather than the naive
   join; (c) the failure mode of each earlier stage has been seen at least once (e.g. ask for
   a non-open thread). If the append fails, the cached-`ctx.thread` variant is run **by hand
   as a separate spike run**, and only after a human has confirmed the first marker never
   appeared in the thread — never as an automatic retry inside the same run. Everything after
   this step is hardening, not hope.
2. **Real plugin** (`amp-bridge/plugin/amp-bridge-inbox.ts`): the B state machine, C
   protocol, D registry, conditional availability, dispose. Verified live in the spike's
   session via `nc` plus the reload loop; load-inertness via exec (tier 2).
3. **`inbox.go`** plus fake-socket tier-1 tests. `make check` green.
4. **`askAmp` routing** plus regression proof that the no-inbox path is unchanged.
   `make check` green.
5. **`init --amp-plugin`** plus embed, drift gate, tests. Commit the installed copy.
6. **Doctor inbox check** plus the `ampDiagnosis` extension and tests.
7. **Tier-3 checklist end to end** (all 8 items) with a real Claude session; record
   outcomes — including the coalescing answer from check 3 — as §20 of the main research
   doc.
8. **Docs:** skill.md (new error texts, the Ctrl+O enable instruction), AGENTS.md,
   README.md. `make check` gates the skill/plugin sync.

Steps 3–6 are individually `make check`-verifiable with no live session; steps 1, 2 and 7
are where the human is genuinely required, concentrated at the start (prove) and the end
(accept) rather than scattered through the middle.

---

## Review record

Reviewed by the Amp development thread on 2026-08-20, over the bridge itself. All eight
blocking corrections accepted and folded into the sections above; two refinements added on
top. Summary of what changed and why, so the reasoning is not lost:

| # | Correction | Where it landed |
|---|---|---|
| A | `net.Server.close()` is async and waits for accepted clients — "all synchronous" was wrong and would leave `EADDRINUSE` on reload | B-10, rewritten as an awaited six-step sequence with client tracking |
| B | Reply extraction must take the **last** assistant message, not join all of them — tool-using turns have several completions | B-7, plus a no-text fallback and a tier-3 check on a tool-using turn |
| C | Unbounded newline reader; missing field validation | C, rewritten: one op per connection, 64 KiB cap, full validation table |
| D | Lane must not be released when the caller vanishes after a successful append | B-11, new: lane released by `agent.end`, with a bounded orphan grace |
| E | Registry writes needed read-level hardening; `Buffer.byteLength` not `String.length` | D, new write-hardening block |
| F | `seen` means "since load", not "open now" — no close event exists | B-3, `seen` removed entirely rather than reworded |
| G | `state.get()` enrichment could itself lose the race it exists to win | B-8, explicit 500 ms `Promise.race` budget |
| H | Keep the spike genuinely minimal; prove last-assistant on a tool-using turn | H-1, explicit exclusion list and four exit criteria |

**Answers to the four questions carried into review:**

1. **Cross-thread append** — use `threads.get(id)` in the spike. A cached `ctx.thread` is
   *not* a fallback (same host RPC proxy, same likely failure). Spike failure is a blocker
   requiring API/source investigation. Recorded in A and risk 2.
2. **Fallback boundary** — the no-fallback-after-send rule is right, and is now stated as a
   transaction invariant with a case table: fallback only where no `ask` bytes could have
   been accepted. Recorded in E.
3. **Visible marker** — keep it; no hidden metadata or append-returned id exists, and human
   input can race the append. Make the header the UX as well as the token, with `from` and
   `id` validated before interpolation. Recorded in C.
4. **Development auto-enable** — **do not build it.** The manual Ctrl+O protects the explicit
   opt-in invariant, which is the design's main control given same-UID trust. Hard-code it in
   the throwaway spike if needed; never carry it into v1. Recorded in B-11 and H-1. *(This
   was my own open question; the answer is the conservative one.)*

### Round two

Seven follow-up corrections after Amp read rev 2. All accepted. Three of them overturn things
this side contributed, which is worth recording rather than smoothing over.

| # | Correction | Where it landed |
|---|---|---|
| 1 | **Veto the automatic cached-`ctx.thread` attempt.** A rejected `appendUserMessage` no more proves non-delivery than a failed socket write does — an automatic second attempt is the same double-delivery E rules out. This side had proposed it and was inconsistent with its own invariant. | A, H-1: manual variant only, after a human confirms the marker never landed |
| 2 | **"Load-bearing" was an overclaim.** The transaction boundary comes from the status/ask phase split and never-fallback-once-writing — program state, not connection identity. Sequential status-then-ask on one connection would preserve it too. | C: one-op is auditable and simpler, *not* logically necessary |
| 3 | **One-op still needs connection hygiene.** A client can connect and send neither newline nor EOF forever. rev 2 wrongly said one-op removed idle timeouts and caps "entirely". | C: short pre-frame idle timeout plus connection cap |
| 4 | **Frame and text caps must differ.** 64 KiB of text, JSON-encoded with escaping plus the other fields, exceeds 64 KiB — equal caps reject a legitimate maximum message. | C: 128 KiB frame, 64 KiB text |
| 5 | **Lane ownership begins at append success, not at `turnMsgId`.** rev 2 left a live window between a successful append and `agent.start` in which the lane would be released and B could append into A's imminent turn. A pending append must settle before any release decision. | B-11, rewritten |
| 6 | **No walk-back on a text-less final assistant message.** This side's "empty-reply guard" reintroduced exactly the bug correction B fixed: walking back returns pre-tool commentary. | B-7: explicit `turn-error`, nothing else |
| 7 | **`destroy()` discards pending writes**, so fail-then-destroy can swallow the `disabled` frame and hand the caller a bare EOF. | B-10: bounded flush grace after `end(frame)`, plus an honest statement that an expired grace means EOF and ambiguous delivery |

Of the two refinements this side added in round one, one was vetoed outright (1) and one was
half wrong (6) — the explicit-error half was right, the walk-back half reintroduced the bug it
was meant to guard. Both are recorded as corrected rather than quietly dropped.

**Deferred, not blocking:** putting the request deadline in the channel event metadata so an
answering session can see its budget. That concerns the existing bridge, not the plugin —
candidate §20, see the companion response file.

**Build order is unchanged.** Every correction is a spec detail inside a step, not a
resequencing — the spike still comes first and still gates everything else. The one schedule
effect is that step 2 is larger than drafted (validation table, lane state machine, awaited
dispose), which is a good trade for the defects it removes.

**Non-blocking notes accepted as-is:** unsteered append and no `waitForResponse` in v1; global
`askMu` acceptable in v1 despite per-thread plugin concurrency; per-thread registry plus one
shared plugin socket is the right multi-thread shape; same-UID trust and prompt-injection
exposure must be explicit in the docs (now risk 11).
