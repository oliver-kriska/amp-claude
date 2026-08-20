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
 * writes no file and touches no directory. One palette command per thread turns
 * it on; disabling the last one takes everything back down.
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
  PluginCommandContext,
  CommandSubscription,
} from '@ampcode/plugin'
import * as net from 'node:net'
import * as fs from 'node:fs'
import * as path from 'node:path'

const PROTO = 1

// The envelope is allowed to be larger than the payload it carries: 64 KiB of
// text, JSON-encoded with escaping plus the other fields, exceeds 64 KiB, and
// equal caps would reject a legitimate maximum-size message.
const FRAME_MAX = 128 * 1024
const TEXT_MAX = 64 * 1024

const IDLE_MS = 10_000 // a connection that sends no complete frame is closed
const MAX_CONNS = 32
const QUEUE_MAX = 4
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
  const sockPath = path.join(inboxDir, `plugin-${process.pid}.sock`)
  const logPath = path.join(inboxDir, 'plugin.log')

  const enabled = new Map<string, true>()
  const lanes = new Map<string, Lane>()
  const clients = new Set<net.Socket>()
  let server: net.Server | null = null
  let disposed = false

  let enableCmd: CommandSubscription | null = null
  let disableCmd: CommandSubscription | null = null

  // ---- diagnostics -------------------------------------------------------
  // Only ever writes after the directory exists, i.e. after an explicit enable.
  // Logging at load would break the inertness the design depends on.
  function log(msg: string): void {
    if (!fs.existsSync(inboxDir)) return
    try {
      fs.appendFileSync(logPath, `${new Date().toISOString()} ${msg}\n`, { mode: 0o600 })
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
      await amp.threads.get(threadID as never).appendUserMessage({
        type: 'user-message',
        content,
      })
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

    const matched =
      (req.turnMsgId !== null && event.id === req.turnMsgId) || event.message.includes(req.marker)
    if (!matched) return

    try {
      const assistants = (event.messages || []).filter((m: { role: string }) => m.role === 'assistant')
      log(
        `END_CAPTURED id=${String(event.id)} req=${req.id} status=${event.status} ` +
          `messages=${(event.messages || []).length} assistants=${assistants.length}`,
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
            `Amp session and run 'amp-bridge: Enable Claude inbox for this thread'`,
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
    respond(req, { error: `${where}${detail}`, code: 'timeout' })

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
      return
    }
    const active = amp.activeThread?.current
    if (!active) {
      enableCmd?.setAvailability({
        type: 'disabled',
        reason: 'send the first message in this session, then enable',
      })
      disableCmd?.setAvailability({ type: 'hidden' })
      return
    }
    const on = enabled.has(active.id as unknown as string)
    enableCmd?.setAvailability(on ? { type: 'hidden' } : { type: 'enabled' })
    disableCmd?.setAvailability(on ? { type: 'enabled' } : { type: 'hidden' })
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

      if (!server) {
        try {
          await startServer()
        } catch (err) {
          // Never half-enable: no socket means no entry, so the Go side sees a
          // clean absence rather than a registration pointing at nothing.
          await ctx.ui.notify(`amp-bridge: could not listen — ${String(err)}`)
          return
        }
      }
      try {
        writeEntry(threadID)
      } catch (err) {
        await ctx.ui.notify(`amp-bridge: could not register the thread — ${String(err)}`)
        if (enabled.size === 0) await stopServer()
        return
      }
      enabled.set(threadID, true)
      log(`ENABLED thread=${threadID}`)
      refreshAvailability()
      await ctx.ui.notify(
        `Claude inbox enabled for this thread.\nsocket: ${sockPath}\nA paired Claude session can now use ask_amp.`,
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

  async function disableThread(threadID: string, why: string): Promise<void> {
    enabled.delete(threadID)
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
    log(`DISABLED thread=${threadID} (${why})`)
    // Fully quiescent again when the last thread goes.
    if (enabled.size === 0) await stopServer()
    refreshAvailability()
  }

  // ---- lifecycle ---------------------------------------------------------
  amp.on('session.start', () => {
    // No bookkeeping: session.start has no matching close event, so anything
    // derived from it could only mean "seen since load", which is unsound.
    refreshAvailability()
  })

  amp.activeThread?.subscribe(() => refreshAvailability())

  amp.onDispose(async () => {
    // A hot path, not an edge case: load_plugin runs this on every reload, so it
    // executes constantly during development and must never leak a listening
    // socket or a stale registry entry.
    disposed = true
    log('DISPOSE_START')

    for (const [threadID, l] of lanes) {
      for (const r of [l.inflight, ...l.queue].filter(Boolean) as ReqState[]) {
        if (r.timer) clearTimeout(r.timer)
        if (r.orphan) clearTimeout(r.orphan)
        respond(r, { error: 'the plugin is reloading or unloading', code: 'disabled' })
      }
      removeEntry(threadID)
    }
    lanes.clear()
    enabled.clear()

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
    log('DISPOSE_DONE')
  })

  refreshAvailability()
}
