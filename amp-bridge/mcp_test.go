package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// The protocol contract. Each of these guards a mistake that cost real debugging
// time; the research doc explains the history.

func TestServerDiscoverIsDeclined(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.call(t, map[string]any{
		"jsonrpc": "2.0", "id": "d1", "method": "server/discover",
		"params": map[string]any{
			"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28"},
		},
	})

	frame := h.response(t, "d1")
	errObj, ok := frame["error"].(map[string]any)
	if !ok {
		t.Fatalf("server/discover must fail so the connection stays on the legacy "+
			"handshake; got %v", frame)
	}
	if code, _ := errObj["code"].(float64); int(code) != errMethodNotFound {
		t.Errorf("code = %v, want %d", errObj["code"], errMethodNotFound)
	}
	if !strings.Contains(h.log.String(), "SERVER_DISCOVER_DECLINED") {
		t.Error("declining server/discover should be logged; it is easy to mistake for a bug")
	}
}

func TestInitializeCapabilities(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.call(t, map[string]any{
		"jsonrpc": "2.0", "id": 0, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"clientInfo":      map[string]any{"name": "claude-code", "version": "2.1.235"},
		},
	})

	res := result(t, h.response(t, 0))

	caps, ok := res["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("no capabilities object: %v", res)
	}
	exp, ok := caps["experimental"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities.experimental missing: %v", caps)
	}
	if _, ok := exp["claude/channel"]; !ok {
		t.Errorf("experimental['claude/channel'] is what registers the channel listener; "+
			"got %v", exp)
	}
	if _, ok := caps["claude/channel"]; ok {
		t.Error("claude/channel must NOT be a top-level capability key")
	}
	if _, ok := caps["tools"]; !ok {
		t.Error("tools capability missing")
	}
	if got := res["protocolVersion"]; got != "2025-11-25" {
		t.Errorf("protocolVersion = %v, want the client's own 2025-11-25 echoed back", got)
	}
	if s, _ := res["instructions"].(string); s == "" {
		t.Error("instructions are injected into Claude's system prompt and must be set")
	}
}

func TestInitializeProtocolFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"client names a version", map[string]any{"protocolVersion": "2025-06-18"}, "2025-06-18"},
		{"client names none", map[string]any{}, legacyProtocol},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.call(t, map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": tc.params,
			})
			if got := result(t, h.response(t, 1))["protocolVersion"]; got != tc.want {
				t.Errorf("protocolVersion = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInitializeSurvivesJunkParams(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.b.handle(rpc{ID: json.RawMessage(`7`), Method: "initialize", Params: json.RawMessage(`"not an object"`)})

	res := result(t, h.response(t, 7))
	if got := res["protocolVersion"]; got != legacyProtocol {
		t.Errorf("a malformed initialize should still complete on defaults; got %v", got)
	}
}

func TestToolsList(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.call(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})

	tools, ok := result(t, h.response(t, 1))["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list returned no array")
	}
	byName := map[string]map[string]any{}
	for _, tool := range tools {
		m, ok := tool.(map[string]any)
		if !ok {
			t.Fatalf("tool entry is not an object: %v", tool)
		}
		name, _ := m["name"].(string)
		byName[name] = m
	}

	// Both directions must be exposed or the bridge is only half-duplex.
	for _, want := range []string{toolReply, toolAskAmp, toolSendAmp} {
		if _, ok := byName[want]; !ok {
			t.Errorf("tool %q not exposed; have %v", want, keys(byName))
		}
	}
	if len(byName) != 3 {
		t.Errorf("unexpected tools exposed: %v", keys(byName))
	}

	req := requiredFields(t, byName[toolReply])
	if !req["text"] || !req["request_id"] {
		t.Errorf("reply must require both text and request_id, got %v", req)
	}
	if r := requiredFields(t, byName[toolAskAmp]); !r["text"] {
		t.Errorf("ask_amp must require text, got %v", r)
	}
	if r := requiredFields(t, byName[toolSendAmp]); !r["text"] || !r["thread_id"] {
		t.Errorf("send_amp must require text and thread_id, got %v", r)
	}
}

func requiredFields(t *testing.T, tool map[string]any) map[string]bool {
	t.Helper()
	schema, ok := tool["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("tool has no inputSchema: %v", tool)
	}
	req, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("inputSchema has no required list: %v", schema)
	}
	out := map[string]bool{}
	for _, r := range req {
		if s, ok := r.(string); ok {
			out[s] = true
		}
	}
	return out
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestUnknownToolIsAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.call(t, map[string]any{
		"jsonrpc": "2.0", "id": "junk", "method": "tools/call",
		"params": map[string]any{"name": "nope", "arguments": map[string]any{}},
	})
	if _, ok := h.response(t, "junk")["error"]; !ok {
		t.Error("an unknown tool must be a JSON-RPC error, not a silent success")
	}
}

func TestUnknownMethodIsAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.call(t, map[string]any{"jsonrpc": "2.0", "id": "u", "method": "does/not/exist"})
	if _, ok := h.response(t, "u")["error"]; !ok {
		t.Error("unknown request methods must be answered with an error")
	}
}

func TestUnknownNotificationIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// A notification has no id and must never be answered, even when unknown.
	h.call(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/something/new"})
	if got := h.out.String(); got != "" {
		t.Errorf("a notification must never be answered; bridge wrote %q", got)
	}
}

func TestTrivialMethods(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method string
		field  string
	}{
		{"resources/list", "resources"},
		{"prompts/list", "prompts"},
	}
	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.call(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": tc.method})
			if _, ok := result(t, h.response(t, 1))[tc.field]; !ok {
				t.Errorf("%s must return a %s array", tc.method, tc.field)
			}
		})
	}
}

func TestPing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.call(t, map[string]any{"jsonrpc": "2.0", "id": "p", "method": "ping"})
	result(t, h.response(t, "p"))
}

func TestAskAmpFailsCleanlyWhenDisabled(t *testing.T) {
	t.Parallel()
	h := newHarness(t) // testConfig disables outbound

	h.call(t, map[string]any{
		"jsonrpc": "2.0", "id": "aa", "method": "tools/call",
		"params": map[string]any{
			"name":      toolAskAmp,
			"arguments": map[string]any{"text": "hello amp"},
		},
	})

	res := result(t, h.response(t, "aa"))
	if !isToolError(t, res) {
		t.Errorf("ask_amp must report a tool error, not a success: %v", res)
	}
	if !strings.Contains(toolText(t, res), "disabled") {
		t.Errorf("the error should say why: %q", toolText(t, res))
	}
}

func TestAskAmpWithoutThreadExplainsItself(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config) { c.ampDisabled = false })

	h.call(t, map[string]any{
		"jsonrpc": "2.0", "id": "aa", "method": "tools/call",
		"params": map[string]any{
			"name":      toolAskAmp,
			"arguments": map[string]any{"text": "hello"},
		},
	})

	res := result(t, h.response(t, "aa"))
	if !isToolError(t, res) || !strings.Contains(toolText(t, res), "thread_id") {
		t.Errorf("with no known thread, ask_amp should explain how to supply one: %v", res)
	}
}

func TestSendAmpRequiresAnExplicitThread(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config) { c.ampDisabled = false })

	h.call(t, map[string]any{
		"jsonrpc": "2.0", "id": "sa", "method": "tools/call",
		"params": map[string]any{
			"name":      toolSendAmp,
			"arguments": map[string]any{"text": "background work"},
		},
	})

	res := result(t, h.response(t, "sa"))
	if !isToolError(t, res) || !strings.Contains(toolText(t, res), "thread_id is required") {
		t.Errorf("send_amp must refuse implicit routing: %v", res)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("transport closed") }

func TestSendAmpDoesNotStartWhenTheHandleCannotBeDelivered(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.ampDisabled = false
	var log syncBuf
	b := newBridge(cfg, failingWriter{}, &log)

	b.handleSendAmp(json.RawMessage(`"ack"`), mustJSON(map[string]any{
		"text": "do not run invisibly", "thread_id": "T-ack-failure",
	}))

	if got := len(b.asyncSlots); got != 0 {
		t.Errorf("failed acknowledgement leaked %d async slot(s)", got)
	}
	if !strings.Contains(log.String(), "SEND_AMP_ACK_FAILED") || strings.Contains(log.String(), "SEND_AMP_STARTED") {
		t.Errorf("log should say the request was not started: %s", log.String())
	}
}

func TestTruncateMessagePreservesUTF8AndTheByteCap(t *testing.T) {
	t.Parallel()
	got := truncateMessage("useful\xff"+strings.Repeat("é", 20), 25)
	if len(got) > 25 || !utf8.ValidString(got) || !strings.Contains(got, "[truncated]") {
		t.Errorf("truncateMessage = %q (%d bytes)", got, len(got))
	}
	if !strings.Contains(got, "useful") {
		t.Errorf("an invalid byte discarded valid output: %q", got)
	}
}

func TestMalformedToolCallParams(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.b.handle(rpc{ID: json.RawMessage(`"m"`), Method: "tools/call", Params: json.RawMessage(`[]`)})
	if _, ok := h.response(t, "m")["error"]; !ok {
		t.Error("malformed tools/call params must produce an error, not a panic or silence")
	}
}

func TestDecodeArgsToleratesMissingArguments(t *testing.T) {
	t.Parallel()
	var into struct {
		Text string `json:"text"`
	}
	for _, raw := range []string{"", "null", "  "} {
		if err := decodeArgs(json.RawMessage(raw), &into); err != nil {
			t.Errorf("decodeArgs(%q) = %v, want nil", raw, err)
		}
	}
	if err := decodeArgs(json.RawMessage(`{"text":"x"}`), &into); err != nil || into.Text != "x" {
		t.Errorf("decodeArgs did not populate the target: %v %v", err, into)
	}
}

func TestLogRedactsBodiesByDefault(t *testing.T) {
	t.Parallel()
	secret := "the-quick-brown-fox-secret"

	t.Run("off", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.b.logFrame(">>RECV", "tools/call", json.RawMessage(`1`), []byte(secret))
		if strings.Contains(h.log.String(), secret) {
			t.Error("frame bodies carry conversation text and must stay out of the log by default")
		}
		if !strings.Contains(h.log.String(), "tools/call") {
			t.Error("frame shape should still be logged")
		}
	})

	t.Run("on", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, func(c *config) { c.logBodies = true })
		h.b.logFrame(">>RECV", "tools/call", json.RawMessage(`1`), []byte(secret))
		if !strings.Contains(h.log.String(), secret) {
			t.Error("AMP_BRIDGE_LOG_BODIES=1 should log the body")
		}
	})
}

func TestServeStdio(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A malformed line in the middle must not take the transport down with it.
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`this is not json`,
		``,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n")

	h.b.serveStdio(strings.NewReader(in))

	result(t, h.response(t, 1))
	if _, ok := result(t, h.response(t, 2))["tools"]; !ok {
		t.Error("the transport stopped processing after a malformed line")
	}
	if !strings.Contains(h.log.String(), "PARSE_ERROR") {
		t.Error("a malformed line should be logged")
	}
	if !strings.Contains(h.log.String(), "stdin closed") {
		t.Error("the bridge should log why it stopped; Claude closes stdin to shut it down")
	}
}

func TestServeStdioHandlesAFinalLineWithoutNewline(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// ReadString returns the last fragment together with io.EOF.
	h.b.serveStdio(strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"ping"}`))
	result(t, h.response(t, 9))
}
