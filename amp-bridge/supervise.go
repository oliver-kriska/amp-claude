package main

import (
	"net"
	"os"
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

// superviseSocket keeps the Amp listener alive and the socket path reachable.
func (b *bridge) superviseSocket(sock string) {
	budget := &restartBudget{max: b.restartMax, window: b.restartWindow}
	backoff := b.restartBackoff
	maxBackoff := 20 * b.restartBackoff

	for {
		ln := b.listener()
		if ln == nil {
			return
		}
		b.watchAndServe(sock, ln)

		if b.shuttingDown() {
			return
		}

		// Free the old descriptor before rebinding. Accept can fail on a
		// transient error (EMFILE) while the listener itself is still bound;
		// leaving it open makes bindSocket's liveness probe dial *us*, conclude
		// another bridge owns the path, and refuse to "hijack" it — which would
		// burn the entire restart budget on a fault that was recoverable, while
		// blaming a process that does not exist.
		_ = ln.Close()

		if !budget.allow(time.Now()) {
			b.logf("SOCKET_SUPERVISOR_EXHAUSTED %d restarts within %s — the fault is not "+
				"transient; exiting so it is visible rather than silent", budget.max, budget.window)
			b.escalate()
			return
		}

		time.Sleep(backoff)
		if backoff = 2 * backoff; backoff > maxBackoff {
			backoff = maxBackoff
		}
		// cleanup may have run while we slept; rebinding now would recreate the
		// socket file microseconds before exit and orphan it.
		if b.shuttingDown() {
			return
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

// ensureRegistered re-publishes the registry entry if it has gone missing.
//
// The socket and the registry entry are swept by the same tmp cleaner, but only
// the socket had a detector. A bridge whose registration is gone is invisible to
// `--list` and `--ask` — running, healthy, and unreachable, which is the failure
// mode this whole subsystem exists to eliminate. publish() recreates the runtime
// directory too, so a wholesale sweep recovers.
func (b *bridge) ensureRegistered() {
	if b.regPath == "" {
		return
	}
	if _, err := os.Lstat(b.regPath); err == nil {
		return
	}
	path, err := b.reg.publish()
	if err != nil {
		b.logf("REGISTRY_REPUBLISH_FAILED %v", err)
		return
	}
	b.logf("REGISTRY_REPUBLISHED %s — the bridge is discoverable again", path)
}

// watchAndServe runs the accept loop while watching the socket path.
//
// This watchdog exists because of a Unix detail that is easy to get wrong:
// unlinking a socket path does NOT make Accept fail. The listener holds the
// inode, so it stays blocked indefinitely while every new dial gets ENOENT.
// Verified on Go 1.26.6/macOS: after os.Remove, Accept was still blocked after
// two seconds. That is precisely the /tmp-sweeper failure, and watching for
// Accept to return could never detect it. Closing the listener ourselves is what
// unblocks Accept and hands control back to the restart loop.
func (b *bridge) watchAndServe(sock string, ln net.Listener) {
	served := make(chan struct{})
	go func() {
		defer close(served)
		b.guard("socket accept loop", func() { b.serveSocket(ln) })
	}()

	ticker := time.NewTicker(b.socketCheck)
	defer ticker.Stop()

	for {
		select {
		case <-served:
			return
		case <-ticker.C:
			if b.shuttingDown() {
				continue
			}
			if _, err := os.Lstat(sock); err != nil {
				b.logf("SOCKET_FILE_LOST %s (%v) — closing the listener to force a rebind",
					sock, err)
				_ = ln.Close() // unblocks Accept; serveSocket returns
			}
			b.ensureRegistered()
		}
	}
}
