package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGuardContainsAPanic(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if panicked := h.b.guard("healthy work", func() {}); panicked {
		t.Error("guard reported a panic that did not happen")
	}

	// The whole point: this must not take the process down.
	panicked := h.b.guard("exploding work", func() { panic("boom") })
	if !panicked {
		t.Fatal("guard must report that fn panicked")
	}
	log := h.log.String()
	if !strings.Contains(log, "PANIC in exploding work") || !strings.Contains(log, "boom") {
		t.Errorf("the panic must be logged with its context: %q", log)
	}
	if !strings.Contains(log, "goroutine") {
		t.Error("a stack trace should be logged; without it the panic is unactionable")
	}
}

func TestRestartBudget(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	t.Run("allows up to max within the window", func(t *testing.T) {
		t.Parallel()
		b := &restartBudget{max: 3, window: time.Minute}
		for i := range 3 {
			if !b.allow(base.Add(time.Duration(i) * time.Second)) {
				t.Fatalf("restart %d should be within budget", i)
			}
		}
		if b.allow(base.Add(4 * time.Second)) {
			t.Error("the fourth restart in the window must be refused — the fault is not transient")
		}
	})

	t.Run("old restarts fall out of the window", func(t *testing.T) {
		t.Parallel()
		b := &restartBudget{max: 2, window: time.Minute}
		if !b.allow(base) || !b.allow(base.Add(time.Second)) {
			t.Fatal("first two should be allowed")
		}
		if b.allow(base.Add(2 * time.Second)) {
			t.Fatal("third within the window should be refused")
		}
		// Well past the window: the earlier failures are no longer evidence.
		if !b.allow(base.Add(2 * time.Minute)) {
			t.Error("a restart long after the window must be allowed again")
		}
	})
}

// superviseHarness binds a real socket and starts supervision over it.
func superviseHarness(t *testing.T) (*harness, string) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "b.sock")

	h := newHarness(t)
	h.b.restartMax = 3
	h.b.restartWindow = time.Minute

	ln, err := bindSocket(sock)
	if err != nil {
		t.Fatalf("bindSocket: %v", err)
	}
	h.b.setListener(ln)
	go h.b.superviseSocket(sock)
	t.Cleanup(func() {
		h.b.beginShutdown()
		if l := h.b.listener(); l != nil {
			_ = l.Close()
		}
	})
	return h, sock
}

// TestSupervisorRebindsASweptSocket is the regression test that matters.
//
// Unlinking the socket path does NOT make Accept fail — the listener keeps the
// inode and stays blocked, while every new dial gets ENOENT. An earlier version
// of this test simulated the sweeper with ln.Close(), which unlinks the path as
// a side effect, and so exercised the wrong failure entirely: the watchdog could
// have been absent and it would still have passed.
func TestSupervisorRebindsASweptSocket(t *testing.T) {
	t.Parallel()
	h, sock := superviseHarness(t)

	conn := dialBridge(t, sock)
	if err := json.NewEncoder(conn).Encode(ampRequest{Text: "before"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, "the first event", func() bool { return len(h.notifications(t)) == 1 })

	// Exactly what a /tmp sweeper does, and nothing else.
	if err := os.Remove(sock); err != nil {
		t.Fatalf("remove socket: %v", err)
	}
	waitFor(t, "the sweep to be noticed", func() bool {
		return strings.Contains(h.log.String(), "SOCKET_FILE_LOST")
	})

	waitFor(t, "the socket to be rebound", func() bool {
		return strings.Contains(h.log.String(), "SOCKET_REBOUND")
	})

	// Without supervision the process would still look healthy here while Amp
	// could never connect again.
	next := dialBridge(t, sock)
	if err := json.NewEncoder(next).Encode(ampRequest{Text: "after"}); err != nil {
		t.Fatalf("send after rebind: %v", err)
	}
	waitFor(t, "an event over the rebound socket", func() bool {
		return len(h.notifications(t)) == 2
	})

	select {
	case <-h.b.fatalSignal():
		t.Error("a single recoverable loss must not escalate")
	default:
	}
}

func TestSupervisorStaysQuietOnDeliberateShutdown(t *testing.T) {
	t.Parallel()
	h, _ := superviseHarness(t)

	h.b.beginShutdown()
	if l := h.b.listener(); l != nil {
		_ = l.Close()
	}

	time.Sleep(100 * time.Millisecond)
	if strings.Contains(h.log.String(), "SOCKET_REBOUND") {
		t.Error("shutdown must not be mistaken for a fault and restarted")
	}
	select {
	case <-h.b.fatalSignal():
		t.Error("a clean shutdown must not escalate")
	default:
	}
}

func TestSupervisorEscalatesWhenTheFaultIsPermanent(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "sub", "b.sock")
	if err := ensureDir(t, filepath.Dir(sock)); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	h := newHarness(t)
	h.b.restartMax = 2
	h.b.restartWindow = time.Minute
	h.b.restartBackoff = time.Millisecond

	ln, err := bindSocket(sock)
	if err != nil {
		t.Fatalf("bindSocket: %v", err)
	}
	h.b.setListener(ln)

	// Remove the directory so every rebind fails: a permanent fault, not a
	// transient one. Restarting forever would hide it.
	if err := removeAll(t, filepath.Dir(sock)); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	go h.b.superviseSocket(sock)
	_ = ln.Close()

	select {
	case <-h.b.fatalSignal():
	case <-time.After(5 * time.Second):
		t.Fatal("supervision should have exhausted its budget and escalated")
	}
	if !strings.Contains(h.log.String(), "SOCKET_SUPERVISOR_EXHAUSTED") {
		t.Error("giving up must be logged, loudly enough to explain the exit code")
	}
}

func TestHandlePanicDoesNotKillTheBridge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A panic while handling one request must become an error frame, not a
	// dead channel for the rest of the session.
	if panicked := h.b.guard("handler for tools/call", func() {
		panic("simulated handler failure")
	}); !panicked {
		t.Fatal("expected the panic to be caught")
	}

	h.call(t, map[string]any{"jsonrpc": "2.0", "id": "after", "method": "ping"})
	result(t, h.response(t, "after"))
}

// ensureDir / removeAll keep the filesystem fiddling out of the test body.
func ensureDir(t *testing.T, dir string) error {
	t.Helper()
	return os.MkdirAll(dir, 0o700)
}

func removeAll(t *testing.T, dir string) error {
	t.Helper()
	return os.RemoveAll(dir)
}

// TestTransportStaysResponsiveDuringASlowToolCall guards finding 2: ask_amp can
// run for minutes, and answering it on the read loop stalled the entire
// transport — no ping, no reply from Claude, not even stdin EOF noticed.
func TestTransportStaysResponsiveDuringASlowToolCall(t *testing.T) {
	t.Parallel()
	h := ampHarness(t, `sleep 2; echo done`)

	// Start the clock BEFORE dispatching: the whole point is that neither the
	// dispatch nor the ping may wait for the 2s Amp turn. Timing the ping alone
	// would pass even with a fully serialised transport, because handle() would
	// already have blocked before the clock started.
	start := time.Now()
	h.call(t, map[string]any{
		"jsonrpc": "2.0", "id": "slow", "method": "tools/call",
		"params": map[string]any{
			"name":      toolAskAmp,
			"arguments": map[string]any{"text": "hello", "thread_id": "T-abc"},
		},
	})
	if blocked := time.Since(start); blocked > 500*time.Millisecond {
		t.Fatalf("dispatching a slow tool call held the read loop for %v", blocked)
	}

	h.call(t, map[string]any{"jsonrpc": "2.0", "id": "ping", "method": "ping"})
	result(t, h.response(t, "ping"))
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("ping answered only after %v — the transport is serialised behind ask_amp", waited)
	}

	result(t, h.response(t, "slow")) // and the slow call still completes
}
