package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	errInternal       = -32603
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

	// The listener is replaceable: the supervisor rebinds it if it dies.
	lnMu sync.Mutex
	ln   net.Listener

	// downCh marks a deliberate shutdown, so the supervisor does not treat it
	// as a fault worth restarting. fatalCh is the opposite: supervision has
	// given up and the process should exit non-zero.
	downOnce  sync.Once
	downCh    chan struct{}
	fatalOnce sync.Once
	fatalCh   chan struct{}

	// Supervision knobs. Defaults are set in newBridge; tests lower them so a
	// budget can be exhausted in milliseconds rather than seconds.
	restartMax     int
	restartWindow  time.Duration
	restartBackoff time.Duration
	socketCheck    time.Duration

	// Amp turns are serialised: two concurrent `threads continue` runs against
	// one thread would interleave writes to the same conversation.
	askMu sync.Mutex

	// ctx is cancelled on shutdown. Tool calls run off the read loop, so
	// without it an `amp threads continue` subprocess outlives the bridge as an
	// orphan. toolWG lets shutdown wait for that work to unwind.
	ctx       context.Context
	cancelCtx context.CancelFunc
	toolWG    sync.WaitGroup

	// Registry entry, republished by the watchdog if it is swept. Guarded
	// because the read loop stamps initialized_at onto it while the supervisor
	// goroutine may be republishing it.
	regMu   sync.Mutex
	reg     registryEntry
	regPath string
}

// markInitialized records that Claude completed the MCP handshake.
//
// This is the only positive evidence that the channel is registered rather than
// the process merely being alive, and putting it in the registry makes it
// per-session and per-run. The alternative — grepping an append-only log for a
// marker — is satisfied forever by a line written years ago.
func (b *bridge) markInitialized() {
	b.regMu.Lock()
	if b.regPath == "" {
		b.regMu.Unlock()
		return
	}
	b.reg.InitializedAt = time.Now().UTC().Format(time.RFC3339)
	entry := b.reg
	b.regMu.Unlock()

	if _, err := entry.publish(); err != nil {
		b.logf("REGISTRY_UPDATE_FAILED %v", err)
	}
}

func (b *bridge) listener() net.Listener {
	b.lnMu.Lock()
	defer b.lnMu.Unlock()
	return b.ln
}

func (b *bridge) setListener(ln net.Listener) {
	b.lnMu.Lock()
	b.ln = ln
	b.lnMu.Unlock()
}

func (b *bridge) beginShutdown() {
	b.downOnce.Do(func() {
		close(b.downCh)
		b.cancelCtx() // kills any in-flight Amp subprocess
	})
}

func (b *bridge) shuttingDown() bool {
	select {
	case <-b.downCh:
		return true
	default:
		return false
	}
}

// escalate is the supervisor giving up: the fault is permanent, so surface it
// rather than limping on as a bridge Amp cannot reach.
func (b *bridge) escalate() { b.fatalOnce.Do(func() { close(b.fatalCh) }) }

func (b *bridge) fatalSignal() <-chan struct{} { return b.fatalCh }

func newBridge(cfg config, out, logw io.Writer) *bridge {
	b := &bridge{
		cfg:     cfg,
		out:     bufio.NewWriter(out),
		logw:    logw,
		pending: make(map[string]chan string),
		downCh:  make(chan struct{}),
		fatalCh: make(chan struct{}),

		restartMax:     5,
		restartWindow:  time.Minute,
		restartBackoff: 100 * time.Millisecond,
		socketCheck:    5 * time.Second,
	}
	b.ctx, b.cancelCtx = context.WithCancel(context.Background())
	return b
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

// send returns a transport error rather than only logging it: an unsolicited
// event that never reached Claude must unwind its pending slot, or the Amp
// caller blocks for the full timeout waiting on a message nobody received.
func (b *bridge) send(msg rpc) error {
	msg.JSONRPC = "2.0"
	data, err := json.Marshal(msg)
	if err != nil {
		b.logf("MARSHAL_ERROR %v", err)
		return fmt.Errorf("marshal %s: %w", msg.Method, err)
	}

	b.outMu.Lock()
	_, werr := b.out.Write(data)
	if werr == nil {
		werr = b.out.WriteByte('\n')
	}
	if werr == nil {
		werr = b.out.Flush()
	}
	b.outMu.Unlock()

	if werr != nil {
		b.logf("SEND_ERROR method=%s %v", msg.Method, werr)
		return fmt.Errorf("write %s: %w", msg.Method, werr)
	}
	b.logFrame("<<SENT", msg.Method, msg.ID, data)
	return nil
}

// reply and fail answer a request Claude is already waiting on; a transport
// failure there is logged by send and there is nowhere better to report it.
func (b *bridge) reply(id json.RawMessage, result any) { _ = b.send(rpc{ID: id, Result: result}) }

func (b *bridge) fail(id json.RawMessage, code int, message string) {
	_ = b.send(rpc{ID: id, Error: &rpcError{Code: code, Message: message}})
}

// toolResult builds the MCP content envelope every tool call answers with.
func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// ── dispatch ────────────────────────────────────────────────────────────────

// handle dispatches one JSON-RPC message. A panic in any handler is contained
// here: killing the process would take the channel down for the whole session
// over one malformed request.
func (b *bridge) handle(msg rpc) {
	if panicked := b.guard("handler for "+msg.Method, func() { b.dispatch(msg) }); panicked {
		if len(msg.ID) > 0 {
			b.fail(msg.ID, errInternal, "internal error handling "+msg.Method)
		}
	}
}

func (b *bridge) dispatch(msg rpc) {
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
		b.markInitialized()

	case "tools/list":
		b.reply(msg.ID, map[string]any{"tools": []any{replyTool, askAmpTool}})

	case "tools/call":
		// Off the read loop. ask_amp shells out to the Amp CLI for up to five
		// minutes; handling it inline stalls the whole transport — no ping, no
		// reply from Claude, not even stdin EOF noticed. JSON-RPC correlates by
		// id and send is mutex-guarded, so answering out of order is fine.
		b.toolWG.Go(func() {
			if panicked := b.guard("tools/call", func() { b.handleToolsCall(msg) }); panicked {
				if len(msg.ID) > 0 {
					b.fail(msg.ID, errInternal, "internal error handling tools/call")
				}
			}
		})

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

	if strings.TrimSpace(a.Text) == "" {
		b.reply(id, toolResult("text is empty — Amp would receive a blank answer. "+
			"Send the actual reply body.", true))
		return
	}

	res, rid := b.resolve(a.RequestID, a.Text)
	b.logf("REPLY_TOOL request_id=%q outcome=%s bytes=%d", a.RequestID, res, len(a.Text))

	switch res {
	case resolveOK:
		b.reply(id, toolResult("delivered to Amp (request_id "+rid+")", false))
	case resolveMissingID:
		b.reply(id, toolResult("request_id is required. The reply was not routed and no one "+
			"received it. Call reply again, passing the request_id attribute from the "+
			"<channel> event you are answering.", true))
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
