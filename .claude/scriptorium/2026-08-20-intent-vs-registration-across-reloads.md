---
scriptorium: true
action: create
title: "Separate the user's intent from the live registration"
type: pattern
domain: general
tags: [plugins, reload, state, process-identity, opt-in]
---

A plugin, daemon or extension that a user explicitly turns on faces a question
the first version always gets wrong: what happens to that choice when the
process reloads?

## The two states are not one state

Storing "enabled" in a single place conflates two different facts:

1. **The live registration** — there is a socket, it is bound, here is where to
   reach it. Owned by the runtime. Other processes are entitled to garbage-collect
   it when the thing behind it is gone.
2. **The user's decision** — they chose this. Owned by the user. Nothing should
   ever collect it except an explicit un-choosing.

Keeping both in one file creates a specific, nasty race: during a reload the
socket is *legitimately* absent, so a well-behaved consumer sweeps the stale
registration — and destroys the decision you were about to restore. The sweep is
correct; the storage was wrong.

**The fix is two directories.** `live/` is written on enable, deleted on dispose,
and sweepable by anyone. `intent/` is written on enable and deleted *only* on an
explicit disable. Load reads `intent/` and re-arms. Consumers never look at
`intent/` at all.

## pid is not a process identity

Re-arming needs to know "did *this* process make that choice". The obvious key
is the pid, and it is wrong: pids are reused, so a later process inherits
consent that a dead one gave. That is a privilege escalation dressed as a
convenience feature.

Add the process start instant. In Node:

```js
const PROCESS_STARTED_AT = Math.round(Date.now() - process.uptime() * 1000)
```

Stable across a module reload (uptime is a property of the process, not the
module) and distinct per process. Gate re-arm on **pid AND start instant**, with
a small tolerance for clock jitter. Clock drift then only ever *loses* a re-arm,
which is the safe direction — the user re-enables by hand.

This also gives you correct sweeping: an intent whose pid is yours but whose
start instant is not belongs to a process that cannot still be running, because
you are holding its pid. Delete it. An intent with a foreign pid is somebody
else's, so check liveness (`process.kill(pid, 0)`, treating `EPERM` as alive)
and leave it alone if it answers.

## Decide what a "reload" is allowed to mean

Surviving a reload of the same process and surviving a full restart are
different promises:

- **Same process** — nothing about who holds what has changed. Restore it. The
  user reloaded to get new code, not to revoke a grant.
- **New process** — restoring here silently converts "enabled by a deliberate
  act inside the session that owns this resource" into "enabled once, enabled
  forever, in whatever session opens it next". If the opt-in was the security
  control, this deletes it.

Pick deliberately and say which in the docs. The pid+start gate gives you the
first without the second for free, and it degrades safely: if the host turns out
to reload in a fresh process, the gate simply never fires and you are back to
today's behaviour rather than to a wrong one.

## The bug hiding underneath

Load guards that prevent duplicate initialisation are usually set on a global
and never cleared. If the host reloads **in-process**, the global survives, the
reloaded copy takes the early return, and the plugin comes back completely
inert — no commands, no listener, no log line, nothing to notice until something
downstream fails. Release the guard at the *end* of dispose: duplicate-copy
protection is preserved (a second copy in the same load still sees it) and a
reload can actually reload.

Test it by loading, disposing, and loading again in one process, asserting the
second load initialises. Related: [[green-tests-that-read-your-machine]] — this
is another defect with no symptom until something far away breaks.
