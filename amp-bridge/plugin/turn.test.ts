// What happens between "the message was appended" and "a turn ran".
//
// This tier exists because the gap between those two is where the plugin failed
// in the field on its first day: appendUserMessage resolved, no turn started,
// and the caller waited out a five minute timeout for an answer nobody was
// writing. Nothing in the unit or integration tiers could see it, because both
// halves were individually behaving correctly.
import { afterEach, beforeEach, expect, test } from 'bun:test'
import * as fs from 'node:fs'
import * as net from 'node:net'
import * as os from 'node:os'
import * as path from 'node:path'

const THREAD = 'T-01a02275-c11d-7585-9b65-bdc0e4c58162'
const MANAGED = 'T-01a0335c-7794-769d-b5b4-f8a8b8bb2347'
const LOAD_GUARD = '__ampBridgeInboxLoaded'

let dir: string
let load = 0

type Ev = (e: unknown) => unknown

interface Harness {
  enable: () => Promise<void>
  enableManaged: () => Promise<void>
  dispose: () => Promise<void>
  appendCalls: Array<{ content: string; options: unknown }>
  fire: (name: string, event: unknown) => void
  notices: string[]
  setState: (s: unknown) => void
}

async function loadPlugin(): Promise<Harness> {
  const commands = new Map<string, (ctx: unknown) => Promise<void> | void>()
  const disposers: Array<() => Promise<void> | void> = []
  const handlers = new Map<string, Ev[]>()
  const appendCalls: Array<{ content: string; options: unknown }> = []
  const notices: string[] = []
  let threadState: unknown = 'idle'

  const amp = {
    registerCommand(_id: string, opts: { title: string }, handler: (ctx: unknown) => Promise<void> | void) {
      commands.set(opts.title, handler)
      return { setAvailability() {}, dispose() {} }
    },
    on(name: string, h: Ev) {
      handlers.set(name, [...(handlers.get(name) ?? []), h])
    },
    onDispose(cb: () => Promise<void> | void) {
      disposers.push(cb)
    },
    threads: {
      get: () => ({
        appendUserMessage: async (m: { content: string }, options?: unknown) => {
          appendCalls.push({ content: m.content, options })
        },
        state: { get: async () => threadState },
      }),
    },
    activeThread: { current: { id: THREAD }, subscribe: () => {} },
    system: { executor: { kind: 'local' } },
    ui: { notify: async (message: string) => void notices.push(message) },
  }

  const mod = await import(`./amp-bridge-inbox.ts?turn=${++load}`)
  mod.default(amp as never)

  return {
    appendCalls,
    notices,
    setState: (s: unknown) => {
      threadState = s
    },
    fire: (name, event) => {
      for (const h of handlers.get(name) ?? []) h(event)
    },
    enable: async () => {
      const h = commands.get('Enable Claude inbox for this thread')
      if (!h) throw new Error('enable command not registered')
      await h({ thread: { id: THREAD }, ui: { notify: async () => {} } })
    },
    enableManaged: async () => {
      const h = commands.get('Enable Claude inbox for another thread')
      if (!h) throw new Error('managed enable command not registered')
      await h({
        thread: { id: THREAD },
        ui: {
          notify: async () => {},
          input: async () => MANAGED,
          confirm: async () => true,
        },
      })
    },
    dispose: async () => {
      for (const d of disposers) await d()
    },
  }
}

const sockPath = () => path.join(dir, 'inbox', `plugin-${process.pid}.sock`)

// Speaks the same wire protocol ask_amp does, so what the test observes is what
// the Go client would observe.
function ask(timeoutMs: number, threadID = THREAD): Promise<Record<string, unknown>> {
  return new Promise((resolve, reject) => {
    const c = net.createConnection(sockPath())
    let buf = ''
    c.on('error', reject)
    c.on('connect', () =>
      c.write(
        JSON.stringify({
          op: 'ask',
          id: 'abcdef012345',
          thread_id: threadID,
          text: 'is the build green?',
          from: 'test-session',
          timeout_ms: timeoutMs,
        }) + '\n',
      ),
    )
    c.on('data', (d) => {
      buf += d
      if (buf.includes('\n')) {
        c.end()
        resolve(JSON.parse(buf.split('\n')[0]))
      }
    })
  })
}

beforeEach(() => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ampb-turn-'))
  process.env.AMP_BRIDGE_DIR = dir
  delete (globalThis as Record<string, unknown>)[LOAD_GUARD]
})

afterEach(() => {
  delete (globalThis as Record<string, unknown>)[LOAD_GUARD]
  fs.rmSync(dir, { recursive: true, force: true })
})

test('the append asks Amp to steer, so it is not queued behind other work', async () => {
  const p = await loadPlugin()
  await p.enable()
  p.setState('idle')
  void ask(2000).catch(() => {})
  await Bun.sleep(150)

  expect(p.appendCalls).toHaveLength(1)
  expect(p.appendCalls[0].options).toEqual({ steer: true })
  await p.dispose()
})

test('an idle thread that never starts a turn fails fast and says so', async () => {
  const p = await loadPlugin()
  await p.enable()
  p.setState('idle') // nothing running, and nothing ever will

  const started = Date.now()
  const reply = await ask(2000) // grace is a quarter of this: 500ms
  const elapsed = Date.now() - started

  expect(reply.code).toBe('no-turn')
  expect(String(reply.error)).toContain('did not start a turn')
  // The point of the fix: this used to consume the entire timeout.
  expect(elapsed).toBeLessThan(1800)
  await p.dispose()
})

test('the message is reported as already delivered, not lost', async () => {
  const p = await loadPlugin()
  await p.enable()
  p.setState('idle')
  const reply = await ask(2000)
  // Resending would duplicate the question, so the caller must be told it landed.
  expect(String(reply.error)).toContain('queued in the thread but unanswered for now')
  expect(String(reply.error)).toContain('next activity in that thread may pick it up')
  expect(String(reply.error)).not.toMatch(/send it again|resend it/i)
  await p.dispose()
})

test('an idle managed thread notifies its controller that the request is queued', async () => {
  const p = await loadPlugin()
  await p.enableManaged()
  p.setState('idle')

  const reply = await ask(2000, MANAGED)
  expect(reply.code).toBe('no-turn')
  expect(p.notices).toContainEqual(expect.stringContaining(`managed thread ${MANAGED}`))
  expect(p.notices).toContainEqual(expect.stringContaining('do not ask Claude to resend'))
  await p.dispose()
})

test('a busy thread is left alone — queued is not stalled', async () => {
  const p = await loadPlugin()
  await p.enable()
  p.setState('running') // somebody else's turn; ours is queued behind it

  const reply = await ask(2000)
  // Must ride out its own timeout rather than being killed by the watchdog.
  expect(reply.code).toBe('timeout')
  await p.dispose()
})

test('a thread awaiting approval is busy, not stalled', async () => {
  const p = await loadPlugin()
  await p.enable()
  p.setState('awaiting-approval')

  const reply = await ask(2000)
  expect(reply.code).toBe('timeout')
  await p.dispose()
})

test('a thread in error fails fast instead of waiting out the deadline', async () => {
  const p = await loadPlugin()
  await p.enable()
  p.setState('error')

  const reply = await ask(2000)
  expect(reply.code).toBe('no-turn')
  expect(String(reply.error)).toContain('thread state: error')
  await p.dispose()
})

test('an unrecognised future state is treated as busy, never as stalled', async () => {
  const p = await loadPlugin()
  await p.enable()
  p.setState('paused')

  const reply = await ask(2000)
  expect(reply.code).toBe('timeout')
  await p.dispose()
})

test('an unreadable thread state is treated as busy, never as stalled', async () => {
  const p = await loadPlugin()
  await p.enable()
  p.setState(null) // probe failed; that is not evidence of a stall

  const reply = await ask(2000)
  expect(reply.code).toBe('timeout')
  await p.dispose()
})

test('a turn that does start disarms the watchdog and answers normally', async () => {
  const p = await loadPlugin()
  await p.enable()
  p.setState('idle') // idle, but a turn is about to start for us

  const pending = ask(4000)
  await Bun.sleep(120)

  const marker = /\[(amp-bridge-req-[0-9a-f]{12})\]/.exec(p.appendCalls[0].content)
  expect(marker).not.toBeNull()

  p.fire('agent.start', { thread: { id: THREAD }, message: `... ${marker![1]} ...`, id: 'M-1' })
  await Bun.sleep(700) // comfortably past the 1s grace this timeout implies
  p.fire('agent.end', {
    thread: { id: THREAD },
    id: 'M-1',
    status: 'done',
    message: '',
    messages: [{ role: 'assistant', id: 'M-1', content: [{ type: 'text', text: 'yes, green' }] }],
  })

  const reply = await pending
  expect(reply.reply).toBe('yes, green')
  expect(reply.code).toBeUndefined()
  await p.dispose()
})

test('a request steered into an existing turn is correlated at agent end', async () => {
  const p = await loadPlugin()
  await p.enable()
  p.setState('running')

  // The turn predates the bridge request, so its agent.start cannot contain a
  // marker that does not exist yet.
  p.fire('agent.start', { thread: { id: THREAD }, message: 'run a long tool', id: 'M-existing' })
  const pending = ask(4000)
  await Bun.sleep(120)

  const marker = /\[(amp-bridge-req-[0-9a-f]{12})\]/.exec(p.appendCalls[0].content)
  expect(marker).not.toBeNull()
  p.fire('agent.end', {
    thread: { id: THREAD },
    id: 'M-existing',
    status: 'done',
    message: 'run a long tool',
    messages: [
      { role: 'user', id: 'U-original', content: [{ type: 'text', text: 'run a long tool' }] },
      { role: 'assistant', id: 'A-before', content: [{ type: 'text', text: 'starting the tool' }] },
      { role: 'user', id: 'U-steer', content: [{ type: 'text', text: p.appendCalls[0].content }] },
      { role: 'assistant', id: 'A-after', content: [{ type: 'text', text: 'steered answer' }] },
    ],
  })

  const reply = await pending
  expect(reply.reply).toBe('steered answer')
  expect(reply.code).toBeUndefined()
  await p.dispose()
})

test('log lines name the pid, since every Amp session shares one file', async () => {
  const p = await loadPlugin()
  await p.enable()
  const logged = fs.readFileSync(path.join(dir, 'inbox', 'plugin.log'), 'utf8')
  expect(logged).toContain(`[${process.pid}]`)
  await p.dispose()
})

// The `texts.length === 0` guard runs before normalization, so a text block that
// is empty or whitespace slips past it and only becomes blank after joining and
// trimming. It then went back as a successful zero-byte reply, which reads as
// "Amp had nothing to add" — a claim the plugin is in no position to make.
//
// Not covered here: an answer consisting only of the request marker. Stripping
// removes the id but leaves its brackets, so that normalizes to "[]" and stays a
// (strange) non-empty answer. Widening the strip is a separate decision about
// marker syntax, not part of the blank check.
test.each([
  ['an empty text block', ''],
  ['whitespace only', '   \n\t  '],
])('a blank answer is reported, not returned as success: %s', async (_name, text) => {
  const p = await loadPlugin()
  await p.enable()
  p.setState('idle')

  const pending = ask(4000)
  await Bun.sleep(120)

  const marker = /\[(amp-bridge-req-[0-9a-f]{12})\]/.exec(p.appendCalls[0].content)
  expect(marker).not.toBeNull()

  p.fire('agent.start', { thread: { id: THREAD }, message: `... ${marker![1]} ...`, id: 'M-1' })
  p.fire('agent.end', {
    thread: { id: THREAD },
    id: 'M-1',
    status: 'done',
    message: '',
    messages: [{ role: 'assistant', id: 'M-1', content: [{ type: 'text', text }] }],
  })

  const reply = await pending
  expect(reply.reply).toBeUndefined()
  expect(reply.code).toBe('empty-answer')
  expect(reply.error).toContain('empty')
  await p.dispose()
})
