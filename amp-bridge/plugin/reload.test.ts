// Reload behaviour, driven through the real module against a fake PluginAPI.
//
// This tier exists because the plugin's reload path has no symptom short of
// ask_amp failing: a load that returns early registers nothing, binds nothing
// and logs nothing, so it looks exactly like a session that was never enabled.
// That is how the load guard came to make every reload inert without anyone
// noticing.
import { afterEach, beforeEach, expect, test } from 'bun:test'
import * as fs from 'node:fs'
import * as net from 'node:net'
import * as os from 'node:os'
import * as path from 'node:path'

const THREAD = 'T-01a01877-2274-734d-8306-7c37b33f2a7f'
const MANAGED = 'T-01a0335c-7794-769d-b5b4-f8a8b8bb2347'
const OTHER = 'T-01a0444d-19fd-7afd-b695-c325c4536169'
const LOAD_GUARD = '__ampBridgeInboxLoaded'

let dir: string
let load = 0

type Handler = (ctx: unknown) => Promise<void> | void

interface Harness {
  dispose: () => Promise<void>
  enable: () => Promise<void>
  disable: () => Promise<void>
  enableManaged: (threadID: string) => Promise<void>
  disableManaged: (threadID: string) => Promise<void>
  startSession: (threadID: string) => void
  availability: (title: string) => unknown
  notices: string[]
}

// Faithful only where the plugin actually touches it. A richer fake would drift
// from the published types without adding coverage.
async function loadPlugin(
  threadID: string | null = THREAD,
  resolveThreads = true,
): Promise<Harness> {
  const commands = new Map<string, Handler>()
  const availabilities = new Map<string, unknown>()
  const disposers: Array<() => Promise<void> | void> = []
  const handlers = new Map<string, Handler[]>()
  const notices: string[] = []

  const amp = {
    registerCommand(_id: string, opts: { title: string }, handler: Handler) {
      commands.set(opts.title, handler)
      return {
        setAvailability(status: unknown) {
          availabilities.set(opts.title, status)
        },
        dispose() {},
      }
    },
    on(name: string, handler: Handler) {
      handlers.set(name, [...(handlers.get(name) ?? []), handler])
    },
    onDispose(cb: () => Promise<void> | void) {
      disposers.push(cb)
    },
    threads: {
      get: () => ({
        appendUserMessage: async () => {},
        state: {
          get: async () => {
            if (!resolveThreads) throw new Error('thread not found')
            return 'idle'
          },
        },
      }),
    },
    activeThread: { current: threadID ? { id: threadID } : null, subscribe: () => {} },
    system: { executor: { kind: 'local' } },
  }

  // Bun caches modules, and a reload is precisely a second evaluation of the
  // same file in the same process — so the query string is load-bearing.
  const mod = await import(`./amp-bridge-inbox.ts?load=${++load}`)
  mod.default(amp as never)

  const run = async (title: string, input?: string) => {
    const h = commands.get(title)
    if (!h) throw new Error(`command not registered: ${title} (have: ${[...commands.keys()].join(', ') || 'none'})`)
    await h({
      thread: threadID ? { id: threadID } : null,
      ui: {
        notify: async (m: string) => void notices.push(m),
        input: async () => input,
        confirm: async () => true,
      },
    })
  }

  return {
    notices,
    availability: (title: string) => availabilities.get(title),
    enable: () => run('Enable Claude inbox for this thread'),
    disable: () => run('Disable Claude inbox for this thread'),
    enableManaged: (id: string) => run('Enable Claude inbox for another thread', id),
    disableManaged: (id: string) => run('Disable Claude inbox for another thread', id),
    startSession: (id: string) => {
      for (const handler of handlers.get('session.start') ?? []) {
        void handler({ thread: { id } })
      }
    },
    dispose: async () => {
      for (const d of disposers) await d()
    },
  }
}

// The status op is what ask_amp uses to confirm an inbox really serves a thread,
// so asking over the socket tests the state the Go side will actually observe.
function statusOver(sock: string): Promise<{ enabled_threads: string[]; pid: number }> {
  return new Promise((resolve, reject) => {
    const c = net.createConnection(sock)
    let buf = ''
    c.on('error', reject)
    c.on('connect', () => c.write(JSON.stringify({ op: 'status', proto: 1 }) + '\n'))
    c.on('data', (d) => {
      buf += d
      if (buf.includes('\n')) {
        c.end()
        resolve(JSON.parse(buf))
      }
    })
  })
}

const entry = (threadID = THREAD) => path.join(dir, 'inbox', 'threads', `${threadID}.json`)
const intent = (threadID = THREAD) => path.join(dir, 'inbox', 'intent', `${threadID}.json`)
const sockPath = () => path.join(dir, 'inbox', `plugin-${process.pid}.sock`)

beforeEach(() => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ampb-reload-'))
  process.env.AMP_BRIDGE_DIR = dir
  delete (globalThis as Record<string, unknown>)[LOAD_GUARD]
})

afterEach(() => {
  delete (globalThis as Record<string, unknown>)[LOAD_GUARD]
  fs.rmSync(dir, { recursive: true, force: true })
})

test('loading writes nothing and binds nothing until enabled', async () => {
  await loadPlugin()
  expect(fs.existsSync(path.join(dir, 'inbox'))).toBe(false)
})

test('enable registers the thread and records the intent', async () => {
  const p = await loadPlugin()
  await p.enable()
  expect(fs.existsSync(entry())).toBe(true)
  expect(fs.existsSync(intent())).toBe(true)
  const saved = JSON.parse(fs.readFileSync(intent(), 'utf8'))
  expect(saved.amp_pid).toBe(process.ppid)
  expect(typeof saved.amp_started_at).toBe('string')
  expect(saved.plugin_pid).toBeUndefined()
  expect(saved.plugin_started_at).toBeUndefined()
  expect((await statusOver(sockPath())).enabled_threads).toEqual([THREAD])
  await p.dispose()
})

test('a reload re-arms the thread without a second keystroke', async () => {
  const first = await loadPlugin()
  await first.enable()
  await first.dispose()

  // Dispose clears the live registration but must keep the decision.
  expect(fs.existsSync(entry())).toBe(false)
  expect(fs.existsSync(intent())).toBe(true)

  // Amp replaces the plugin worker on reload. Simulate that change: ownership
  // belongs to the stable parent host, not to this disposable worker pid.
  const saved = JSON.parse(fs.readFileSync(intent(), 'utf8'))
  saved.plugin_pid = 2
  saved.plugin_started_at = 1
  fs.writeFileSync(intent(), JSON.stringify(saved))

  const second = await loadPlugin()
  await Bun.sleep(150) // re-arm is fire-and-forget at load
  expect(fs.existsSync(entry())).toBe(true)
  expect((await statusOver(sockPath())).enabled_threads).toEqual([THREAD])
  expect(second.availability('Enable Claude inbox for this thread')).toEqual({
    type: 'disabled',
    reason: 'already enabled; use “Disable Claude inbox for this thread” to turn it off',
  })
  expect(second.availability('Disable Claude inbox for this thread')).toEqual({ type: 'enabled' })
  await second.dispose()
})

test('an explicit disable is not undone by a reload', async () => {
  const first = await loadPlugin()
  await first.enable()
  await first.disable()
  expect(fs.existsSync(intent())).toBe(false)
  await first.dispose()

  const second = await loadPlugin()
  await Bun.sleep(150)
  expect(fs.existsSync(entry())).toBe(false)
  await second.dispose()
})

test('a local controller can explicitly enable and disable a managed thread', async () => {
  const p = await loadPlugin()
  await p.enableManaged(`https://ampcode.com/threads/${MANAGED}`)

  expect(fs.existsSync(entry(MANAGED))).toBe(true)
  expect(fs.existsSync(intent(MANAGED))).toBe(true)
  const saved = JSON.parse(fs.readFileSync(intent(MANAGED), 'utf8'))
  expect(saved.thread_id).toBe(MANAGED)
  expect(saved.controller_thread_id).toBe(THREAD)
  expect((await statusOver(sockPath())).enabled_threads).toEqual([MANAGED])

  await p.disableManaged(MANAGED)
  expect(fs.existsSync(entry(MANAGED))).toBe(false)
  expect(fs.existsSync(intent(MANAGED))).toBe(false)
  await p.dispose()
})

test('a dormant managed consent can be forgotten after its controller is gone', async () => {
  const first = await loadPlugin()
  await first.enableManaged(MANAGED)
  await first.dispose()

  const saved = JSON.parse(fs.readFileSync(intent(MANAGED), 'utf8'))
  saved.amp_pid = 2
  saved.amp_started_at = 'old'
  fs.writeFileSync(intent(MANAGED), JSON.stringify(saved))

  const other = await loadPlugin(OTHER)
  await Bun.sleep(150)
  expect(fs.existsSync(entry(MANAGED))).toBe(false)
  expect(fs.existsSync(intent(MANAGED))).toBe(true)

  await other.disableManaged(MANAGED)
  expect(fs.existsSync(entry(MANAGED))).toBe(false)
  expect(fs.existsSync(intent(MANAGED))).toBe(false)
  expect(other.notices.at(-1)).toContain('pairing forgotten')
  await other.dispose()
})

test('a live managed consent can be revoked after its controller is gone', async () => {
  const first = await loadPlugin()
  await first.enableManaged(MANAGED)
  await first.dispose()

  // Same Amp host, different active thread: load-time re-arm keeps the managed
  // inbox live even though its recorded controller is no longer available.
  const other = await loadPlugin(OTHER)
  await Bun.sleep(150)
  expect(fs.existsSync(entry(MANAGED))).toBe(true)
  expect(fs.existsSync(intent(MANAGED))).toBe(true)

  await other.disableManaged(MANAGED)
  expect(fs.existsSync(entry(MANAGED))).toBe(false)
  expect(fs.existsSync(intent(MANAGED))).toBe(false)
  expect(other.notices.at(-1)).toContain('inbox disabled')
  await other.dispose()
})

test('managed enable validates the pasted thread before recording consent', async () => {
  const p = await loadPlugin(THREAD, false)
  await p.enableManaged(MANAGED)
  expect(fs.existsSync(entry(MANAGED))).toBe(false)
  expect(fs.existsSync(intent(MANAGED))).toBe(false)
  expect(p.notices.at(-1)).toContain(`could not resolve thread ${MANAGED}`)
  await p.dispose()
})

test('managed disable cannot revoke an ordinary thread consent', async () => {
  const p = await loadPlugin()
  await p.enable()
  await p.disableManaged(THREAD)
  expect(fs.existsSync(entry())).toBe(true)
  expect(fs.existsSync(intent())).toBe(true)
  expect(p.notices.at(-1)).toContain('is not a managed inbox')
  await p.disable()
  await p.dispose()
})

test('a restarted controller thread reclaims its managed thread consent', async () => {
  const first = await loadPlugin()
  await first.enableManaged(MANAGED)
  await first.dispose()

  const saved = JSON.parse(fs.readFileSync(intent(MANAGED), 'utf8'))
  saved.amp_pid = 2
  saved.amp_started_at = 'old'
  fs.writeFileSync(intent(MANAGED), JSON.stringify(saved))

  const second = await loadPlugin(null)
  await Bun.sleep(150)
  expect(fs.existsSync(entry(MANAGED))).toBe(false)

  second.startSession(THREAD)
  await Bun.sleep(150)
  expect(fs.existsSync(entry(MANAGED))).toBe(true)
  const claimed = JSON.parse(fs.readFileSync(intent(MANAGED), 'utf8'))
  expect(claimed.controller_thread_id).toBe(THREAD)
  expect(claimed.amp_pid).toBe(process.ppid)
  await second.dispose()
})

test('an unrelated Amp host cannot claim managed thread consent', async () => {
  const first = await loadPlugin()
  await first.enableManaged(MANAGED)
  await first.dispose()

  const saved = JSON.parse(fs.readFileSync(intent(MANAGED), 'utf8'))
  saved.amp_pid = 2
  saved.amp_started_at = 'old'
  fs.writeFileSync(intent(MANAGED), JSON.stringify(saved))

  const second = await loadPlugin(null)
  second.startSession('T-unrelated')
  await Bun.sleep(150)
  expect(fs.existsSync(entry(MANAGED))).toBe(false)
  expect(fs.existsSync(intent(MANAGED))).toBe(true)
  await second.dispose()
})

test('opening the managed target elsewhere does not race its controller for ownership', async () => {
  const first = await loadPlugin()
  await first.enableManaged(MANAGED)
  await first.dispose()

  const saved = JSON.parse(fs.readFileSync(intent(MANAGED), 'utf8'))
  saved.amp_pid = 2
  saved.amp_started_at = 'old'
  fs.writeFileSync(intent(MANAGED), JSON.stringify(saved))

  const targetHost = await loadPlugin(null)
  targetHost.startSession(MANAGED)
  await Bun.sleep(150)
  expect(fs.existsSync(entry(MANAGED))).toBe(false)
  expect(fs.existsSync(intent(MANAGED))).toBe(true)
  await targetHost.dispose()
})

test('a restarted Amp host reclaims consent when that exact thread starts', async () => {
  const p = await loadPlugin()
  await p.enable()
  await p.dispose()

  // The process that last served the inbox is gone, but the user's thread-level
  // decision remains. A host with no active thread must not claim it merely
  // because threads.get(id) can resolve globally.
  fs.writeFileSync(
    intent(),
    JSON.stringify({
      thread_id: THREAD,
      amp_pid: 2,
      amp_started_at: 'old',
      enabled_at: '2026-08-20T12:00:00.000Z',
    }),
  )
  const second = await loadPlugin(null)
  await Bun.sleep(150)
  expect(fs.existsSync(entry())).toBe(false)
  expect(fs.existsSync(intent())).toBe(true)

  second.startSession(THREAD)
  await Bun.sleep(150)
  expect(fs.existsSync(entry())).toBe(true)
  const claimed = JSON.parse(fs.readFileSync(intent(), 'utf8'))
  expect(claimed.amp_pid).toBe(process.ppid)
  expect(claimed.enabled_at).toBe('2026-08-20T12:00:00.000Z')
  await second.dispose()
})

test('pid reuse without ownership proof leaves consent dormant and intact', async () => {
  const p = await loadPlugin()
  await p.enable()
  await p.dispose()

  // Same pid, different start token: not our host. With no active thread or
  // session.start proof, adopting would expose a thread this process never opened.
  fs.writeFileSync(
    intent(),
    JSON.stringify({ thread_id: THREAD, amp_pid: process.ppid, amp_started_at: 'old' }),
  )
  const second = await loadPlugin(null)
  await Bun.sleep(150)
  expect(fs.existsSync(entry())).toBe(false)
  expect(fs.existsSync(intent())).toBe(true)
  await second.dispose()
})

test('session start does not auto-enable a thread without consent', async () => {
  const p = await loadPlugin(null)
  p.startSession(THREAD)
  await Bun.sleep(150)
  expect(fs.existsSync(entry())).toBe(false)
  expect(fs.existsSync(intent())).toBe(false)
  await p.dispose()
})

test('dispose removes the registration of a thread that never had traffic', async () => {
  const p = await loadPlugin()
  await p.enable()
  expect(fs.existsSync(entry())).toBe(true)
  await p.dispose()
  // Lanes are created by the first request; this thread never had one.
  expect(fs.existsSync(entry())).toBe(false)
})
