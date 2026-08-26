//go:build integration

// End-to-end tests against a real amp-bridge process: they drive it over stdio
// exactly as Claude Code does, and over the Unix socket exactly as Amp does.
//
//	go test -tags=integration ./...
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ── process harness ─────────────────────────────────────────────────────────

type proc struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	bin     string
	dir     string
	logPath string
	env     []string

	mu     sync.Mutex
	frames []map[string]any
}

// buildBinary compiles the bridge once per test binary. It builds rather than
// copies: copying a Mach-O invalidates its signature and macOS then SIGKILLs it.
var buildOnce = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("/tmp", "ampbuild")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "amp-bridge")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		return "", errors.New("go build: " + err.Error() + ": " + string(out))
	}
	return bin, nil
})

func startBridge(t *testing.T, extraEnv ...string) *proc {
	t.Helper()

	bin, err := buildOnce()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Short paths: Unix sockets cap out around 103 bytes.
	dir, err := os.MkdirTemp("/tmp", "ampb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	logPath := filepath.Join(dir, "bridge.log")
	env := append(os.Environ(),
		"AMP_BRIDGE_DIR="+dir,
		"AMP_BRIDGE_LOG="+logPath,
		"AMP_BRIDGE_MAX_INFLIGHT=3",
		"AMP_BRIDGE_MAX_BYTES=2048",
		"AMP_BRIDGE_TIMEOUT=10s",
		"AMP_BRIDGE_DISABLE_OUTBOUND=1",
	)
	env = append(env, extraEnv...)

	cmd := exec.Command(bin)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	p := &proc{t: t, cmd: cmd, stdin: stdin, bin: bin, dir: dir, logPath: logPath, env: env}
	go p.pump(stdout)
	t.Cleanup(p.stop)
	return p
}

func (p *proc) pump(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		p.mu.Lock()
		p.frames = append(p.frames, m)
		p.mu.Unlock()
	}
}

func (p *proc) stop() {
	_ = p.stdin.Close()
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
	}
}

func (p *proc) send(msg map[string]any) {
	p.t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		p.t.Fatalf("marshal: %v", err)
	}
	if _, err := p.stdin.Write(append(data, '\n')); err != nil {
		p.t.Fatalf("write: %v", err)
	}
}

func (p *proc) snapshot() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.frames...)
}

// await polls until pick returns a frame, so no test depends on a fixed sleep.
func (p *proc) await(what string, pick func([]map[string]any) (map[string]any, bool)) map[string]any {
	p.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m, ok := pick(p.snapshot()); ok {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for %s", what)
	return nil
}

func (p *proc) awaitID(id any) map[string]any {
	p.t.Helper()
	return p.await("a response to request "+jsonStr(id), func(fs []map[string]any) (map[string]any, bool) {
		for _, f := range fs {
			if got, ok := f["id"]; ok && equalJSON(got, id) {
				return f, true
			}
		}
		return nil, false
	})
}

func (p *proc) channelEvents() []map[string]any {
	var out []map[string]any
	for _, f := range p.snapshot() {
		if f["method"] == "notifications/claude/channel" {
			out = append(out, f)
		}
	}
	return out
}

func (p *proc) awaitEvents(n int) []map[string]any {
	p.t.Helper()
	p.await("at least "+jsonStr(n)+" channel events", func([]map[string]any) (map[string]any, bool) {
		if len(p.channelEvents()) >= n {
			return map[string]any{}, true
		}
		return nil, false
	})
	return p.channelEvents()
}

func requestIDOf(t *testing.T, event map[string]any) string {
	t.Helper()
	params, ok := event["params"].(map[string]any)
	if !ok {
		t.Fatalf("event has no params: %v", event)
	}
	meta, ok := params["meta"].(map[string]any)
	if !ok {
		t.Fatalf("event has no meta: %v", params)
	}
	id, _ := meta["request_id"].(string)
	if id == "" {
		t.Fatalf("event carries no request_id: %v", meta)
	}
	return id
}

func (p *proc) handshake() {
	p.t.Helper()
	p.send(map[string]any{
		"jsonrpc": "2.0", "id": 0, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{"roots": map[string]any{"listChanged": true}},
			"clientInfo":      map[string]any{"name": "claude-code", "version": "2.1.235"},
		},
	})
	p.awaitID(0)
	p.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

// socketPath waits for the bridge to publish itself, then returns its socket.
func (p *proc) socketPath() string {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(p.dir, "*.json"))
		for _, m := range matches {
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			var e registryEntry
			if json.Unmarshal(data, &e) == nil && e.Socket != "" {
				if _, err := os.Stat(e.Socket); err == nil {
					return e.Socket
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.t.Fatal("bridge never published a usable socket")
	return ""
}

func (p *proc) dial() net.Conn {
	p.t.Helper()
	conn, err := net.Dial("unix", p.socketPath())
	if err != nil {
		p.t.Fatalf("dial: %v", err)
	}
	p.t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		p.t.Fatalf("deadline: %v", err)
	}
	return conn
}

func (p *proc) logText() string {
	p.t.Helper()
	data, err := os.ReadFile(p.logPath)
	if err != nil {
		p.t.Fatalf("read log: %v", err)
	}
	return string(data)
}

// awaitLog polls the log file: log lines are written just after the frame they
// describe, so waiting on stdout alone can outrun them.
func (p *proc) awaitLog(want string) {
	p.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.logText(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for %q in the log; log was:\n%s", want, p.logText())
}

func (p *proc) replyTool(id, requestID, text string) map[string]any {
	p.t.Helper()
	p.send(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{
			"name":      toolReply,
			"arguments": map[string]any{"text": text, "request_id": requestID},
		},
	})
	return p.awaitID(id)
}

func sendReq(t *testing.T, conn net.Conn, req ampRequest) {
	t.Helper()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func readResp(t *testing.T, conn net.Conn) ampResponse {
	t.Helper()
	var r ampResponse
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return r
}

// ── the negotiation contract, against the real process ──────────────────────

func TestProcessDeclinesServerDiscover(t *testing.T) {
	p := startBridge(t)

	p.send(map[string]any{
		"jsonrpc": "2.0", "id": "d1", "method": "server/discover",
		"params": map[string]any{
			"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28"},
		},
	})

	if _, ok := p.awaitID("d1")["error"]; !ok {
		t.Fatal("server/discover must be declined: answering it negotiates the modern era, " +
			"which has no delivery path for unsolicited notifications")
	}
}

func TestProcessHandshakeAndTools(t *testing.T) {
	p := startBridge(t)
	p.handshake()

	res, _ := p.awaitID(0)["result"].(map[string]any)
	caps, _ := res["capabilities"].(map[string]any)
	exp, _ := caps["experimental"].(map[string]any)
	if _, ok := exp["claude/channel"]; !ok {
		t.Errorf("experimental['claude/channel'] missing: %v", caps)
	}
	if _, ok := caps["claude/channel"]; ok {
		t.Error("claude/channel must not be a top-level capability")
	}
	if res["protocolVersion"] != "2025-11-25" {
		t.Errorf("protocolVersion = %v, want the client's own echoed back", res["protocolVersion"])
	}
	if s, _ := res["instructions"].(string); s == "" {
		t.Error("instructions must be declared")
	}

	p.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	lr, _ := p.awaitID(1)["result"].(map[string]any)
	tools, _ := lr["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		m, _ := tool.(map[string]any)
		n, _ := m["name"].(string)
		names[n] = true
	}
	if !names[toolReply] || !names[toolAskAmp] || !names[toolSendAmp] {
		t.Errorf("reply plus synchronous and asynchronous outbound tools must be exposed, got %v", names)
	}
}

// ── the round trip both ways ────────────────────────────────────────────────

func TestProcessRoundTrip(t *testing.T) {
	p := startBridge(t)
	p.handshake()
	conn := p.dial()

	sendReq(t, conn, ampRequest{Text: "what is 2+2?"})
	events := p.awaitEvents(1)

	params, _ := events[0]["params"].(map[string]any)
	if params["content"] != "what is 2+2?" {
		t.Errorf("content = %v, want the Amp text", params["content"])
	}
	if _, ok := params["_meta"]; ok {
		t.Error("the field is `meta`, not the MCP-standard `_meta`")
	}
	id := requestIDOf(t, events[0])

	res, _ := p.replyTool("r1", id, "4")["result"].(map[string]any)
	if !strings.Contains(jsonStr(res), "delivered") {
		t.Errorf("reply should report delivery: %v", res)
	}

	got := readResp(t, conn)
	if got.Reply != "4" || got.RequestID != id {
		t.Errorf("Amp got %+v, want reply 4 correlated to %s", got, id)
	}
}

func TestProcessConcurrentCallersDoNotCrossTalk(t *testing.T) {
	p := startBridge(t)
	p.handshake()

	// Two independent callers, as two `&`-launched Amp jobs would be. They race
	// to the socket, so nothing may assume they arrive in launch order.
	a, b := p.dial(), p.dial()
	sendReq(t, a, ampRequest{Text: "question A"})
	sendReq(t, b, ampRequest{Text: "question B"})

	events := p.awaitEvents(2)
	id1, id2 := requestIDOf(t, events[0]), requestIDOf(t, events[1])
	if id1 == id2 {
		t.Fatal("concurrent requests must get distinct request_ids")
	}

	// With two in flight, an un-addressed reply must be refused rather than
	// guessed — guessing silently hands one caller the other's answer.
	p.send(map[string]any{
		"jsonrpc": "2.0", "id": "amb", "method": "tools/call",
		"params": map[string]any{
			"name":      toolReply,
			"arguments": map[string]any{"text": "guess"},
		},
	})
	ambRes, _ := p.awaitID("amb")["result"].(map[string]any)
	if v, _ := ambRes["isError"].(bool); !v {
		t.Errorf("an ambiguous reply must be refused: %v", ambRes)
	}
	if !strings.Contains(jsonStr(ambRes), "request_id is required") {
		t.Errorf("the refusal must say how to fix it: %v", ambRes)
	}

	p.send(map[string]any{
		"jsonrpc": "2.0", "id": "bad", "method": "tools/call",
		"params": map[string]any{
			"name":      toolReply,
			"arguments": map[string]any{"text": "nope", "request_id": "amp-does-not-exist"},
		},
	})
	badRes, _ := p.awaitID("bad")["result"].(map[string]any)
	if v, _ := badRes["isError"].(bool); !v {
		t.Errorf("an unknown request_id must be refused: %v", badRes)
	}

	answers := map[string]string{id1: "ANSWER-1", id2: "ANSWER-2"}
	p.replyTool("x1", id1, answers[id1])
	p.replyTool("x2", id2, answers[id2])

	got1, got2 := readResp(t, a), readResp(t, b)
	for _, r := range []ampResponse{got1, got2} {
		if answers[r.RequestID] != r.Reply {
			t.Errorf("caller %s got %q, want %q", r.RequestID, r.Reply, answers[r.RequestID])
		}
	}
	if got1.Reply == got2.Reply {
		t.Error("both callers received the same answer — replies crossed")
	}
}

// ── caps, hygiene, resilience ───────────────────────────────────────────────

func TestProcessResourceCaps(t *testing.T) {
	p := startBridge(t) // MAX_BYTES=2048, MAX_INFLIGHT=3
	p.handshake()

	over := p.dial()
	sendReq(t, over, ampRequest{Text: strings.Repeat("X", 3000)})
	if r := readResp(t, over); !strings.Contains(r.Error, "too large") {
		t.Errorf("oversize should be rejected with an explanation, got %+v", r)
	}

	// Fill every slot, then prove the next one is refused rather than queued.
	held := make([]net.Conn, 0, 3)
	for range 3 {
		c := p.dial()
		sendReq(t, c, ampRequest{Text: "hold"})
		held = append(held, c)
	}
	p.awaitEvents(3)

	extra := p.dial()
	sendReq(t, extra, ampRequest{Text: "one too many"})
	if r := readResp(t, extra); !strings.Contains(r.Error, "too many requests") {
		t.Errorf("the in-flight cap must be enforced, got %+v", r)
	}

	// Answering frees the slots again.
	for i, e := range p.awaitEvents(3) {
		p.replyTool("h"+jsonStr(i), requestIDOf(t, e), "done")
	}
	for _, c := range held {
		readResp(t, c)
	}
	after := p.dial()
	sendReq(t, after, ampRequest{Text: "room again"})
	p.awaitEvents(4)
}

func TestProcessLogHygiene(t *testing.T) {
	p := startBridge(t)
	p.handshake()
	conn := p.dial()

	secret := "the-passphrase-is-hunter2"
	sendReq(t, conn, ampRequest{Text: secret})
	p.awaitEvents(1)

	fi, err := os.Stat(p.logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("log mode = %o, want 600 — it can hold conversation text", perm)
	}
	p.awaitLog("EVENT_PUSHED") // frame shape is logged even when bodies are not
	if strings.Contains(p.logText(), secret) {
		t.Error("conversation content must stay out of the log unless AMP_BRIDGE_LOG_BODIES=1")
	}
}

func TestProcessSurvivesCallerDisconnect(t *testing.T) {
	p := startBridge(t)
	p.handshake()

	doomed, err := net.Dial("unix", p.socketPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sendReq(t, doomed, ampRequest{Text: "I will hang up immediately"})
	p.awaitEvents(1)
	if err := doomed.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A hang-up must release the slot immediately, not hold it for the full timeout.
	p.awaitLog("AMP_ABANDONED")

	// And the bridge keeps serving.
	next := p.dial()
	sendReq(t, next, ampRequest{Text: "still working?"})
	events := p.awaitEvents(2)
	p.replyTool("alive", requestIDOf(t, events[1]), "ALIVE")
	if r := readResp(t, next); r.Reply != "ALIVE" {
		t.Errorf("bridge did not recover from a disconnect: %+v", r)
	}
}

func TestProcessAskAmpFailsCleanly(t *testing.T) {
	p := startBridge(t) // outbound disabled
	p.handshake()

	p.send(map[string]any{
		"jsonrpc": "2.0", "id": "aa", "method": "tools/call",
		"params": map[string]any{
			"name":      toolAskAmp,
			"arguments": map[string]any{"text": "hello amp"},
		},
	})
	res, _ := p.awaitID("aa")["result"].(map[string]any)
	if v, _ := res["isError"].(bool); !v {
		t.Errorf("ask_amp must report a tool error when outbound is disabled: %v", res)
	}

	p.send(map[string]any{
		"jsonrpc": "2.0", "id": "junk", "method": "tools/call",
		"params": map[string]any{"name": "nope", "arguments": map[string]any{}},
	})
	if _, ok := p.awaitID("junk")["error"]; !ok {
		t.Error("an unknown tool must be a JSON-RPC error")
	}
}

// ── the client half of the same binary ──────────────────────────────────────

func TestProcessClientListAndAsk(t *testing.T) {
	p := startBridge(t)
	p.handshake()
	p.socketPath() // wait until it has registered

	// p.env matters: without it --list reads the ambient AMP_BRIDGE_DIR instead
	// of this harness's, so on a developer machine it finds their own live
	// session and passes while testing nothing. On CI there is no such session
	// and it fails, which is how this was found.
	listCmd := exec.Command(p.bin, "--list")
	listCmd.Env = p.env
	listOut, err := listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--list failed: %v: %s", err, listOut)
	}
	if !strings.Contains(string(listOut), "claude_pid=") {
		t.Errorf("--list should name the live bridge, got %q", listOut)
	}
	verboseCmd := exec.Command(p.bin, "--list", "--verbose")
	verboseCmd.Env = p.env
	verboseOut, err := verboseCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--list --verbose failed: %v: %s", err, verboseOut)
	}
	for _, want := range []string{"bridge_pid=", "version=", "build=", "handshake=", "socket="} {
		if !strings.Contains(string(verboseOut), want) {
			t.Errorf("verbose list omitted %q: %s", want, verboseOut)
		}
	}

	// --ask blocks until Claude replies, so answer it from this side.
	type askResult struct {
		out []byte
		err error
	}
	done := make(chan askResult, 1)
	go func() {
		cmd := exec.Command(p.bin, "--ask", "hello", "from", "the", "client")
		cmd.Env = p.env
		out, err := cmd.CombinedOutput()
		done <- askResult{out, err}
	}()

	events := p.awaitEvents(1)
	params, _ := events[0]["params"].(map[string]any)
	if params["content"] != "hello from the client" {
		t.Errorf("content = %v, want the joined message", params["content"])
	}
	p.replyTool("cli", requestIDOf(t, events[0]), "ANSWERED")

	r := <-done
	if r.err != nil {
		t.Fatalf("--ask failed: %v: %s", r.err, r.out)
	}
	if strings.TrimSpace(string(r.out)) != "ANSWERED" {
		t.Errorf("--ask printed %q, want ANSWERED on stdout", r.out)
	}
}

func TestClientListWithNoBridges(t *testing.T) {
	bin, err := buildOnce()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	dir, err := os.MkdirTemp("/tmp", "ampb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	cmd := exec.Command(bin, "--list")
	cmd.Env = append(os.Environ(), "AMP_BRIDGE_DIR="+dir)
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Error("--list with nothing live must exit non-zero so scripts can branch on it")
	}
	if !strings.Contains(string(out), "no live amp-bridge sessions") {
		t.Errorf("output = %q, want a plain explanation", out)
	}
}

func TestClientRejectsUnknownFlags(t *testing.T) {
	bin, err := buildOnce()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	out, err := exec.Command(bin, "--sesion", "typo").CombinedOutput()
	if err == nil {
		t.Error("a typo'd flag must fail loudly rather than starting a server")
	}
	if !strings.Contains(string(out), "unknown argument") {
		t.Errorf("output = %q, want it to name the bad argument", out)
	}
}

// ── lifecycle ───────────────────────────────────────────────────────────────

func TestProcessCleansUpOnSIGTERM(t *testing.T) {
	p := startBridge(t)
	p.handshake()

	sock := p.socketPath()
	entries, _ := filepath.Glob(filepath.Join(p.dir, "*.json"))
	if len(entries) == 0 {
		t.Fatal("bridge never published a registry entry")
	}

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	_ = p.cmd.Wait()

	// A stale socket plus a stale registration makes the next `--list` lie
	// until it sweeps them, which is confusing while it lasts.
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket %s survived SIGTERM", sock)
	}
	for _, e := range entries {
		if _, err := os.Stat(e); !os.IsNotExist(err) {
			t.Errorf("registry entry %s survived SIGTERM", e)
		}
	}
	if !strings.Contains(p.logText(), "signal terminated") {
		t.Errorf("the shutdown should be logged; log was:\n%s", p.logText())
	}
}

func TestProcessRefusesToHijackALiveSocket(t *testing.T) {
	first := startBridge(t)
	first.handshake()
	sock := first.socketPath()

	// A second bridge pointed at the same socket must not steal it: doing so
	// would silently cut the first session off from Amp.
	cmd := exec.Command(first.bin)
	cmd.Env = append(append([]string{}, first.env...), "AMP_BRIDGE_SOCKET="+sock)
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Error("the second bridge should have refused to start")
	}
	if !strings.Contains(string(out), "refusing to hijack") {
		t.Errorf("stderr = %q, want an explanation of the refusal", out)
	}
}

func TestProcessExitsWhenStdinCloses(t *testing.T) {
	p := startBridge(t)
	p.handshake()

	// Claude Code closes stdin to shut a server down.
	if err := p.stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("bridge exited with %v, want a clean exit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not exit after stdin closed")
	}
}
