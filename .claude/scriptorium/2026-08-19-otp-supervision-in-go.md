---
scriptorium: true
action: create
title: "OTP Supervision Patterns Ported to Go"
type: pattern
domain: general
tags: [go, otp, supervision, genserver, concurrency, panic-recovery, restart-intensity]
---

# OTP Supervision Patterns Ported to Go

Go has no supervision tree, and its two default failure modes are both silent or
total:

- A goroutine that **returns** leaves no trace. The rest of the process carries
  on holding a dead reference — the `:one_for_one` anti-pattern from
  [[OTP rest_for_one Supervision for Shared State]], with no supervisor to
  notice.
- A goroutine that **panics** takes the entire process down. There is no process
  boundary to contain it.

Applied while hardening a Go MCP server (`amp_claude/amp-bridge`). Three OTP
ideas port cleanly; one does not.

## Port: process isolation via `recover`

Wrap each independent unit of work — a connection, a request handler, a waiter —
so one failure cannot take down its peers.

```go
func (b *bridge) guard(what string, fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			b.logf("PANIC in %s: %v\n%s", what, r, debug.Stack())
		}
	}()
	fn()
	return false
}
```

Log the stack. A recovered panic without one is unactionable.

## Port: bounded restart intensity (`max_restarts` / `max_seconds`)

A supervisor that restarts forever hides a permanent fault. OTP's answer is a
restart budget; exceeding it escalates to the parent. In a single Go process,
"escalate" means exit non-zero so something above notices.

```go
type restartBudget struct {
	max    int
	window time.Duration
	mu     sync.Mutex
	times  []time.Time
}
```

Pair it with exponential backoff between restarts. Without the budget you have a
hot loop; without backoff you burn the budget in microseconds.

## Do NOT port: a GenServer for simple shared state

The most literal translation — replacing a mutex-guarded map with an owning
goroutine and a command channel — is usually the least useful. If the state is
small, the critical section short, and the code race-tested, an actor adds a hop
and fixes no bug.

**Decision rule: a process earns its place when it buys concurrency or isolation.
If it buys neither, use shared memory and a mutex.** Same reasoning as
[[Atomics-Based Rate Limiter Design]] choosing atomics over a GenServer, and the
"Mix task, not GenServer" call — OTP machinery with zero concurrency benefit is
just complexity.

## Watch the signal that actually fires

A supervisor is only as good as its failure detector, and the detector is the
easy part to get wrong.

**Unlinking a Unix socket path does not make `Accept` fail.** The listener holds
the inode, so it stays blocked indefinitely while every new `Dial` gets `ENOENT`.
Verified on Go 1.26.6 / macOS: `Accept` was still blocked two seconds after
`os.Remove`. Any supervisor that restarts "when the accept loop returns" is blind
to the most likely real-world cause of a lost socket — a `/tmp` sweeper. Poll the
path and close the listener yourself to unblock `Accept`.

**Mutation-check any test that pins concurrency or syscall behaviour.** The test
for the above passed with the watchdog deleted, because it simulated the sweeper
with `ln.Close()` — which unlinks the path as a side effect and therefore
exercised a different failure. Break the fix, confirm the test goes red, restore.
A test that cannot fail is documentation, not verification.

## Choosing self-heal vs. let-it-crash

"Let it crash" assumes a supervisor exists. Before designing around one, verify
it: does the runtime above actually restart you? If that is unverified, self-heal
with a bounded budget — it works whether or not the outer supervisor is real, and
still surfaces a permanent fault by exiting.
