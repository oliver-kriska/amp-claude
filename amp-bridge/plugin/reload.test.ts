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
const LOAD_GUARD = '__ampBridgeInboxLoaded'

let dir: string
let load = 0

type Handler = (ctx: unknown) => Promise<void> | void

interface Harness {
  dispose: () => Promise<void>
  enable: () => Promise<void>
  disable: () => Promise<void>
  notices: string[]
}

// Faithful only where the plugin actually touches it. A richer fake would drift
// from the published types without adding coverage.
async function loadPlugin(threadID: string | null = THREAD): Promise<Harness> {
  const commands = new Map<string, Handler>()
  const disposers: Array<() => Promise<void> | void> = []
  const notices: string[] = []

  const amp = {
    registerCommand(_id: string, opts: { title: string }, handler: Handler) {
      commands.set(opts.title, handler)
      return { setAvailability() {}, dispose() {} }
    },
    on() {},
    onDispose(cb: () => Promise<void> | void) {
      disposers.push(cb)
    },
    threads: { get: () => ({ appendUserMessage: async () => {}, state: { get: async () => 'idle' } }) },
    activeThread: { current: threadID ? { id: threadID } : null, subscribe: () => {} },
    system: { executor: { kind: 'local' } },
  }

  // Bun caches modules, and a reload is precisely a second evaluation of the
  // same file in the same process — so the query string is load-bearing.
  const mod = await import(`./amp-bridge-inbox.ts?load=${++load}`)
  mod.default(amp as never)

  const run = async (title: string) => {
    const h = commands.get(title)
    if (!h) throw new Error(`command not registered: ${title} (have: ${[...commands.keys()].join(', ') || 'none'})`)
    await h({ thread: threadID ? { id: threadID } : null, ui: { notify: async (m: string) => void notices.push(m) } })
  }

  return {
    notices,
    enable: () => run('Enable Claude inbox for this thread'),
    disable: () => run('Disable Claude inbox for this thread'),
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

const entry = () => path.join(dir, 'inbox', 'threads', `${THREAD}.json`)
const intent = () => path.join(dir, 'inbox', 'intent', `${THREAD}.json`)
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

  const second = await loadPlugin()
  await Bun.sleep(150) // re-arm is fire-and-forget at load
  expect(fs.existsSync(entry())).toBe(true)
  expect((await statusOver(sockPath())).enabled_threads).toEqual([THREAD])
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

test('an intent from a dead process is swept, not armed', async () => {
  const p = await loadPlugin()
  await p.enable()
  await p.dispose()

  // Same shape, a pid that cannot be running. Arming from this would enable a
  // thread the current session never consented to.
  fs.writeFileSync(
    intent(),
    JSON.stringify({ thread_id: THREAD, plugin_pid: 2, plugin_started_at: 1 }),
  )
  const second = await loadPlugin()
  await Bun.sleep(150)
  expect(fs.existsSync(entry())).toBe(false)
  await second.dispose()
})

test('our own pid from a previous process life is not armed', async () => {
  const p = await loadPlugin()
  await p.enable()
  await p.dispose()

  // Our pid, a start instant that is not ours: a process whose pid we inherited.
  fs.writeFileSync(
    intent(),
    JSON.stringify({ thread_id: THREAD, plugin_pid: process.pid, plugin_started_at: 1 }),
  )
  const second = await loadPlugin()
  await Bun.sleep(150)
  expect(fs.existsSync(entry())).toBe(false)
  expect(fs.existsSync(intent())).toBe(false) // swept: its owner cannot be alive
  await second.dispose()
})

test('dispose removes the registration of a thread that never had traffic', async () => {
  const p = await loadPlugin()
  await p.enable()
  expect(fs.existsSync(entry())).toBe(true)
  await p.dispose()
  // Lanes are created by the first request; this thread never had one.
  expect(fs.existsSync(entry())).toBe(false)
})
