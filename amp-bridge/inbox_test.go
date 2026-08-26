package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakePlugin stands in for the Amp plugin: it serves the inbox protocol over a
// Unix socket, one operation per connection, and records the ask frames it saw.
//
// Being pure Go, it lets every routing, sweep and error-mapping path be tested
// without Amp, a live session, or a human at a keyboard — which is the whole
// point of keeping tier one separate from tier three.
type fakePlugin struct {
	t         *testing.T
	sock      string
	ln        net.Listener
	threads   []string
	proto     int
	reply     inboxReply // what to answer an ask with
	hangup    bool       // close without answering, to test EOF mid-request
	replyGate <-chan struct{}
	askFrame  chan []byte
}

func newFakePlugin(t *testing.T, threads ...string) *fakePlugin {
	t.Helper()
	dir := shortTempDir(t)
	p := &fakePlugin{
		t:        t,
		sock:     filepath.Join(dir, "p.sock"),
		threads:  threads,
		proto:    inboxProto,
		reply:    inboxReply{Reply: "from amp"},
		askFrame: make(chan []byte, 8),
	}
	ln, err := net.Listen("unix", p.sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p.ln = ln
	go p.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *fakePlugin) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(conn)
	}
}

func (p *fakePlugin) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	dec := json.NewDecoder(conn)
	var raw json.RawMessage
	if dec.Decode(&raw) != nil {
		return
	}
	var op struct {
		Op string `json:"op"`
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &op) != nil {
		return
	}
	switch op.Op {
	case "status":
		_ = json.NewEncoder(conn).Encode(inboxStatusReply{
			PID: os.Getpid(), Proto: p.proto, EnabledThreads: p.threads,
		})
	case "ask":
		select {
		case p.askFrame <- raw:
		default:
		}
		if p.hangup {
			return // close without answering
		}
		if p.replyGate != nil {
			<-p.replyGate
		}
		r := p.reply
		r.ID = op.ID
		_ = json.NewEncoder(conn).Encode(r)
	}
}

// inboxHarness builds a bridge whose runtime dir is a private temp tree, and
// optionally registers one plugin entry for threadID.
func inboxHarness(t *testing.T, p *fakePlugin, threadID string, mutate ...func(*config)) *harness {
	t.Helper()
	runtime := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", runtime)
	if err := os.Chmod(runtime, 0o700); err != nil {
		t.Fatalf("chmod runtime: %v", err)
	}
	if p != nil {
		threads := filepath.Join(runtime, "inbox", "threads")
		if err := os.MkdirAll(threads, 0o700); err != nil {
			t.Fatalf("mkdir threads: %v", err)
		}
		e := inboxEntry{
			ThreadID: threadID, Socket: p.sock, PluginPID: os.Getpid(),
			Proto: inboxProto, EnabledAt: "now", Note: "test",
		}
		data, _ := json.Marshal(e)
		if err := os.WriteFile(filepath.Join(threads, threadID+".json"), data, 0o600); err != nil {
			t.Fatalf("write entry: %v", err)
		}
	}
	// A fail-fast amp: if the CLI path is taken when it should not be, the test
	// sees a distinctive error rather than a silent success.
	bin := fakeAmp(t, `echo "CLI-WAS-SPAWNED" >&2; exit 9`)
	base := func(c *config) {
		c.ampDisabled = false
		c.ampBin = bin
		c.ampTimeout = 60 * time.Second
	}
	return newHarness(t, append([]func(*config){base}, mutate...)...)
}

func TestInboxLookupHitAndAsk(t *testing.T) {
	p := newFakePlugin(t, "T-open")
	h := inboxHarness(t, p, "T-open")

	out, err := h.b.askAmp("T-open", "how goes it?")
	if err != nil {
		t.Fatalf("askAmp via inbox: %v", err)
	}
	if out != "from amp" {
		t.Errorf("reply = %q, want %q", out, "from amp")
	}

	// The CLI must not have been touched: this thread is by construction open.
	if strings.Contains(h.log.String(), "CLI-WAS-SPAWNED") {
		t.Error("the CLI path was taken even though a live inbox existed")
	}

	// Golden: the ask frame carries exactly the documented fields.
	select {
	case frame := <-p.askFrame:
		var got map[string]any
		if err := json.Unmarshal(frame, &got); err != nil {
			t.Fatalf("ask frame is not JSON: %v", err)
		}
		for _, k := range []string{"op", "id", "thread_id", "text", "from", "timeout_ms"} {
			if _, ok := got[k]; !ok {
				t.Errorf("ask frame missing %q: %s", k, frame)
			}
		}
		if got["op"] != "ask" {
			t.Errorf("op = %v, want ask", got["op"])
		}
		if id, _ := got["id"].(string); len(id) != 12 {
			t.Errorf("id = %q, want 12 hex chars", id)
		}
		// The plugin's budget must sit inside ours, or its diagnosis loses the race.
		if ms, _ := got["timeout_ms"].(float64); ms >= float64(h.b.cfg.ampTimeout.Milliseconds()) {
			t.Errorf("timeout_ms %v must be less than ampTimeout %v", ms, h.b.cfg.ampTimeout)
		}
	default:
		t.Fatal("plugin never received an ask frame")
	}
}

func TestInboxRequestsAreNotSerializedByTheCLIMutex(t *testing.T) {
	gate := make(chan struct{})
	p := newFakePlugin(t, "T-open")
	p.replyGate = gate
	h := inboxHarness(t, p, "T-open")

	type result struct {
		out string
		err error
	}
	results := make(chan result, 2)
	for _, text := range []string{"first", "second"} {
		go func(text string) {
			out, err := h.b.askAmp("T-open", text)
			results <- result{out, err}
		}(text)
	}

	for i := range 2 {
		select {
		case <-p.askFrame:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d inbox request(s) arrived before the first completed; a Go-side mutex is serialising the plugin's own queue", i)
		}
	}
	close(gate)
	for range 2 {
		if got := <-results; got.err != nil || got.out != "from amp" {
			t.Errorf("askAmp = %q, %v", got.out, got.err)
		}
	}
}

func TestSendAmpReturnsImmediatelyAndPushesACompletion(t *testing.T) {
	p := newFakePlugin(t, "T-open")
	h := inboxHarness(t, p, "T-open")
	h.b.rememberThread("T-paired")

	h.call(t, sendAmpCall("sa", "T-open", "do the review"))
	res := result(t, h.response(t, "sa"))
	if isToolError(t, res) || !strings.Contains(toolText(t, res), "amp-async-") {
		t.Fatalf("send_amp did not return an async handle: %v", res)
	}
	waitFor(t, "the async completion event", func() bool { return len(h.notifications(t)) == 1 })

	params, _ := h.notifications(t)[0]["params"].(map[string]any)
	meta, _ := params["meta"].(map[string]any)
	if meta["status"] != "done" || meta["thread_id"] != "T-open" {
		t.Errorf("completion metadata = %v", meta)
	}
	if id, _ := meta["async_id"].(string); !strings.HasPrefix(id, "amp-async-") {
		t.Errorf("async_id = %v", meta["async_id"])
	}
	if _, ok := meta["request_id"]; ok {
		t.Error("a completion must not carry request_id; no Amp caller is waiting for reply")
	}
	if content, _ := params["content"].(string); !strings.Contains(content, "from amp") || !strings.Contains(content, "No reply is required") {
		t.Errorf("completion content = %q", content)
	}
	if h.b.pendingCount() != 0 {
		t.Errorf("send_amp allocated a reply slot; pending = %d", h.b.pendingCount())
	}
	if got := h.b.knownThread(); got != "T-paired" {
		t.Errorf("send_amp rebound the default thread to %q", got)
	}
}

func TestSendAmpPushesFailuresAsCompletionEvents(t *testing.T) {
	p := newFakePlugin(t, "T-open")
	p.reply = inboxReply{Error: "Amp turn failed", Code: "turn-error"}
	h := inboxHarness(t, p, "T-open")

	h.call(t, sendAmpCall("sa", "T-open", "do the review"))
	if res := result(t, h.response(t, "sa")); isToolError(t, res) {
		t.Fatalf("accepted async work should return a handle, not the later failure: %v", res)
	}
	waitFor(t, "the async failure event", func() bool { return len(h.notifications(t)) == 1 })
	params, _ := h.notifications(t)[0]["params"].(map[string]any)
	meta, _ := params["meta"].(map[string]any)
	if meta["status"] != "error" {
		t.Errorf("failure status = %v", meta["status"])
	}
	if content, _ := params["content"].(string); !strings.Contains(content, "failed") || !strings.Contains(content, "check thread") {
		t.Errorf("failure content = %q", content)
	}
}

func TestSendAmpBoundsCompletionContent(t *testing.T) {
	p := newFakePlugin(t, "T-open")
	p.reply = inboxReply{Reply: strings.Repeat("é", 200)}
	h := inboxHarness(t, p, "T-open", func(c *config) { c.maxMessageBytes = 128 })

	h.call(t, sendAmpCall("bounded", "T-open", "return a large result"))
	if res := result(t, h.response(t, "bounded")); isToolError(t, res) {
		t.Fatalf("send_amp was rejected: %v", res)
	}
	waitFor(t, "the bounded completion event", func() bool { return len(h.notifications(t)) == 1 })

	params, _ := h.notifications(t)[0]["params"].(map[string]any)
	content, _ := params["content"].(string)
	if len(content) > h.b.cfg.maxMessageBytes {
		t.Errorf("completion is %d bytes, max is %d", len(content), h.b.cfg.maxMessageBytes)
	}
	if !strings.Contains(content, "[truncated]") || !utf8.ValidString(content) {
		t.Errorf("completion was not safely truncated: %q", content)
	}
	if !strings.Contains(h.log.String(), "SEND_AMP_COMPLETION_TRUNCATED") {
		t.Error("truncation should log the original size")
	}
}

func TestSendAmpIsBoundedAndShutdownCancelsIt(t *testing.T) {
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	p := newFakePlugin(t, "T-open")
	p.replyGate = gate
	h := inboxHarness(t, p, "T-open", func(c *config) { c.maxInFlight = 1 })

	h.call(t, sendAmpCall("first", "T-open", "long work"))
	if res := result(t, h.response(t, "first")); isToolError(t, res) {
		t.Fatalf("first send_amp was rejected: %v", res)
	}
	select {
	case <-p.askFrame:
	case <-time.After(5 * time.Second):
		t.Fatal("first async request never reached the plugin")
	}

	h.call(t, sendAmpCall("second", "T-open", "too much"))
	second := result(t, h.response(t, "second"))
	if !isToolError(t, second) || !strings.Contains(toolText(t, second), "too many send_amp") {
		t.Errorf("second request should hit the cap: %v", second)
	}

	start := time.Now()
	h.b.drainToolWork()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("shutdown took %v; the inbox connection did not react to cancellation", elapsed)
	}
	if n := len(h.b.asyncSlots); n != 0 {
		t.Errorf("shutdown leaked %d async slot(s)", n)
	}
	if strings.Contains(h.log.String(), "SHUTDOWN_TIMEOUT") {
		t.Error("async work did not unwind before the shutdown bound")
	}
}

func sendAmpCall(id, threadID, text string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{
			"name":      toolSendAmp,
			"arguments": map[string]any{"text": text, "thread_id": threadID},
		},
	}
}

// No entry at all: the bridge must behave exactly as it did before inboxes
// existed, i.e. spawn the CLI.
func TestInboxAbsenceFallsThroughToCLI(t *testing.T) {
	h := inboxHarness(t, nil, "")

	_, err := h.b.askAmp("T-quiet", "hello")
	if err == nil {
		t.Fatal("the fail-fast CLI stub must surface an error")
	}
	if !strings.Contains(err.Error(), "CLI-WAS-SPAWNED") {
		t.Errorf("expected the CLI path, got %v", err)
	}
}

// A dead socket means the Amp session went away. The entry is swept and the
// caller degrades to the CLI rather than hanging.
func TestInboxStaleEntryIsSweptThenFallsBack(t *testing.T) {
	p := newFakePlugin(t, "T-open")
	h := inboxHarness(t, p, "T-open")
	_ = p.ln.Close() // the session exits without running onDispose

	_, err := h.b.askAmp("T-open", "hello")
	if err == nil || !strings.Contains(err.Error(), "CLI-WAS-SPAWNED") {
		t.Fatalf("a stale inbox must fall back to the CLI, got %v", err)
	}
	entry := filepath.Join(os.Getenv("AMP_BRIDGE_DIR"), "inbox", "threads", "T-open.json")
	if _, err := os.Stat(entry); err == nil {
		t.Error("the stale entry was not swept")
	}
	if !strings.Contains(h.log.String(), "INBOX_STALE") {
		t.Error("the sweep was not logged")
	}
}

// Live socket, but the plugin is not serving this thread — a disable race, or a
// reused pid. Must be an absence, never a misdelivery.
func TestInboxStatusMismatchDoesNotDeliver(t *testing.T) {
	p := newFakePlugin(t, "T-someone-else")
	h := inboxHarness(t, p, "T-open")

	_, err := h.b.askAmp("T-open", "hello")
	if err == nil || !strings.Contains(err.Error(), "CLI-WAS-SPAWNED") {
		t.Fatalf("a mismatched inbox must not deliver, got %v", err)
	}
	if !strings.Contains(h.log.String(), "INBOX_MISMATCH") {
		t.Error("the mismatch was not logged")
	}
	select {
	case <-p.askFrame:
		t.Fatal("an ask was delivered to a plugin not serving that thread")
	default:
	}
}

func TestInboxProtoMismatchIsLoud(t *testing.T) {
	p := newFakePlugin(t, "T-open")
	p.proto = inboxProto + 1
	h := inboxHarness(t, p, "T-open")

	_, err := h.b.askAmp("T-open", "hello")
	if err == nil {
		t.Fatal("a protocol mismatch must fail, not fall back")
	}
	if !strings.Contains(err.Error(), "protocol") || !strings.Contains(err.Error(), "init --amp-plugin") {
		t.Errorf("error must name the mismatch and the fix, got %v", err)
	}
}

func TestInboxErrorCodesAreActionable(t *testing.T) {
	for _, tc := range []struct {
		code, want string
	}{
		{"not-enabled", "Ctrl+O"},
		{"busy", "queued"},
		{"disabled", "check it before resending"},
		{"turn-error", "before answering"},
		{"turn-cancelled", "before answering"},
		{"timeout", "did not finish in time"},
		// The distinguishing fact is that the question already landed, so the
		// message must steer the caller away from resending it.
		{"no-turn", "do not resend"},
		{"append-failed", "could not append"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			p := newFakePlugin(t, "T-open")
			p.reply = inboxReply{Error: "plugin says so", Code: tc.code}
			h := inboxHarness(t, p, "T-open")

			_, err := h.b.askAmp("T-open", "hello")
			if err == nil {
				t.Fatal("an error code must surface as an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("code %s produced %q, want it to mention %q", tc.code, err, tc.want)
			}
			// Never silently retried through the CLI.
			if strings.Contains(err.Error(), "CLI-WAS-SPAWNED") {
				t.Errorf("code %s fell back to the CLI after the ask was sent", tc.code)
			}
		})
	}
}

// The plugin process dies mid-request. Delivery is genuinely unknown, and the
// message must say so rather than implying either outcome.
func TestInboxEOFMidRequestReportsAmbiguousDelivery(t *testing.T) {
	p := newFakePlugin(t, "T-open")
	p.hangup = true
	h := inboxHarness(t, p, "T-open")

	_, err := h.b.askAmp("T-open", "hello")
	if err == nil {
		t.Fatal("a hangup must surface as an error")
	}
	if !strings.Contains(err.Error(), "may or may not") {
		t.Errorf("error must admit delivery is ambiguous, got %v", err)
	}
}

// An untrusted runtime directory is a refusal, not an absence: it must not
// quietly downgrade into "no inbox, use the CLI".
func TestInboxRefusesUntrustedRuntimeDir(t *testing.T) {
	runtime := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", runtime)
	if err := os.Chmod(runtime, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	bin := fakeAmp(t, `echo CLI-WAS-SPAWNED >&2; exit 9`)
	h := newHarness(t, func(c *config) {
		c.ampDisabled = false
		c.ampBin = bin
		c.ampTimeout = 60 * time.Second
	})

	_, err := h.b.askAmp("T-open", "hello")
	if err == nil {
		t.Fatal("a world-writable runtime dir must be refused")
	}
	if strings.Contains(err.Error(), "CLI-WAS-SPAWNED") {
		t.Error("a trust refusal was downgraded into a CLI fallback")
	}
	if !strings.Contains(err.Error(), "group/other access") {
		t.Errorf("error must explain the refusal, got %v", err)
	}
}

// inbox/ is a subdirectory precisely so listBridges cannot mistake its contents
// for bridge registrations. glob does not descend; this pins that.
func TestListBridgesIgnoresTheInboxSubdirectory(t *testing.T) {
	runtime := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", runtime)
	if err := os.Chmod(runtime, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	threads := filepath.Join(runtime, "inbox", "threads")
	if err := os.MkdirAll(threads, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, _ := json.Marshal(inboxEntry{ThreadID: "T-open", Socket: "/nonexistent"})
	if err := os.WriteFile(filepath.Join(threads, "T-open.json"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := listBridges()
	if err != nil {
		t.Fatalf("listBridges: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("inbox entries leaked into the bridge registry: %+v", got)
	}
	// And the sweep must not have deleted the plugin's file.
	if _, err := os.Stat(filepath.Join(threads, "T-open.json")); err != nil {
		t.Errorf("listBridges removed a plugin inbox entry: %v", err)
	}
}

func TestDoctorReportsNoInboxesAsHealthy(t *testing.T) {
	runtime := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", runtime)
	if err := os.Chmod(runtime, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	got := checkPluginInboxes()
	// The plugin is optional. A doctor that warns about a feature nobody
	// installed teaches people to ignore it.
	if got.status != statusOK {
		t.Errorf("no inboxes must be OK, got %v: %s", got.status, got.detail)
	}
	if !strings.Contains(got.detail, "none") {
		t.Errorf("detail = %q, want it to say none", got.detail)
	}
}

func TestDoctorListsALiveInbox(t *testing.T) {
	p := newFakePlugin(t, "T-open")
	inboxHarness(t, p, "T-open") // sets AMP_BRIDGE_DIR and writes the entry

	got := checkPluginInboxes()
	if got.status != statusOK {
		t.Errorf("a live inbox must be OK, got %v: %s", got.status, got.detail)
	}
	if !strings.Contains(got.detail, "T-open") {
		t.Errorf("detail = %q, want it to name the thread", got.detail)
	}
}

func TestDoctorSweepsAndReportsAStaleInbox(t *testing.T) {
	p := newFakePlugin(t, "T-open")
	inboxHarness(t, p, "T-open")
	_ = p.ln.Close() // the Amp session exited without disposing

	got := checkPluginInboxes()
	if got.status != statusWarn {
		t.Errorf("a stale inbox must warn, got %v: %s", got.status, got.detail)
	}
	if !strings.Contains(got.detail, "stale") || !strings.Contains(got.fix, "Ctrl+O") {
		t.Errorf("must name the state and the fix, got %q / %q", got.detail, got.fix)
	}
	entry := filepath.Join(os.Getenv("AMP_BRIDGE_DIR"), "inbox", "threads", "T-open.json")
	if _, err := os.Stat(entry); err == nil {
		t.Error("doctor did not sweep the stale entry")
	}
}

// A live socket that no longer serves the thread is worse than a dead one: it
// would look healthy to a naive check.
func TestDoctorFlagsAnInboxThatNoLongerServesItsThread(t *testing.T) {
	p := newFakePlugin(t, "T-different")
	inboxHarness(t, p, "T-open")

	got := checkPluginInboxes()
	if got.status != statusWarn {
		t.Errorf("a mismatched inbox must warn, got %v: %s", got.status, got.detail)
	}
	if !strings.Contains(got.detail, "no longer serves") {
		t.Errorf("detail = %q, want it to explain the mismatch", got.detail)
	}
}

// The executor-conflict diagnosis is the one an operator hits most, so it must
// name the plugin route that removes the limitation, not only describe it.
func TestExecutorDiagnosisPointsAtThePluginRoute(t *testing.T) {
	t.Parallel()
	got := ampDiagnosis(writeLog(t, executorBusyLog))
	for _, want := range []string{"init --amp-plugin", "Ctrl+O", "one executor per thread"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnosis %q must mention %q", got, want)
		}
	}
}
