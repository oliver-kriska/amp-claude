package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// MCP stdio transport and request handling.
//
// Wire contract verified against Anthropic's own scaffold
// (`claude plugin init <name> --with channel`) and
// https://code.claude.com/docs/en/channels-reference. The three load-bearing
// details are documented at their call sites: no server/discover (see handle),
// the experimental capability key (see handleInitialize), and `meta` rather
// than `_meta` (see pushEvent).

const (
	serverName    = "amp-bridge"
	serverVersion = "0.3.0"

	// legacyProtocol is what Claude Code offers on the fallback `initialize`
	// handshake. Used only when the client names no version at all.
	legacyProtocol = "2025-11-25"

	toolReply  = "reply"
	toolAskAmp = "ask_amp"
)

// JSON-RPC error codes we use (per the spec).
const (
	errMethodNotFound = -32601
	errInvalidParams  = -32602
)

var instruction = "Events from " + serverName + " arrive as <channel source=\"" + serverName +
	"\" request_id=\"...\">. Anything you want the sender to see must go through the " +
	"reply tool — your transcript output never reaches the channel. ALWAYS pass the " +
	"request_id attribute from the event you are answering when you call reply."

type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type bridge struct {
	cfg config

	out   *bufio.Writer
	outMu sync.Mutex

	logMu sync.Mutex
	logw  io.Writer

	pendMu  sync.Mutex
	pending map[string]chan string
	seq     int

	threadMu   sync.Mutex
	lastThread string
}

func newBridge(cfg config, out, logw io.Writer) *bridge {
	return &bridge{
		cfg:     cfg,
		out:     bufio.NewWriter(out),
		logw:    logw,
		pending: make(map[string]chan string),
	}
}

func (b *bridge) logf(format string, args ...any) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	fmt.Fprintf(b.logw, "[%s] %s\n",
		time.Now().UTC().Format(time.RFC3339Nano),
		fmt.Sprintf(format, args...))
	// Flush to disk so a crashed session still leaves a usable trail.
	if f, ok := b.logw.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
}

// logFrame records protocol traffic. By default it logs shape, not content:
// conversation text stays out of the log unless explicitly enabled.
func (b *bridge) logFrame(dir, method string, id json.RawMessage, raw []byte) {
	if b.cfg.logBodies {
		b.logf("%s %s", dir, string(raw))
		return
	}
	if method == "" {
		method = "(response)"
	}
	b.logf("%s method=%s id=%s bytes=%d", dir, method, strings.TrimSpace(string(id)), len(raw))
}

func (b *bridge) send(msg rpc) {
	msg.JSONRPC = "2.0"
	data, err := json.Marshal(msg)
	if err != nil {
		b.logf("MARSHAL_ERROR %v", err)
		return
	}
	b.outMu.Lock()
	_, _ = b.out.Write(data)
	_ = b.out.WriteByte('\n')
	err = b.out.Flush()
	b.outMu.Unlock()
	if err != nil {
		b.logf("SEND_ERROR method=%s %v", msg.Method, err)
		return
	}
	b.logFrame("<<SENT", msg.Method, msg.ID, data)
}

func (b *bridge) reply(id json.RawMessage, result any) { b.send(rpc{ID: id, Result: result}) }

func (b *bridge) fail(id json.RawMessage, code int, message string) {
	b.send(rpc{ID: id, Error: &rpcError{Code: code, Message: message}})
}

// toolResult builds the MCP content envelope every tool call answers with.
func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// ── dispatch ────────────────────────────────────────────────────────────────

func (b *bridge) handle(msg rpc) {
	switch msg.Method {

	// Deliberately unhandled. Claude Code negotiates in two phases: it probes
	// the modern discovery handshake first, and falls back to the legacy
	// `initialize` one only if the server does not answer. Channels have a
	// delivery path for unsolicited notifications ONLY on the legacy handshake,
	// so answering this would silently kill delivery while every health check
	// still passed. Failing it is the feature.
	case "server/discover":
		b.logf("SERVER_DISCOVER_DECLINED (intentional — keeps us on the legacy handshake)")
		b.fail(msg.ID, errMethodNotFound, "server/discover not supported")

	case "initialize":
		b.handleInitialize(msg)

	case "notifications/initialized":
		b.logf("CLIENT_INITIALIZED — channel listener should now be registered")

	case "tools/list":
		b.reply(msg.ID, map[string]any{"tools": []any{replyTool, askAmpTool}})

	case "tools/call":
		b.handleToolsCall(msg)

	case "ping":
		b.reply(msg.ID, map[string]any{})
	case "resources/list":
		b.reply(msg.ID, map[string]any{"resources": []any{}})
	case "prompts/list":
		b.reply(msg.ID, map[string]any{"prompts": []any{}})

	default:
		// Notifications carry no id and must never be answered.
		if strings.HasPrefix(msg.Method, "notifications/") {
			return
		}
		if len(msg.ID) > 0 {
			b.fail(msg.ID, errMethodNotFound, "method not found: "+msg.Method)
		}
	}
}

func (b *bridge) handleInitialize(msg rpc) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      any    `json:"clientInfo"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		// Not fatal: we can still complete the handshake on defaults.
		b.logf("INITIALIZE_PARAMS_UNPARSEABLE %v", err)
	}
	b.logf("INITIALIZE client=%s proto=%s", jsonStr(p.ClientInfo), p.ProtocolVersion)

	// Echo whatever the client offered. Forcing a version here was an early
	// mistake: the fallback handshake Claude reaches us on is already correct,
	// and overriding it only risks a mismatch.
	negotiated := p.ProtocolVersion
	if negotiated == "" {
		negotiated = legacyProtocol
	}
	b.reply(msg.ID, map[string]any{
		"protocolVersion": negotiated,
		"capabilities": map[string]any{
			"tools": map[string]any{},
			// The presence of this experimental key is what registers the
			// channel notification listener. It is NOT a top-level capability.
			"experimental": map[string]any{"claude/channel": map[string]any{}},
		},
		"serverInfo":   map[string]any{"name": serverName, "version": serverVersion},
		"instructions": instruction,
	})
}

func (b *bridge) handleToolsCall(msg rpc) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		b.fail(msg.ID, errInvalidParams, "invalid tools/call params: "+err.Error())
		return
	}
	switch p.Name {
	case toolAskAmp:
		b.handleAskAmp(msg.ID, p.Arguments)
	case toolReply:
		b.handleReply(msg.ID, p.Arguments)
	default:
		b.fail(msg.ID, errMethodNotFound, "unknown tool: "+p.Name)
	}
}

func (b *bridge) handleReply(id, args json.RawMessage) {
	var a struct {
		Text      string `json:"text"`
		RequestID string `json:"request_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		b.fail(id, errInvalidParams, "invalid reply arguments: "+err.Error())
		return
	}

	res, rid := b.resolve(a.RequestID, a.Text)
	b.logf("REPLY_TOOL request_id=%q outcome=%s bytes=%d", a.RequestID, res, len(a.Text))

	switch res {
	case resolveOK:
		b.reply(id, toolResult("delivered to Amp (request_id "+rid+")", false))
	case resolveAmbiguous:
		b.reply(id, toolResult("request_id is required: more than one Amp request is in "+
			"flight, so the reply could not be routed. Call reply again with the request_id "+
			"attribute from the <channel> event you are answering.", true))
	default:
		b.reply(id, toolResult("no pending Amp request matches request_id "+rid+
			" — it may have already timed out. Reply dropped.", true))
	}
}

func (b *bridge) handleAskAmp(id, args json.RawMessage) {
	var a struct {
		Text     string `json:"text"`
		ThreadID string `json:"thread_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		b.fail(id, errInvalidParams, "invalid ask_amp arguments: "+err.Error())
		return
	}
	b.logf("ASK_AMP thread=%q bytes=%d", a.ThreadID, len(a.Text))

	out, err := b.askAmp(a.ThreadID, a.Text)
	if err != nil {
		b.logf("ASK_AMP_FAILED %v", err)
		b.reply(id, toolResult(err.Error(), true))
		return
	}
	b.logf("ASK_AMP_OK bytes=%d", len(out))
	b.reply(id, toolResult(out, false))
}

// decodeArgs tolerates a missing or null arguments member, which some clients
// omit entirely for a no-argument call.
func decodeArgs(args json.RawMessage, into any) error {
	s := strings.TrimSpace(string(args))
	if s == "" || s == "null" {
		return nil
	}
	return json.Unmarshal(args, into)
}

// ── tool declarations ───────────────────────────────────────────────────────

var replyTool = map[string]any{
	"name": toolReply,
	"description": "Send a message back to the Amp thread that raised a channel event. " +
		"This is the only way to reach the sender — your transcript output never leaves " +
		"this session. Pass the request_id from the <channel> event you are answering.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "The reply body."},
			"request_id": map[string]any{
				"type":        "string",
				"description": "request_id attribute of the <channel> event being answered.",
			},
		},
		"required": []string{"text", "request_id"},
	},
}

var askAmpTool = map[string]any{
	"name": toolAskAmp,
	"description": "Send a message to the Amp thread on the other side of this bridge and wait " +
		"for its answer. Use this to ASK Amp something or hand it work — it is the only way " +
		"to start a conversation with Amp, since the reply tool can only answer an event Amp " +
		"already sent. Blocks until Amp finishes its turn.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{
				"type":        "string",
				"description": "What to say to the Amp thread.",
			},
			"thread_id": map[string]any{
				"type": "string",
				"description": "Amp thread id. Optional — defaults to the thread that last " +
					"messaged this bridge.",
			},
		},
		"required": []string{"text"},
	},
}

// ── helpers ─────────────────────────────────────────────────────────────────

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func jsonStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return string(b)
}
