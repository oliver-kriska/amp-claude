/**
 * amp-bridge inbox — lets a Claude Code session reach THIS Amp thread.
 *
 * Amp permits one executor per thread. An interactive session holds it, so
 * `amp threads continue --execute` cannot attach to the very thread you are
 * sitting in — which is the thread a paired Claude session most wants to ask.
 * A plugin loaded inside that session can append to it, so this opens a small
 * Unix socket that the Go bridge delivers questions through, and hands Amp's
 * answer back.
 *
 * Nothing happens until you ask for it. Loading this file binds no socket,
 * writes no file and touches no directory. Palette commands opt in either the
 * active thread or a named managed thread, and disabling revokes that consent.
 *
 *   Ctrl+O → "amp-bridge: Enable Claude inbox for this thread"
 *
 * Trust boundary, stated plainly: 0700 directories and a 0600 socket keep other
 * *users* out. They do not keep out other *processes running as you*. Any such
 * process can append a message into an enabled thread. The controls that matter
 * are therefore the explicit per-thread opt-in, the local-executor gate, and the
 * validation every wire field passes before it reaches Amp's context.
 */

import type {
  PluginAPI,
  AgentStartEvent,
  AgentEndEvent,
  SessionStartEvent,
  PluginCommandContext,
  CommandSubscription,
} from '@ampcode/plugin'
import * as net from 'node:net'
import * as fs from 'node:fs'
import * as path from 'node:path'
import { execFileSync } from 'node:child_process'

const PROTO = 1

// The envelope is allowed to be larger than the payload it carries: 64 KiB of
// text, JSON-encoded with escaping plus the other fields, exceeds 64 KiB, and
// equal caps would reject a legitimate maximum-size message.
const FRAME_MAX = 128 * 1024
const TEXT_MAX = 64 * 1024

const IDLE_MS = 10_000 // a connection that sends no complete frame is closed
const MAX_CONNS = 32
const QUEUE_MAX = 4
// Ceiling on how long after a successful append we wait for Amp to start a turn
// before checking whether one is ever going to. Generous against the observed
// healthy case, where agent.start followed the append in about a second.
const NO_TURN_GRACE_MS = 20_000
const DEFAULT_TIMEOUT_MS = 240_000
const MAX_TIMEOUT_MS = 600_000
const STATE_PROBE_MS = 500 // bounded, so enrichment cannot lose the race it exists to win
const FLUSH_GRACE_MS = 200 // destroy() discards pending writes; give end() a moment
const ORPHAN_FACTOR = 2 // release a lane whose turn never ends after 2x the budget

// sockaddr_un is ~104 bytes including the NUL. Same constant as capSocketName
// in identity.go; bind failure at the limit is opaque, so refuse early and say why.
const SOCKET_PATH_MAX = 100

// Identical to threadIDRE in amp.go:35. The leading-alnum anchor is also what
// makes a thread id safe as a path component: it rules out ".." and leading dots.
const THREAD_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/
const REQ_ID_RE = /^[0-9a-f]{12}$/
const FROM_MAX = 64

type ReqState = {
  id: string
  marker: string
  from: string
  threadID: string
  conn: net.Socket | null // null once the caller has gone away
  timer: ReturnType<typeof setTimeout> | null
  orphan: ReturnType<typeof setTimeout> | null
  noTurn: ReturnType<typeof setTimeout> | null
  turnMsgId: string | number | null
  appended: boolean // lane ownership begins HERE, not at agent.start
  settled: boolean
  timeoutMs: number
}

type Lane = { inflight: ReqState | null; queue: ReqState[] }

// Guards against the same plugin being loaded twice into one Amp process, which
// happens the moment a project has a committed copy under .amp/plugins/ AND the
// user installs it globally. Both copies would compute the same
// plugin-<pid>.sock — identical pid, identical path — and the second would
// unlink the first's socket, exactly the collision two Amp hosts hit with a
// fixed socket name. First load wins; the second stays completely inert rather
// than registering duplicate palette commands or stealing the socket.
const LOAD_GUARD = '__ampBridgeInboxLoaded'

export default function (amp: PluginAPI) {
  const g = globalThis as unknown as Record<string, unknown>
  if (g[LOAD_GUARD]) {
    // Nothing registered, nothing bound, nothing written. Silent by design: a
    // duplicate load is a normal consequence of installing globally in a repo
    // that also ships a copy, not an error the user needs to act on.
    return
  }
  g[LOAD_GUARD] = true

  const uid = typeof process.getuid === 'function' ? process.getuid() : 0
  const runtimeDir = process.env.AMP_BRIDGE_DIR || `/tmp/amp-bridge-${uid}`
  const inboxDir = path.join(runtimeDir, 'inbox')
  const threadsDir = path.join(inboxDir, 'threads')
  // Intent is kept apart from registration on purpose. `threads/` is the live
  // registration the Go side reads and is entitled to sweep when the socket
  // behind it is gone; `intent/` is the user's decision, which nothing outside
  // this plugin touches. Keeping them in one file meant a sweep during the
  // reload window — exactly when the socket is legitimately absent — would
  // destroy the choice we are trying to restore.
  const intentDir = path.join(inboxDir, 'intent')
  const sockPath = path.join(inboxDir, `plugin-${process.pid}.sock`)
  const logPath = path.join(inboxDir, 'plugin.log')

  // The plugin WORKER is replaced on every reload; its parent Amp host survives.
  // Host identity lets a reload restore every inbox that host already served.
  // Consent itself belongs to the THREAD, though: a later Amp host may claim it
  // only after activeThread/session.start proves that exact thread opened there.
  const AMP_HOST_PID = process.ppid
  const AMP_HOST_STARTED_AT = processStartedAt(AMP_HOST_PID)

  // Live registrations belong to this worker's socket, unlike durable intent.
  const PROCESS_STARTED_AT = Math.round(Date.now() - process.uptime() * 1000)

  const enabled = new Map<string, true>()
  const controllers = new Map<string, string>()
  const lanes = new Map<string, Lane>()
  const clients = new Set<net.Socket>()
  let server: net.Server | null = null
  let disposed = false

  let enableCmd: CommandSubscription | null = null
  let disableCmd: CommandSubscription | null = null
  let enableManagedCmd: CommandSubscription | null = null
  let disableManagedCmd: CommandSubscription | null = null

  // ---- diagnostics -------------------------------------------------------
  // Only ever writes after the directory exists, i.e. after an explicit enable.
  // Logging at load would break the inertness the design depends on.
  function log(msg: string): void {
    if (!fs.existsSync(inboxDir)) return
    try {
      // Every Amp session on this machine appends to this one file, so a line
      // without a pid cannot be attributed to a session. That cost real time
      // during the first field diagnosis: three DISPOSE lines that could have
      // belonged to any of four running Amps.
      fs.appendFileSync(logPath, `${new Date().toISOString()} [${process.pid}] ${msg}\n`, {
        mode: 0o600,
      })
    } catch {
      /* diagnostics must never be the thing that breaks delivery */
    }
  }

  // ---- filesystem, mirroring identity.go's discipline --------------------
  // The Go reader applies ownRuntimeDir/trustedRuntimeDir to everything below,
  // and that refusal is not relaxed to accommodate us — so create exactly what
  // it will accept. Inspect before mutating: /tmp is world-writable, and both
  // mkdir and chmod follow symlinks, so checking afterwards is already too late.
  function ensureDir(dir: string): void {
    let st: fs.Stats | null = null
    try {
      st = fs.lstatSync(dir)
    } catch {
      fs.mkdirSync(dir, { recursive: true, mode: 0o700 })
      return
    }
    if (st.isSymbolicLink()) throw new Error(`${dir} is a symlink — refusing to use it`)
    if (!st.isDirectory()) throw new Error(`${dir} exists but is not a directory`)
    if (st.uid !== uid) throw new Error(`${dir} is owned by uid ${st.uid}, not ${uid}`)
    if (st.mode & 0o077) fs.chmodSync(dir, 0o700)
  }

  function writeFileAtomic(file: string, data: string): void {
    const tmp = `${file}.tmp-${process.pid}`
    fs.writeFileSync(tmp, data, { mode: 0o600 })
    fs.renameSync(tmp, file) // a half-written entry must never be observable
  }

  function entryPath(threadID: string): string {
    // Validated by the caller; re-asserted because this builds a path.
    if (!THREAD_ID_RE.test(threadID)) throw new Error(`implausible thread id ${threadID}`)
    return path.join(threadsDir, `${threadID}.json`)
  }

  function writeEntry(threadID: string): void {
    writeFileAtomic(
      entryPath(threadID),
      JSON.stringify(
        {
          thread_id: threadID,
          socket: sockPath,
          plugin_pid: process.pid,
          plugin_started_at: PROCESS_STARTED_AT,
          proto: PROTO,
          enabled_at: new Date().toISOString(),
          note: 'amp-bridge plugin inbox; consumed by ask_amp',
        },
        null,
        2,
      ),
    )
  }

  function removeEntry(threadID: string): void {
    try {
      fs.unlinkSync(entryPath(threadID))
    } catch {
      /* already gone */
    }
  }

  function intentPath(threadID: string): string {
    if (!THREAD_ID_RE.test(threadID)) throw new Error(`implausible thread id ${threadID}`)
    return path.join(intentDir, `${threadID}.json`)
  }

  // Survives worker and Amp-host restarts. Removed only when the user explicitly
  // disables the thread or the runtime directory itself is cleaned.
  function writeIntent(
    threadID: string,
    enabledAt?: string,
    controllerThreadID?: string,
  ): void {
    ensureDir(intentDir)
    writeFileAtomic(
      intentPath(threadID),
      JSON.stringify(
        {
          thread_id: threadID,
          amp_pid: AMP_HOST_PID,
          amp_started_at: AMP_HOST_STARTED_AT,
          enabled_at: enabledAt || new Date().toISOString(),
          ...(controllerThreadID ? { controller_thread_id: controllerThreadID } : {}),
          note:
            'amp-bridge plugin inbox; the user enabled this thread until explicitly disabled. Not read by ask_amp.',
        },
        null,
        2,
      ),
    )
  }

  function removeIntent(threadID: string): void {
    try {
      fs.unlinkSync(intentPath(threadID))
    } catch {
      /* already gone */
    }
  }

  function threadIDFromInput(input: string): string | null {
    const value = input.trim()
    if (!value) return null

    let candidate = value
    try {
      const url = new URL(value)
      candidate = url.pathname.split('/').filter(Boolean).at(-1) || ''
    } catch {
      // A bare thread id is the normal input; URL parsing is only convenience.
    }
    return candidate.startsWith('T-') && THREAD_ID_RE.test(candidate) ? candidate : null
  }

  function processStartedAt(pid: number): string | null {
    try {
      const out = execFileSync('ps', ['-o', 'lstart=', '-p', String(pid)], {
        encoding: 'utf8',
        env: { ...process.env, LC_ALL: 'C' },
        timeout: 1_000,
      }).trim()
      return out || null
    } catch {
      return null
    }
  }

  // Only ever removes a path that is genuinely a stale socket, and only one
  // bearing this process's pid. Unlinking whatever happens to sit at a path is
  // how a cleanup routine becomes an arbitrary-delete primitive — and a fixed
  // socket name is how two plugin hosts in one project stole each other's
  // socket during the spike.
  function unlinkOwnSocket(): void {
    try {
      if (fs.lstatSync(sockPath).isSocket()) fs.unlinkSync(sockPath)
    } catch {
      /* nothing there */
    }
  }

  // ---- lanes -------------------------------------------------------------
  function lane(threadID: string): Lane {
    let l = lanes.get(threadID)
    if (!l) {
      l = { inflight: null, queue: [] }
      lanes.set(threadID, l)
    }
    return l
  }

  function respond(req: ReqState, obj: Record<string, unknown>): void {
    if (req.settled) return
    req.settled = true
    if (req.timer) clearTimeout(req.timer)
    req.timer = null
    if (req.noTurn) clearTimeout(req.noTurn)
    req.noTurn = null
    const conn = req.conn
    req.conn = null
    if (!conn) return
    try {
      conn.end(JSON.stringify({ id: req.id, ...obj }) + '\n')
    } catch {
      /* caller vanished; the log still records what we would have said */
    }
  }

  // Releasing the lane is what allows the next request to append. It must never
  // happen while a turn we appended is still running, or the next marker would
  // land inside somebody else's turn and correlation would be a guess.
  function releaseLane(threadID: string, req: ReqState): void {
    const l = lanes.get(threadID)
    if (!l || l.inflight !== req) return
    if (req.orphan) clearTimeout(req.orphan)
    req.orphan = null
    if (req.noTurn) clearTimeout(req.noTurn)
    req.noTurn = null
    l.inflight = null
    const next = l.queue.shift()
    if (next) void startRequest(threadID, next)
  }

  async function startRequest(threadID: string, req: ReqState): Promise<void> {
    const l = lane(threadID)
    l.inflight = req

    const content =
      `Claude Code session "${req.from}" asks via amp-bridge [${req.marker}]:\n\n` +
      `${textOf(req)}\n\n` +
      `(Reply normally — your answer is returned to Claude Code automatically.)`

    log(`APPEND_TRY req=${req.id} thread=${threadID} from=${req.from}`)
    try {
      await amp.threads.get(threadID as never).appendUserMessage(
        {
          type: 'user-message',
          content,
        },
        // The caller is blocked on this with a deadline, so being queued behind
        // whatever else the thread is doing means timing out with the question
        // still unread. steer asks Amp to prefer it. Without this, a request
        // sent while the thread was mid-turn sat unanswered until it expired.
        { steer: true },
      )
    } catch (err) {
      // A rejected append does not prove nothing landed, but it is the only
      // signal available, and there is deliberately no second attempt through
      // another handle: retrying is how one question becomes two messages.
      log(`APPEND_FAIL req=${req.id} thread=${threadID} err=${String(err)}`)
      respond(req, { error: `append failed: ${String(err)}`, code: 'append-failed' })
      releaseLane(threadID, req)
      return
    }

    // Lane ownership begins here. Between this point and agent.start there is a
    // window where the turn is about to exist but has no id yet; releasing the
    // lane in that window would let the next request append into it.
    req.appended = true
    log(`APPEND_OK req=${req.id} thread=${threadID}`)
    req.orphan = setTimeout(() => {
      log(`ORPHAN req=${req.id} thread=${threadID} — no agent.end within ${ORPHAN_FACTOR}x budget`)
      releaseLane(threadID, req)
    }, req.timeoutMs * ORPHAN_FACTOR)

    // Appending a message is not the same as starting a turn, and discovering
    // the difference by waiting out the whole timeout tells the caller nothing
    // about what went wrong. See checkTurnStarted.
    req.noTurn = setTimeout(() => void checkTurnStarted(threadID, req), noTurnDelay(req))
  }

  // A caller who allowed two seconds should not wait twenty to be told nothing
  // started. Scale with their deadline and cap at the ceiling, so a short
  // request fails fast and a long one still gets a generous grace.
  function noTurnDelay(req: ReqState): number {
    return Math.min(NO_TURN_GRACE_MS, Math.max(250, Math.floor(req.timeoutMs / 4)))
  }

  // Did Amp actually start a turn for the message we appended?
  //
  // appendUserMessage resolves when the message is in the thread. Whether a
  // turn follows is Amp's decision, and in the field it sometimes does not:
  // the message lands, nothing runs, and the caller waits out the full timeout
  // for an answer that was never being written. That failure used to be
  // indistinguishable from a slow turn.
  //
  // The thread's own state separates them. Something running means our request
  // is plausibly queued behind it, which is legitimate — keep waiting, the
  // request timeout still bounds us. Nothing running, with no turn of ours
  // started, means nothing is ever going to happen, and the honest answer is to
  // say so now and name the consequence: the message IS in the thread.
  async function checkTurnStarted(threadID: string, req: ReqState): Promise<void> {
    if (disposed || req.settled || req.turnMsgId !== null) return

    let state: unknown = null
    try {
      state = await Promise.race([
        amp.threads.get(threadID as never).state.get(),
        new Promise((resolve) => setTimeout(() => resolve(null), STATE_PROBE_MS)),
      ])
    } catch {
      /* the probe is a diagnostic; its failure must not settle the request */
    }
    if (disposed || req.settled || req.turnMsgId !== null) return

    // Only states that prove the thread cannot start our turn are stalled.
    // `awaiting-approval` is busy just like `running`, and an unknown future
    // state must stay on the safe side too: a failed probe is not evidence of a
    // stall, and guessing wrong here would abort a turn that is perfectly well.
    const stateName = String(state)
    const stalled = state !== null && (stateName === 'idle' || stateName === 'error')
    if (!stalled) {
      req.noTurn = setTimeout(() => void checkTurnStarted(threadID, req), noTurnDelay(req))
      return
    }

    log(`NO_TURN req=${req.id} thread=${threadID} state=${String(state)}`)
    const controllerThreadID = controllers.get(threadID)
    if (controllerThreadID) {
      void amp.ui
        .notify(
          `amp-bridge: Claude queued a request for managed thread ${threadID}, but Amp did not start a turn. ` +
            `Continue that thread to process it; do not ask Claude to resend.`,
        )
        .catch((err) => log(`CONTROLLER_NOTIFY_FAIL thread=${threadID} ${String(err)}`))
    }
    respond(req, {
      error:
        `the message was appended to thread ${threadID} but Amp did not start a turn ` +
        `for it (thread state: ${String(state)}). It is queued in the thread but unanswered ` +
        `for now; the next activity in that thread may pick it up. Ask your user to continue there`,
      code: 'no-turn',
    })
    releaseLane(threadID, req)
  }

  // The text is carried on the request object rather than closed over, so the
  // append content is built from validated data in one place.
  const textByReq = new WeakMap<ReqState, string>()
  function textOf(req: ReqState): string {
    return textByReq.get(req) ?? ''
  }

  // ---- agent lifecycle ---------------------------------------------------
  amp.on('agent.start', (event: AgentStartEvent) => {
    if (disposed) return
    const l = lanes.get(event.thread.id as unknown as string)
    const req = l?.inflight
    if (!req || !req.appended) return
    if (!event.message.includes(req.marker)) {
      // Somebody typed while ours was queued. Not our turn; ours is still coming.
      log(`START_OTHER id=${String(event.id)} (human turn; req=${req.id} still pending)`)
      return
    }
    req.turnMsgId = event.id
    if (req.noTurn) clearTimeout(req.noTurn)
    req.noTurn = null
    log(`START_MATCHED id=${String(event.id)} req=${req.id}`)
    // Returning undefined rather than {}: an AgentStartResult with a `message`
    // would append content to the user's turn, and undefined is the shape both
    // shipped example plugins use and the runtime is known to accept. The cast
    // is because AgentStartResult has no `| void` arm, not because we want a value.
    return undefined as unknown as ReturnType<typeof Object>
  })

  amp.on('agent.end', (event: AgentEndEvent) => {
    if (disposed) return
    const threadID = event.thread.id as unknown as string
    const l = lanes.get(threadID)
    const req = l?.inflight
    if (!req || !req.appended) return

    const messages = event.messages || []
    const markerIndex = messages.findIndex(
      (message: { role: string; content?: { type: string; text?: string }[] }) =>
        message.role === 'user' &&
        (message.content || []).some(
          (block) =>
            block.type === 'text' &&
            typeof block.text === 'string' &&
            block.text.includes(req.marker),
        ),
    )
    const matched =
      (req.turnMsgId !== null && event.id === req.turnMsgId) ||
      event.message.includes(req.marker) ||
      markerIndex >= 0
    if (!matched) return

    try {
      // `steer: true` can inject the marked message into a turn that started
      // before this request existed. That path has no second agent.start event,
      // so agent.end's messages are the first place correlation is observable.
      // Ignore assistants from before the marker or their pre-steer commentary
      // could be mistaken for the answer.
      const relevantMessages = markerIndex >= 0 ? messages.slice(markerIndex + 1) : messages
      const assistants = relevantMessages.filter((m: { role: string }) => m.role === 'assistant')
      if (markerIndex >= 0 && req.turnMsgId === null) {
        if (req.noTurn) clearTimeout(req.noTurn)
        req.noTurn = null
        log(`END_MATCHED_STEER id=${String(event.id)} req=${req.id} marker_index=${markerIndex}`)
      }
      log(
        `END_CAPTURED id=${String(event.id)} req=${req.id} status=${event.status} ` +
          `messages=${messages.length} assistants=${assistants.length}`,
      )

      if (event.status !== 'done') {
        respond(req, { error: `turn ${event.status}`, code: `turn-${event.status}` })
        return
      }

      // The LAST assistant message, and only that one. A tool-using turn holds
      // several assistant completions; joining them returns the intermediate
      // commentary ahead of the answer — and a one-line reply cannot tell the
      // two implementations apart, which is why this is proved on a tool turn.
      const last = assistants[assistants.length - 1] as { content?: { type: string; text?: string }[] } | undefined
      const texts = (last?.content || [])
        .filter((b) => b.type === 'text' && typeof b.text === 'string')
        .map((b) => b.text as string)

      if (texts.length === 0) {
        // Deliberately no walk-back to an earlier assistant message: its text IS
        // the pre-tool commentary this selection exists to avoid. An honest
        // error beats a plausible wrong answer.
        log(`END_NO_TEXT req=${req.id} assistants=${assistants.length}`)
        respond(req, {
          error: 'the turn produced no final assistant text',
          code: 'turn-error',
        })
        return
      }

      const reply = texts.join('\n').split(req.marker).join('').trim()
      log(`RESPONDED req=${req.id} bytes=${Buffer.byteLength(reply)}`)
      respond(req, { reply })
    } catch (err) {
      // A throw inside a turn-ending handler must never surface into the turn.
      log(`END_HANDLER_ERROR req=${req.id} ${String(err)}`)
      respond(req, { error: `plugin error handling the reply: ${String(err)}`, code: 'turn-error' })
    } finally {
      releaseLane(threadID, req)
    }
  })

  // ---- wire protocol -----------------------------------------------------
  function validateAsk(frame: Record<string, unknown>): string | null {
    if (typeof frame.id !== 'string' || !REQ_ID_RE.test(frame.id)) {
      return 'id must be 12 lowercase hex characters'
    }
    if (typeof frame.thread_id !== 'string' || !THREAD_ID_RE.test(frame.thread_id)) {
      return 'thread_id is not a plausible Amp thread id'
    }
    if (typeof frame.text !== 'string' || frame.text.length === 0) {
      return 'text must be a non-empty string'
    }
    if (Buffer.byteLength(frame.text) > TEXT_MAX) {
      return `text exceeds ${TEXT_MAX} bytes`
    }
    if (frame.from !== undefined && typeof frame.from !== 'string') {
      return 'from must be a string'
    }
    if (frame.timeout_ms !== undefined && !Number.isInteger(frame.timeout_ms)) {
      return 'timeout_ms must be an integer'
    }
    return null
  }

  // `from` is interpolated into a message that enters Amp's context, so it is
  // sanitized rather than trusted: printable ASCII, bounded, no newlines.
  function sanitizeFrom(raw: unknown): string {
    const s = typeof raw === 'string' ? raw : ''
    // eslint-disable-next-line no-control-regex
    const clean = s.replace(/[^\x20-\x7E]/g, '').slice(0, FROM_MAX)
    return clean || 'a Claude Code session'
  }

  function handleAsk(frame: Record<string, unknown>, conn: net.Socket): void {
    const bad = validateAsk(frame)
    if (bad) {
      conn.end(JSON.stringify({ error: bad, code: 'invalid' }) + '\n')
      return
    }
    const threadID = frame.thread_id as string
    const id = frame.id as string

    if (!enabled.has(threadID)) {
      log(`REJECT not-enabled thread=${threadID}`)
      conn.end(
        JSON.stringify({
          id,
          error:
            `thread ${threadID} has not enabled its Claude inbox — press Ctrl+O in that ` +
            `Amp session and run 'amp-bridge: Enable Claude inbox for this thread', or from ` +
            `another local Amp session run 'amp-bridge: Enable Claude inbox for another thread'`,
          code: 'not-enabled',
        }) + '\n',
      )
      return
    }

    const timeoutMs = Math.min(
      Math.max(Number(frame.timeout_ms) || DEFAULT_TIMEOUT_MS, 1_000),
      MAX_TIMEOUT_MS,
    )
    const req: ReqState = {
      id,
      marker: `amp-bridge-req-${id}`,
      from: sanitizeFrom(frame.from),
      threadID,
      conn,
      timer: null,
      orphan: null,
      noTurn: null,
      turnMsgId: null,
      appended: false,
      settled: false,
      timeoutMs,
    }
    textByReq.set(req, frame.text as string)

    // The caller going away does not release the lane once we have appended:
    // Amp's turn has not stopped just because nobody is listening any more.
    conn.on('close', () => {
      if (req.conn === conn) {
        req.conn = null
        if (!req.appended && !req.settled) {
          req.settled = true
          if (req.timer) clearTimeout(req.timer)
          const l = lanes.get(threadID)
          if (l) {
            const i = l.queue.indexOf(req)
            if (i >= 0) l.queue.splice(i, 1)
          }
        }
      }
    })

    // Started at arrival, so a request that expires while queued says so.
    req.timer = setTimeout(() => void expire(req), timeoutMs)

    const l = lane(threadID)
    if (l.inflight) {
      if (l.queue.length >= QUEUE_MAX) {
        respond(req, {
          error: `the Claude inbox for ${threadID} already has ${QUEUE_MAX} requests queued`,
          code: 'busy',
        })
        return
      }
      l.queue.push(req)
      log(`QUEUED req=${id} thread=${threadID} depth=${l.queue.length}`)
      return
    }
    void startRequest(threadID, req)
  }

  async function expire(req: ReqState): Promise<void> {
    if (req.settled) return
    // "Awaiting approval — a human must approve a tool call" beats a bare
    // deadline, but only if it arrives in time, hence the hard race budget.
    let detail = ''
    try {
      const state = await Promise.race([
        amp.threads.get(req.threadID as never).state.get(),
        new Promise((resolve) => setTimeout(() => resolve(null), STATE_PROBE_MS)),
      ])
      if (state) detail = ` (thread state: ${String(state)})`
    } catch {
      /* enrichment is a bonus, never a dependency */
    }
    const where = req.appended ? 'the Amp turn did not finish in time' : 'queued behind another bridge request'
    log(`TIMEOUT req=${req.id} thread=${req.threadID} appended=${req.appended}`)
    // `delivered` is the same fact as `appended`, but stated where the bridge
    // can read it. Both cases share code 'timeout' and want opposite responses
    // — resend freely versus never resend — so leaving the distinction in prose
    // meant the caller had to guess, and a wrong guess duplicates a message.
    // A field rather than a new code, so an older bridge simply ignores it.
    respond(req, { error: `${where}${detail}`, code: 'timeout', delivered: req.appended })

    // If we never appended, nothing owns a turn and the lane can move on. If we
    // did, the turn is still running: agent.end or the orphan timer releases it.
    if (!req.appended) releaseLane(req.threadID, req)
  }

  function handleConn(conn: net.Socket): void {
    if (disposed || clients.size >= MAX_CONNS) {
      conn.destroy()
      return
    }
    clients.add(conn)
    conn.on('close', () => clients.delete(conn))
    conn.on('error', () => {})
    conn.setEncoding('utf8')

    let buf = ''
    let handled = false
    const idle = setTimeout(() => {
      if (!handled) {
        log('IDLE_CLOSE (no complete frame)')
        try {
          conn.destroy()
        } catch {
          /* already gone */
        }
      }
    }, IDLE_MS)
    conn.on('close', () => clearTimeout(idle))

    conn.on('data', (chunk: string) => {
      if (handled) return
      buf += chunk
      if (Buffer.byteLength(buf) > FRAME_MAX) {
        handled = true
        clearTimeout(idle)
        log('FRAME_TOO_LARGE')
        conn.end(JSON.stringify({ error: `frame exceeds ${FRAME_MAX} bytes` }) + '\n')
        return
      }
      const nl = buf.indexOf('\n')
      if (nl < 0) return
      handled = true
      clearTimeout(idle)
      const line = buf.slice(0, nl)
      buf = ''

      let frame: Record<string, unknown>
      try {
        frame = JSON.parse(line)
      } catch {
        conn.end(JSON.stringify({ error: 'malformed json' }) + '\n')
        return
      }
      // One operation per connection: it makes "no ask bytes were written"
      // trivial to see, which is what the Go side's fallback rule turns on.
      if (frame?.op === 'status') {
        conn.end(
          JSON.stringify({
            pid: process.pid,
            proto: PROTO,
            enabled_threads: [...enabled.keys()],
            started_at: startedAt,
          }) + '\n',
        )
        return
      }
      if (frame?.op !== 'ask') {
        conn.end(JSON.stringify({ error: `unknown op: ${String(frame?.op)}` }) + '\n')
        return
      }
      handleAsk(frame, conn)
    })
  }

  const startedAt = new Date().toISOString()

  function startServer(): Promise<void> {
    if (Buffer.byteLength(sockPath) > SOCKET_PATH_MAX) {
      // Byte length, not string length: Go counts bytes, and a non-ASCII
      // AMP_BRIDGE_DIR would pass a naive check and still overflow sockaddr_un.
      return Promise.reject(
        new Error(
          `socket path ${sockPath} is ${Buffer.byteLength(sockPath)} bytes, over the ` +
            `${SOCKET_PATH_MAX}-byte limit — set AMP_BRIDGE_DIR to something shorter`,
        ),
      )
    }
    ensureDir(runtimeDir)
    ensureDir(inboxDir)
    ensureDir(threadsDir)
    unlinkOwnSocket()
    return new Promise<void>((resolve, reject) => {
      const s = net.createServer(handleConn)
      s.once('error', reject)
      s.listen(sockPath, () => {
        try {
          fs.chmodSync(sockPath, 0o600)
        } catch {
          /* best effort; the directory is already 0700 */
        }
        s.removeListener('error', reject)
        s.on('error', (e) => log(`SERVER_ERROR ${String(e)}`))
        server = s
        log(`LISTENING ${sockPath}`)
        resolve()
      })
    })
  }

  async function stopServer(): Promise<void> {
    if (!server) return
    const s = server
    server = null
    await new Promise<void>((resolve) => s.close(() => resolve()))
    unlinkOwnSocket()
  }

  async function enableThread(
    threadID: string,
    controllerThreadID?: string,
  ): Promise<string | null> {
    if (enabled.has(threadID)) return null
    if (!server) {
      try {
        await startServer()
      } catch (err) {
        return `could not listen — ${String(err)}`
      }
    }
    try {
      writeEntry(threadID)
    } catch (err) {
      if (enabled.size === 0) await stopServer()
      return `could not register the thread — ${String(err)}`
    }
    try {
      writeIntent(threadID, undefined, controllerThreadID)
    } catch (err) {
      // Not fatal: the live route works for this host, but say so in the log so
      // a later reload failure has a cause rather than looking like lost consent.
      log(`INTENT_WRITE_FAIL thread=${threadID} ${String(err)}`)
    }
    enabled.set(threadID, true)
    if (controllerThreadID) controllers.set(threadID, controllerThreadID)
    log(
      `ENABLED thread=${threadID}` +
        (controllerThreadID ? ` controller=${controllerThreadID}` : ''),
    )
    refreshAvailability()
    return null
  }

  // ---- commands ----------------------------------------------------------
  function refreshAvailability(): void {
    if (disposed) return
    const kind = (amp as { system?: { executor?: { kind?: string } } }).system?.executor?.kind
    if (kind !== 'local') {
      enableCmd?.setAvailability({
        type: 'disabled',
        reason: `needs a local executor (running on: ${String(kind)})`,
      })
      disableCmd?.setAvailability({ type: 'hidden' })
      enableManagedCmd?.setAvailability({
        type: 'disabled',
        reason: `needs a local executor (running on: ${String(kind)})`,
      })
      disableManagedCmd?.setAvailability({ type: 'hidden' })
      return
    }
    const active = amp.activeThread?.current
    if (!active) {
      enableCmd?.setAvailability({
        type: 'disabled',
        reason: 'send the first message in this session, then enable',
      })
      disableCmd?.setAvailability({ type: 'hidden' })
      enableManagedCmd?.setAvailability({
        type: 'disabled',
        reason: 'send the first message in this session, then enable',
      })
      disableManagedCmd?.setAvailability({ type: 'hidden' })
      return
    }
    const on = enabled.has(active.id as unknown as string)
    enableCmd?.setAvailability(
      on
        ? { type: 'disabled', reason: 'already enabled; use “Disable Claude inbox for this thread” to turn it off' }
        : { type: 'enabled' },
    )
    disableCmd?.setAvailability(on ? { type: 'enabled' } : { type: 'hidden' })
    enableManagedCmd?.setAvailability({ type: 'enabled' })
    disableManagedCmd?.setAvailability({ type: 'enabled' })
  }

  enableCmd = amp.registerCommand(
    'enable-claude-inbox',
    {
      title: 'Enable Claude inbox for this thread',
      category: 'amp-bridge',
      description: 'Let a paired Claude Code session ask this thread questions',
    },
    async (ctx: PluginCommandContext) => {
      if (!ctx.thread) {
        // A freshly started `amp` has no thread until the first message, so this
        // is the normal first-run experience, not an error state. Say what to do.
        await ctx.ui.notify(
          'amp-bridge: no active thread yet — send a message in this session first, then run this command again',
        )
        return
      }
      const kind = (amp as { system?: { executor?: { kind?: string } } }).system?.executor?.kind
      if (kind !== 'local') {
        await ctx.ui.notify(`amp-bridge: needs a local executor (running on: ${String(kind)})`)
        return
      }
      const threadID = ctx.thread.id as unknown as string
      if (!THREAD_ID_RE.test(threadID)) {
        await ctx.ui.notify(`amp-bridge: implausible thread id ${threadID}`)
        return
      }

      const error = await enableThread(threadID)
      if (error) {
        await ctx.ui.notify(`amp-bridge: ${error}`)
        return
      }
      await ctx.ui.notify(
        `Claude inbox enabled for this thread.\nsocket: ${sockPath}\n` +
          `This survives plugin and Amp process restarts until you disable it.\n` +
          `A paired Claude session can append requests; Amp must start each queued turn.`,
      )
    },
  )

  enableManagedCmd = amp.registerCommand(
    'enable-claude-inbox-for-thread',
    {
      title: 'Enable Claude inbox for another thread',
      category: 'amp-bridge',
      description: 'Pair a managed or background Amp thread by URL or thread id',
    },
    async (ctx: PluginCommandContext) => {
      const controllerThreadID = ctx.thread?.id as unknown as string | undefined
      if (!controllerThreadID || !THREAD_ID_RE.test(controllerThreadID)) {
        await ctx.ui.notify(
          'amp-bridge: send a message in this local session first; it will control the managed inbox',
        )
        return
      }
      const kind = (amp as { system?: { executor?: { kind?: string } } }).system?.executor?.kind
      if (kind !== 'local') {
        await ctx.ui.notify(`amp-bridge: needs a local executor (running on: ${String(kind)})`)
        return
      }

      const input = await ctx.ui.input({
        title: 'Enable Claude inbox for another thread',
        helpText: 'Paste an Amp thread URL or T-… id. This local thread will be its inbox controller.',
        submitButtonText: 'Continue',
      })
      if (input === undefined) return
      const threadID = threadIDFromInput(input)
      if (!threadID) {
        await ctx.ui.notify('amp-bridge: enter a valid Amp thread URL or T-… id')
        return
      }
      if (threadID === controllerThreadID) {
        await ctx.ui.notify(
          "amp-bridge: that is this thread — use 'Enable Claude inbox for this thread' instead",
        )
        return
      }
      if (enabled.has(threadID)) {
        await ctx.ui.notify(`amp-bridge: thread ${threadID} already has an inbox in this Amp session`)
        return
      }

      try {
        await Promise.race([
          amp.threads.get(threadID as never).state.get(),
          new Promise((_, reject) =>
            setTimeout(() => reject(new Error('thread lookup timed out')), 2_000),
          ),
        ])
      } catch (err) {
        await ctx.ui.notify(`amp-bridge: could not resolve thread ${threadID} — ${String(err)}`)
        return
      }
      const confirmed = await ctx.ui.confirm({
        title: 'Enable inbox for managed thread?',
        message:
          `Claude sessions on this machine will be able to append requests to **${threadID}**.\n\n` +
          `The consent is controlled by this Amp thread (**${controllerThreadID}**) and remains until explicitly disabled.`,
        confirmButtonText: 'Enable inbox',
      })
      if (!confirmed) return

      const error = await enableThread(threadID, controllerThreadID)
      if (error) {
        await ctx.ui.notify(`amp-bridge: ${error}`)
        return
      }
      await ctx.ui.notify(
        `Claude inbox enabled for managed thread ${threadID}.\n` +
          `controller: ${controllerThreadID}\n` +
          `This survives plugin reloads and returns when this controller thread reopens.`,
      )
    },
  )

  disableCmd = amp.registerCommand(
    'disable-claude-inbox',
    {
      title: 'Disable Claude inbox for this thread',
      category: 'amp-bridge',
      description: 'Stop accepting questions from Claude Code for this thread',
    },
    async (ctx: PluginCommandContext) => {
      const threadID = ctx.thread?.id as unknown as string | undefined
      if (!threadID || !enabled.has(threadID)) {
        await ctx.ui.notify('amp-bridge: this thread does not have a Claude inbox enabled')
        return
      }
      await disableThread(threadID, 'disabled from the palette')
      await ctx.ui.notify('Claude inbox disabled for this thread.')
    },
  )

  disableManagedCmd = amp.registerCommand(
    'disable-claude-inbox-for-thread',
    {
      title: 'Disable Claude inbox for another thread',
      category: 'amp-bridge',
      description: 'Revoke a managed or background thread pairing by URL or thread id',
    },
    async (ctx: PluginCommandContext) => {
      const controllerThreadID = ctx.thread?.id as unknown as string | undefined
      if (!controllerThreadID) {
        await ctx.ui.notify('amp-bridge: no active controller thread')
        return
      }
      const input = await ctx.ui.input({
        title: 'Disable Claude inbox for another thread',
        helpText: 'Paste the Amp thread URL or T-… id whose managed inbox this thread controls.',
        submitButtonText: 'Disable inbox',
      })
      if (input === undefined) return
      const threadID = threadIDFromInput(input)
      if (!threadID) {
        await ctx.ui.notify('amp-bridge: enter a valid Amp thread URL or T-… id')
        return
      }
      if (!enabled.has(threadID)) {
        let dormantController: string | null = null
        try {
          const saved = JSON.parse(fs.readFileSync(intentPath(threadID), 'utf8')) as {
            controller_thread_id?: unknown
          }
          if (
            typeof saved.controller_thread_id === 'string' &&
            THREAD_ID_RE.test(saved.controller_thread_id)
          ) {
            dormantController = saved.controller_thread_id
          }
        } catch {
          // The command may run from a different host after the original
          // controller was deleted. Only a readable managed intent is safe to
          // forget here; ordinary consent must still be revoked from its thread.
        }
        if (!dormantController) {
          await ctx.ui.notify(
            `amp-bridge: ${threadID} does not have a readable dormant managed inbox pairing`,
          )
          return
        }
        const confirmed = await ctx.ui.confirm({
          title: 'Forget dormant managed inbox?',
          message:
            `Managed thread **${threadID}** is not active in this Amp session. ` +
            `Its recorded controller is **${dormantController}**.\n\n` +
            `Forget its saved consent so it cannot be re-armed later?`,
          confirmButtonText: 'Forget pairing',
        })
        if (!confirmed) return
        // Revocation is safe from any explicit local palette action: it only
        // removes access. Removing the live entry too makes the revocation take
        // effect if another host left a stale registration behind.
        removeEntry(threadID)
        removeIntent(threadID)
        log(
          `FORGOT_MANAGED thread=${threadID} former_controller=${dormantController} ` +
            `requested_by=${controllerThreadID}`,
        )
        await ctx.ui.notify(
          `Claude inbox pairing forgotten for dormant managed thread ${threadID}.`,
        )
        return
      }

      const controller = controllers.get(threadID)
      if (!controller) {
        await ctx.ui.notify(
          `amp-bridge: ${threadID} is not a managed inbox; disable it from that thread instead`,
        )
        return
      }
      if (controller !== controllerThreadID) {
        const confirmed = await ctx.ui.confirm({
          title: 'Disable managed inbox controlled by another thread?',
          message:
            `Managed thread **${threadID}** is controlled by **${controller}**, not this thread ` +
            `(**${controllerThreadID}**).\n\nDisable it anyway? Revocation only removes access.`,
          confirmButtonText: 'Disable inbox',
        })
        if (!confirmed) return
        await disableThread(threadID, 'explicitly disabled from another local thread')
        await ctx.ui.notify(`Claude inbox disabled for managed thread ${threadID}.`)
        return
      }
      await disableThread(threadID, 'disabled from its controller thread')
      await ctx.ui.notify(`Claude inbox disabled for managed thread ${threadID}.`)
    },
  )

  async function disableThread(threadID: string, why: string): Promise<void> {
    enabled.delete(threadID)
    controllers.delete(threadID)
    const l = lanes.get(threadID)
    if (l) {
      const all = [l.inflight, ...l.queue].filter(Boolean) as ReqState[]
      for (const r of all) {
        if (r.timer) clearTimeout(r.timer)
        if (r.orphan) clearTimeout(r.orphan)
        respond(r, {
          error: `the Claude inbox was ${why} while the request was in flight`,
          code: 'disabled',
        })
      }
      lanes.delete(threadID)
    }
    removeEntry(threadID)
    removeIntent(threadID)
    log(`DISABLED thread=${threadID} (${why})`)
    // Fully quiescent again when the last thread goes.
    if (enabled.size === 0) await stopServer()
    refreshAvailability()
  }

  // ---- lifecycle ---------------------------------------------------------
  amp.on('session.start', (event: SessionStartEvent) => {
    refreshAvailability()
    // Unlike threads.get(id), this event proves the current Amp host actually
    // opened this thread. That is the safe point at which a consent recorded by
    // a previous host can be claimed after an Amp restart.
    queueRearm(event.thread.id as unknown as string, 'session-start')
  })

  amp.activeThread?.subscribe(() => refreshAvailability())

  amp.onDispose(async () => {
    // A hot path, not an edge case: load_plugin runs this on every reload, so it
    // executes constantly during development and must never leak a listening
    // socket or a stale registry entry.
    disposed = true
    log('DISPOSE_START')

    for (const l of lanes.values()) {
      for (const r of [l.inflight, ...l.queue].filter(Boolean) as ReqState[]) {
        if (r.timer) clearTimeout(r.timer)
        if (r.orphan) clearTimeout(r.orphan)
        if (r.noTurn) clearTimeout(r.noTurn)
        respond(r, { error: 'the plugin is reloading or unloading', code: 'disabled' })
      }
    }
    // Registrations are keyed on enablement, not on traffic. Lanes are created
    // lazily by the first request, so iterating them here left a registration
    // behind for every thread that was enabled and never asked anything.
    // Intents are deliberately kept: they are what a reload re-arms from.
    for (const threadID of enabled.keys()) removeEntry(threadID)
    lanes.clear()
    enabled.clear()
    controllers.clear()

    // end() queues the frame, destroy() would discard it. Grace, then force.
    // If the grace expires the caller sees EOF and ambiguous delivery — the
    // grace makes the clean case clean, it does not make the dirty case go away.
    await new Promise((r) => setTimeout(r, FLUSH_GRACE_MS))
    for (const c of clients) {
      try {
        c.destroy()
      } catch {
        /* already gone */
      }
    }
    clients.clear()

    // close() is ASYNCHRONOUS and waits for accepted clients, which is why the
    // destroy loop above comes first: awaiting it with a peer still attached
    // would block past the dispose budget and leave the socket bound.
    await stopServer()

    // Release the load guard LAST. Current Amp builds replace the worker process
    // on reload, but clearing it also keeps in-process module re-evaluation safe
    // in tests and compatible runtimes.
    delete g[LOAD_GUARD]
    log('DISPOSE_DONE')
  })

  // ---- re-arm across reloads and Amp restarts ----------------------------
  // The intent is durable per-thread consent. A plugin reload under the same Amp
  // host restores all inboxes that host served. A different host may claim only
  // the exact thread proven by activeThread or session.start; merely resolving
  // threads.get(id) is not ownership because Amp synchronizes threads globally.
  //
  // Rearms are serialized because load, active session startup and plugin events
  // can converge. Two concurrent startServer calls would race to bind one socket.
  let rearmSerial: Promise<void> = Promise.resolve()
  let hostIdentityLogged = false

  function queueRearm(onlyThreadID?: string, trigger = 'load'): void {
    rearmSerial = rearmSerial
      .then(() => rearmFromIntents(onlyThreadID, trigger))
      .catch((err) => log(`REARM_ERROR trigger=${trigger} ${String(err)}`))
  }

  async function rearmFromIntents(onlyThreadID?: string, trigger = 'load'): Promise<void> {
    let names: string[]
    try {
      names = fs.readdirSync(intentDir)
    } catch {
      return // nothing has ever been enabled; stay inert
    }

    if (!hostIdentityLogged) {
      hostIdentityLogged = true
      log(
        `HOST_IDENTITY amp_pid=${AMP_HOST_PID} ` +
          `started_at=${AMP_HOST_STARTED_AT === null ? 'unavailable' : JSON.stringify(AMP_HOST_STARTED_AT)}`,
      )
    }

    // A session that has moved to a remote executor cannot serve an inbox, and
    // 'unknown' is not evidence of that — refusing on it would lose re-arms to a
    // race with Amp's own startup, which is the opposite of the point.
    // executor.kind is readonly and the plugin API exposes no transition event;
    // session.start rechecks it whenever a thread session starts.
    const kind = (amp as { system?: { executor?: { kind?: string } } }).system?.executor?.kind
    if (kind === 'remote') {
      log('REARM_SKIP executor=remote')
      return
    }

    const activeThreadID = amp.activeThread?.current?.id as unknown as string | undefined
    let armed = 0
    let deferred = 0
    for (const name of names) {
      if (disposed) return
      if (!name.endsWith('.json')) continue
      const threadID = name.slice(0, -'.json'.length)
      if (!THREAD_ID_RE.test(threadID)) continue
      if (enabled.has(threadID)) continue

      let intent: {
        amp_pid?: unknown
        amp_started_at?: unknown
        enabled_at?: unknown
        controller_thread_id?: unknown
        plugin_pid?: unknown
        plugin_started_at?: unknown
      }
      try {
        intent = JSON.parse(fs.readFileSync(path.join(intentDir, name), 'utf8'))
      } catch {
        continue // unreadable or half-written; leave it for its owner
      }
      const hostPID = Number(intent.amp_pid)
      const hostStartedAt = typeof intent.amp_started_at === 'string' ? intent.amp_started_at : null
      const controllerThreadID =
        typeof intent.controller_thread_id === 'string' &&
        THREAD_ID_RE.test(intent.controller_thread_id)
          ? intent.controller_thread_id
          : undefined

      if (onlyThreadID && threadID !== onlyThreadID && controllerThreadID !== onlyThreadID) {
        continue
      }

      const sameHost =
        Number.isInteger(hostPID) &&
        hostPID === AMP_HOST_PID &&
        AMP_HOST_STARTED_AT !== null &&
        hostStartedAt === AMP_HOST_STARTED_AT
      // A managed intent is owned only through its controller. Letting the
      // target itself also prove ownership would permit two hosts to claim the
      // same registration when target and controller are open separately.
      const openedHere =
        controllerThreadID === undefined &&
        ((trigger === 'session-start' && threadID === onlyThreadID) ||
          (!onlyThreadID && threadID === activeThreadID))
      const controllerOpenedHere =
        controllerThreadID !== undefined &&
        ((trigger === 'session-start' && controllerThreadID === onlyThreadID) ||
          (!onlyThreadID && controllerThreadID === activeThreadID))

      if (!sameHost && !openedHere && !controllerOpenedHere) {
        // Consent is not stale merely because its last host exited. Keep it for
        // the next session.start of this exact thread; absence of ownership is
        // not evidence that the user revoked their decision.
        deferred++
        continue
      }

      // Amp enforces one executor per ownership thread (target for a normal
      // consent, controller for a managed consent), so two live hosts cannot
      // both satisfy this proof. If that invariant ever changes, the single
      // threads/<id>.json registration will need explicit arbitration.

      if (!server) {
        try {
          await startServer()
        } catch (err) {
          // Same rule as enable: never half-arm. No socket means no entry, so
          // the Go side sees a clean absence rather than a dead registration.
          log(`REARM_FAIL could not listen — ${String(err)}`)
          return
        }
      }
      try {
        writeEntry(threadID)
      } catch (err) {
        log(`REARM_FAIL thread=${threadID} ${String(err)}`)
        continue
      }

      // Claim a consent from an older host, and migrate legacy worker-owned
      // intents, only after the live entry is safely in place. Preserve when the
      // user originally enabled it; a re-arm is not a new consent decision.
      if (!sameHost || intent.plugin_pid !== undefined || intent.plugin_started_at !== undefined) {
        try {
          writeIntent(
            threadID,
            typeof intent.enabled_at === 'string' ? intent.enabled_at : undefined,
            controllerThreadID,
          )
        } catch (err) {
          log(`REARM_INTENT_WRITE_FAIL thread=${threadID} ${String(err)}`)
        }
      }
      enabled.set(threadID, true)
      if (controllerThreadID) controllers.set(threadID, controllerThreadID)
      armed++
      log(`REARMED thread=${threadID} trigger=${sameHost ? 'same-host' : trigger}`)
    }

    if (armed > 0) {
      refreshAvailability()
    } else if (enabled.size === 0) {
      // Started a listener for an intent that then failed to register. Inert is
      // the resting state; do not leave a socket bound serving nothing.
      await stopServer()
    }
    if (deferred > 0) log(`REARM_DEFERRED count=${deferred} trigger=${trigger}`)
  }

  refreshAvailability()

  // Fire and forget: a failure here costs convenience, never correctness, and
  // must not stop the plugin loading.
  queueRearm()
}
