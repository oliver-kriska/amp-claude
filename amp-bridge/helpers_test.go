package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain gives the whole binary a runtime directory of its own.
//
// The runtime dir is resolved from the process environment deep inside the call
// stack — ownRuntimeDir, reached from trustedInboxDir and friends — so a test
// that does not set AMP_BRIDGE_DIR reads the DEVELOPER'S real one. That was
// harmless while it held only sockets, and stopped being harmless once
// lookupInbox started reading live plugin registrations out of it: askAmp tests
// then consult whatever Amp sessions happen to be running on the machine, and
// their outcome depends on the state of somebody's afternoon.
//
// The same shape already reached CI once, where a --list assertion passed
// against the developer's own live session and failed the moment it ran
// somewhere clean. Setting it here means no test can reach the real directory
// by omission; a test that wants a specific one still overrides it.
func TestMain(m *testing.M) {
	// Short base path: a Unix socket path is capped near 104 bytes, and the
	// default macOS temp dir eats most of that before a filename.
	dir, err := os.MkdirTemp("/tmp", "ampb-suite")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test runtime dir:", err)
		os.Exit(1)
	}
	if err := os.Setenv("AMP_BRIDGE_DIR", dir); err != nil {
		fmt.Fprintln(os.Stderr, "set AMP_BRIDGE_DIR:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// syncBuf is an io.Writer safe to read while the bridge writes to it.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// testConfig is a fully explicit config: tests never depend on the ambient
// environment, and never mutate it.
func testConfig() config {
	return config{
		maxInFlight:     8,
		maxMessageBytes: 64 * 1024,
		replyWait:       2 * time.Second,
		maxReplyWait:    10 * time.Second,
		resultTTL:       time.Minute,
		maxResults:      8,
		logBodies:       false,
		ampBin:          "amp",
		ampTimeout:      time.Second,
		ampDisabled:     true,
	}
}

type harness struct {
	b   *bridge
	out *syncBuf
	log *syncBuf
}

func newHarness(t *testing.T, mutate ...func(*config)) *harness {
	t.Helper()
	cfg := testConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	out, lg := &syncBuf{}, &syncBuf{}
	b := newBridge(cfg, out, lg)
	// Supervision defaults are tuned for production; tests need them fast.
	b.restartBackoff = 5 * time.Millisecond
	b.socketCheck = 10 * time.Millisecond
	return &harness{b: b, out: out, log: lg}
}

// frames decodes every JSON-RPC frame the bridge has written so far.
func (h *harness) frames(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(h.out.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bridge emitted non-JSON frame %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// response returns the frame answering the given request id, waiting for it:
// tool calls are answered off the read loop, so the reply may not be written by
// the time handle() returns.
func (h *harness) response(t *testing.T, id any) map[string]any {
	t.Helper()
	// Generous: some tests wait on a subprocess, and macOS runs a security check
	// the first time it executes a freshly written script — seconds, under
	// parallel load. A deadline tuned to the fast path makes those tests flaky
	// in a way that looks like a transport bug.
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, f := range h.frames(t) {
			if got := f["id"]; got != nil && equalJSON(got, id) {
				return f
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no response with id %v in %s", id, h.out.String())
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// notifications returns every channel notification pushed so far.
func (h *harness) notifications(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, f := range h.frames(t) {
		if f["method"] == "notifications/claude/channel" {
			out = append(out, f)
		}
	}
	return out
}

func equalJSON(a, b any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// call feeds one JSON-RPC message through the handler.
func (h *harness) call(t *testing.T, msg map[string]any) {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var r rpc
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	h.b.handle(r)
}

// result returns the "result" object of a response, failing if it is an error.
func result(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	if e, ok := frame["error"]; ok {
		t.Fatalf("expected a result, got error %v", e)
	}
	res, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", frame["result"])
	}
	return res
}

// toolText flattens an MCP tool result's content blocks into one string.
func toolText(t *testing.T, res map[string]any) string {
	t.Helper()
	blocks, ok := res["content"].([]any)
	if !ok {
		t.Fatalf("tool result has no content array: %v", res)
	}
	var sb strings.Builder
	for _, blk := range blocks {
		m, ok := blk.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["text"].(string); ok {
			sb.WriteString(s)
		}
	}
	return sb.String()
}

func isToolError(t *testing.T, res map[string]any) bool {
	t.Helper()
	v, ok := res["isError"].(bool)
	return ok && v
}
