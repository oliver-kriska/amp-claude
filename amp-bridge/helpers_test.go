package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

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
	return &harness{b: newBridge(cfg, out, lg), out: out, log: lg}
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

// response returns the single frame answering the given request id.
func (h *harness) response(t *testing.T, id any) map[string]any {
	t.Helper()
	for _, f := range h.frames(t) {
		if got := f["id"]; got != nil && equalJSON(got, id) {
			return f
		}
	}
	t.Fatalf("no response with id %v in %s", id, h.out.String())
	return nil
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
