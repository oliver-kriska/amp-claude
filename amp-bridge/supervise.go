package main

import (
	"runtime/debug"
	"sync"
	"time"
)

// Supervision, borrowed from OTP.
//
// Go has no supervision tree. An unrecovered panic in any goroutine takes the
// whole process down, and a goroutine that merely returns leaves nothing behind
// at all. Both are silent-degradation failures here: Claude keeps reporting a
// healthy MCP server while Amp can no longer reach us. That is the same shape as
// the :one_for_one anti-pattern — a sibling carrying on with a dead reference.
//
// Two OTP ideas map cleanly and earn their keep:
//
//   - Process isolation. One connection failing must not take down its peers,
//     let alone the channel. guard is the goroutine-level process boundary.
//   - Bounded restart intensity (max_restarts within max_seconds). Restarting
//     forever hides a permanent fault; giving up escalates it. Here escalation
//     means exiting non-zero, so the failure is visible instead of silent.
//
// Deliberately NOT borrowed: turning the pending-request map into a
// GenServer-style owning goroutine. It is mutex-guarded, twenty lines and
// race-tested — an actor would add a hop and fix no bug. Same reasoning as
// choosing atomics over a GenServer for a rate limiter: a process is only worth
// it when it buys concurrency or isolation, and here it buys neither.

// guard runs fn, turning a panic into a logged incident rather than a dead
// bridge. It reports whether fn panicked.
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

// restartBudget is OTP's max_restarts/max_seconds: at most max restarts within
// any sliding window. Exhausting it means the fault is permanent, not transient.
type restartBudget struct {
	max    int
	window time.Duration

	mu    sync.Mutex
	times []time.Time
}

// allow records a restart attempt and reports whether it is still within budget.
func (r *restartBudget) allow(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := now.Add(-r.window)
	kept := r.times[:0]
	for _, t := range r.times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.times = kept

	if len(r.times) >= r.max {
		return false
	}
	r.times = append(r.times, now)
	return true
}

// superviseSocket keeps the Amp listener alive.
//
// serveSocket returns when Accept fails, which for a Unix socket means the
// listener is gone: deliberately at shutdown, or because something removed the
// socket file underneath us — a /tmp sweeper is the realistic case. Without this
// the process would keep running as a healthy-looking bridge that Amp can never
// dial again, and nothing would say so.
func (b *bridge) superviseSocket(sock string) {
	budget := &restartBudget{max: b.restartMax, window: b.restartWindow}
	backoff := b.restartBackoff
	maxBackoff := 20 * b.restartBackoff

	for {
		ln := b.listener()
		if ln == nil {
			return
		}
		b.guard("socket accept loop", func() { b.serveSocket(ln) })

		if b.shuttingDown() {
			return
		}
		if !budget.allow(time.Now()) {
			b.logf("SOCKET_SUPERVISOR_EXHAUSTED %d restarts within %s — the fault is not "+
				"transient; exiting so it is visible rather than silent", budget.max, budget.window)
			b.escalate()
			return
		}

		time.Sleep(backoff)
		if backoff < maxBackoff {
			backoff *= 2
		}

		newLn, err := bindSocket(sock)
		if err != nil {
			b.logf("SOCKET_REBIND_FAILED %v", err)
			continue
		}
		b.setListener(newLn)
		b.logf("SOCKET_REBOUND %s — Amp can reach us again", sock)
		backoff = b.restartBackoff
	}
}
