package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── egress shape ────────────────────────────────────────────────────────────

var identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func TestPushEventNotificationShape(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	id, sink, err := h.b.pushEvent("what is 2+2?", "")
	if err != nil {
		t.Fatalf("pushEvent: %v", err)
	}
	if id == "" || sink == nil {
		t.Fatal("pushEvent returned no id or sink")
	}

	notifs := h.notifications(t)
	if len(notifs) != 1 {
		t.Fatalf("want exactly one channel notification, got %d", len(notifs))
	}
	params, ok := notifs[0]["params"].(map[string]any)
	if !ok {
		t.Fatalf("notification has no params: %v", notifs[0])
	}

	if got := params["content"]; got != "what is 2+2?" {
		t.Errorf("content = %v, want the Amp text verbatim", got)
	}
	// The MCP-standard field is _meta; Claude's channel reads `meta`. Sending
	// the standard one means the attributes never reach the <channel> tag.
	if _, ok := params["_meta"]; ok {
		t.Error("must not send _meta — Claude's channel reads `meta`")
	}
	meta, ok := params["meta"].(map[string]any)
	if !ok {
		t.Fatalf("params.meta missing or not an object: %v", params)
	}
	for k, v := range meta {
		if !identifierRE.MatchString(k) {
			t.Errorf("meta key %q is not an identifier; Claude drops it silently", k)
		}
		if _, ok := v.(string); !ok {
			t.Errorf("meta[%q] = %v (%T), want a string", k, v, v)
		}
		// Claude sets source="<channel name>" itself; ours would duplicate it.
		if k == "source" {
			t.Error("`source` is reserved — Claude sets it, a duplicate emits the attribute twice")
		}
	}
	if meta["request_id"] != id {
		t.Errorf("meta.request_id = %v, want the pending id %q", meta["request_id"], id)
	}
}

func TestPushEventIDsAreDistinct(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	seen := map[string]bool{}
	for range 5 {
		id, _, err := h.b.pushEvent("x", "")
		if err != nil {
			t.Fatalf("pushEvent: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate request_id %q — concurrent replies would cross-talk", id)
		}
		seen[id] = true
	}
}

func TestPushEventEnforcesInFlightCap(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config) { c.maxInFlight = 3 })

	for i := range 3 {
		if _, _, err := h.b.pushEvent("x", ""); err != nil {
			t.Fatalf("push %d should fit under the cap: %v", i, err)
		}
	}
	_, _, err := h.b.pushEvent("one too many", "")
	if err == nil {
		t.Fatal("the in-flight cap must be enforced; it is what stops a runaway loop flooding Claude")
	}
	if !strings.Contains(err.Error(), "too many requests") {
		t.Errorf("error should name the cause: %v", err)
	}

	// A released slot is reusable.
	h.b.drop(firstPending(h.b))
	if _, _, err := h.b.pushEvent("now there is room", ""); err != nil {
		t.Errorf("dropping a request must free its slot: %v", err)
	}
}

func firstPending(b *bridge) string {
	b.pendMu.Lock()
	defer b.pendMu.Unlock()
	for k := range b.pending {
		return k
	}
	return ""
}

// ── correlation ─────────────────────────────────────────────────────────────

func TestResolveRouting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pending int
		// useID picks which pending id to answer: -1 means send no id at all.
		useID int
		want  resolveResult
	}{
		{"exact id with one pending", 1, 0, resolveOK},
		{"exact id with several pending", 3, 1, resolveOK},
		// An omitted id is ALWAYS refused, even when exactly one request is
		// pending. Inferring the target from cardinality is unsound across a
		// timeout boundary: A times out and is dropped, B becomes the only
		// pending request, and a belated id-less answer meant for A lands on B.
		// This case previously expected resolveOK — the test was pinning the bug.
		{"omitted id with exactly one pending is still refused", 1, -1, resolveMissingID},
		{"omitted id with several pending is refused", 2, -1, resolveMissingID},
		{"omitted id with none pending is refused", 0, -1, resolveMissingID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, func(c *config) { c.maxInFlight = 10 })

			ids := make([]string, 0, tc.pending)
			sinks := make([]chan string, 0, tc.pending)
			for range tc.pending {
				id, sink, err := h.b.pushEvent("q", "")
				if err != nil {
					t.Fatalf("pushEvent: %v", err)
				}
				ids = append(ids, id)
				sinks = append(sinks, sink)
			}

			ask := ""
			if tc.useID >= 0 {
				ask = ids[tc.useID]
			}
			got, gotID := h.b.resolve(ask, "ANSWER")
			if got != tc.want {
				t.Fatalf("resolve = %v, want %v", got, tc.want)
			}
			if tc.want != resolveOK {
				return
			}

			// The answer must land in the matching sink and nowhere else.
			target := max(tc.useID, 0)
			select {
			case v := <-sinks[target]:
				if v != "ANSWER" {
					t.Errorf("sink got %q, want ANSWER", v)
				}
			default:
				t.Fatalf("resolve reported ok (id %q) but the waiter got nothing", gotID)
			}
			for i, s := range sinks {
				if i == target {
					continue
				}
				select {
				case v := <-s:
					t.Errorf("cross-talk: waiter %d received %q", i, v)
				default:
				}
			}
		})
	}
}

func TestResolveUnknownID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, _, err := h.b.pushEvent("q", ""); err != nil {
		t.Fatalf("pushEvent: %v", err)
	}

	got, id := h.b.resolve("amp-does-not-exist", "x")
	if got != resolveUnknownID {
		t.Errorf("resolve = %v, want resolveUnknownID", got)
	}
	if id != "amp-does-not-exist" {
		t.Errorf("the unmatched id should be echoed back for the error message, got %q", id)
	}
	if h.b.pendingCount() != 1 {
		t.Error("a bad id must not consume the real pending request")
	}
}

func TestResolveIsOneShot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id, _, err := h.b.pushEvent("q", "")
	if err != nil {
		t.Fatalf("pushEvent: %v", err)
	}

	if got, _ := h.b.resolve(id, "first"); got != resolveOK {
		t.Fatalf("first resolve should succeed, got %v", got)
	}
	if got, _ := h.b.resolve(id, "second"); got != resolveUnknownID {
		t.Errorf("a second reply to the same id must be refused, got %v", got)
	}
}

func TestResolveLateReplyIsOneShot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config) { c.maxMessageBytes = 20 })
	id, _, err := h.b.pushEvent("q", "")
	if err != nil {
		t.Fatalf("pushEvent: %v", err)
	}
	if _, ok := h.b.expire(id); !ok {
		t.Fatal("expire = false")
	}

	first := strings.Repeat("first", 10)
	if got, _ := h.b.resolve(id, first); got != resolveStoredLate {
		t.Fatalf("first late resolve should be stored, got %v", got)
	}
	if got, _ := h.b.resolve(id, "second"); got != resolveUnknownID {
		t.Errorf("a second late reply must be refused, got %v", got)
	}
	state, retained := h.b.retainedResult(id)
	if state != retainedReady || retained.text != truncateMessage(first, 20) {
		t.Errorf("retained result = (%v, %q), want bounded first reply", state, retained.text)
	}
}

func TestResolveConcurrent(t *testing.T) {
	t.Parallel()
	const n = 24
	h := newHarness(t, func(c *config) { c.maxInFlight = n })

	ids := make([]string, 0, n)
	sinks := make(map[string]chan string, n)
	for range n {
		id, sink, err := h.b.pushEvent("q", "")
		if err != nil {
			t.Fatalf("pushEvent: %v", err)
		}
		ids = append(ids, id)
		sinks[id] = sink
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if got, _ := h.b.resolve(id, "answer-"+id); got != resolveOK {
				t.Errorf("resolve(%s) = %v", id, got)
			}
		}(id)
	}
	wg.Wait()

	for id, sink := range sinks {
		select {
		case v := <-sink:
			if v != "answer-"+id {
				t.Errorf("waiter %s got %q — answers were mis-routed", id, v)
			}
		default:
			t.Errorf("waiter %s never received its answer", id)
		}
	}
	if h.b.pendingCount() != 0 {
		t.Errorf("pendingCount = %d, want 0", h.b.pendingCount())
	}
}

// ── the reply tool end of correlation ───────────────────────────────────────

func TestReplyToolOutcomes(t *testing.T) {
	t.Parallel()

	t.Run("delivers", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		id, sink, err := h.b.pushEvent("q", "")
		if err != nil {
			t.Fatalf("pushEvent: %v", err)
		}
		h.call(t, replyCall("r", "4", id))

		res := result(t, h.response(t, "r"))
		if isToolError(t, res) {
			t.Errorf("a well-formed reply must succeed: %v", res)
		}
		if !strings.Contains(toolText(t, res), "delivered") {
			t.Errorf("Claude should be told it worked: %q", toolText(t, res))
		}
		if got := <-sink; got != "4" {
			t.Errorf("Amp received %q, want 4", got)
		}
	})

	t.Run("refuses an ambiguous reply", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		for range 2 {
			if _, _, err := h.b.pushEvent("q", ""); err != nil {
				t.Fatalf("pushEvent: %v", err)
			}
		}
		h.call(t, replyCall("r", "guess", ""))

		res := result(t, h.response(t, "r"))
		if !isToolError(t, res) {
			t.Fatal("with two requests in flight an un-addressed reply must be refused, not guessed")
		}
		if !strings.Contains(toolText(t, res), "request_id is required") {
			t.Errorf("the message must tell Claude how to fix it: %q", toolText(t, res))
		}
	})

	t.Run("refuses an unknown id", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.call(t, replyCall("r", "x", "amp-nope"))
		if !isToolError(t, result(t, h.response(t, "r"))) {
			t.Error("an unknown request_id must be reported as an error")
		}
	})

	t.Run("stores a late reply", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		id, _, err := h.b.pushEvent("q", "")
		if err != nil {
			t.Fatalf("pushEvent: %v", err)
		}
		if _, ok := h.b.expire(id); !ok {
			t.Fatal("expire = false")
		}
		h.call(t, replyCall("r", "late", id))

		res := result(t, h.response(t, "r"))
		if isToolError(t, res) {
			t.Errorf("a retained late reply should be accepted: %v", res)
		}
		if !strings.Contains(toolText(t, res), "retained") {
			t.Errorf("Claude should be told the late reply was retained: %q", toolText(t, res))
		}
	})
}

func replyCall(id, text, requestID string) map[string]any {
	args := map[string]any{"text": text}
	if requestID != "" {
		args["request_id"] = requestID
	}
	return map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": toolReply, "arguments": args},
	}
}

// ── ingress validation ──────────────────────────────────────────────────────

func TestRejectRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config) { c.maxMessageBytes = 32 })

	tests := []struct {
		name    string
		req     ampRequest
		wantOK  bool
		wantMsg string
	}{
		{"accepts normal text", ampRequest{Text: "hello"}, true, ""},
		{"rejects empty", ampRequest{Text: ""}, false, "empty text"},
		{"rejects whitespace only", ampRequest{Text: "   \n\t "}, false, "empty text"},
		{"rejects oversize", ampRequest{Text: strings.Repeat("X", 33)}, false, "too large"},
		{"accepts exactly at the cap", ampRequest{Text: strings.Repeat("X", 32)}, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, ok := h.b.rejectRequest(tc.req)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (msg %q)", ok, tc.wantOK, msg)
			}
			if !ok && !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("message = %q, want it to mention %q", msg, tc.wantMsg)
			}
		})
	}
}

func TestRememberThread(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if h.b.knownThread() != "" {
		t.Error("no thread should be known before Amp sends one")
	}
	h.b.rememberThread("  ")
	if h.b.knownThread() != "" {
		t.Error("blank thread ids must be ignored")
	}
	h.b.rememberThread("T-1")
	h.b.rememberThread("T-2")
	if got := h.b.knownThread(); got != "T-2" {
		t.Errorf("knownThread = %q, want the most recent T-2", got)
	}
}

// ── socket ingress, over a real listener ────────────────────────────────────

// tempSocket returns a short path: Unix socket paths cap out around 103 bytes,
// and the default temp dir on macOS is long enough to matter.
func tempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ampb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func serveOnTempSocket(t *testing.T, h *harness) string {
	t.Helper()
	sock := tempSocket(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go h.b.serveSocket(ln)
	return sock
}

func dialBridge(t *testing.T, sock string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	return conn
}

// waitFor polls until cond holds, so tests never depend on a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSocketRoundTrip(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	conn := dialBridge(t, serveOnTempSocket(t, h))

	if err := json.NewEncoder(conn).Encode(ampRequest{Text: "ping?", ThreadID: "T-9"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, "the channel notification", func() bool { return len(h.notifications(t)) == 1 })

	params, _ := h.notifications(t)[0]["params"].(map[string]any)
	meta, _ := params["meta"].(map[string]any)
	id, _ := meta["request_id"].(string)

	if got, _ := h.b.resolve(id, "pong"); got != resolveOK {
		t.Fatalf("resolve = %v", got)
	}

	var resp ampResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if resp.Reply != "pong" {
		t.Errorf("reply = %q, want pong", resp.Reply)
	}
	if resp.RequestID != id {
		t.Errorf("request_id = %q, want %q — callers pair answers by this alone", resp.RequestID, id)
	}
	if h.b.knownThread() != "T-9" {
		t.Error("the thread id on the request should be remembered for ask_amp")
	}
}

func TestSocketRejectionsAreReportedNotDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config) { c.maxMessageBytes = 64 })
	conn := dialBridge(t, serveOnTempSocket(t, h))

	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	for _, tc := range []struct {
		req  ampRequest
		want string
	}{
		{ampRequest{Text: ""}, "empty text"},
		{ampRequest{Text: strings.Repeat("X", 100)}, "too large"},
	} {
		if err := enc.Encode(tc.req); err != nil {
			t.Fatalf("send: %v", err)
		}
		var resp ampResponse
		if err := dec.Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !strings.Contains(resp.Error, tc.want) {
			t.Errorf("error = %q, want it to mention %q", resp.Error, tc.want)
		}
	}

	// The connection must stay usable after a rejection.
	if err := enc.Encode(ampRequest{Text: "still there?"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, "an event after two rejections", func() bool { return len(h.notifications(t)) == 1 })
}

func TestCallerDisconnectFreesTheSlot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sock := serveOnTempSocket(t, h)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := json.NewEncoder(conn).Encode(ampRequest{Text: "I will hang up"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, "the event to be pushed", func() bool { return h.b.pendingCount() == 1 })

	// Hanging up must release the slot immediately rather than holding it for
	// the full reply timeout.
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitFor(t, "the request to be abandoned", func() bool {
		return strings.Contains(h.log.String(), "AMP_ABANDONED")
	})
	if n := h.b.pendingCount(); n != 0 {
		t.Errorf("pendingCount = %d, want 0 — a hang-up must not hold the slot for the full timeout", n)
	}

	// And the bridge must keep serving afterwards.
	next := dialBridge(t, sock)
	if err := json.NewEncoder(next).Encode(ampRequest{Text: "still working?"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, "a post-disconnect event", func() bool { return len(h.notifications(t)) == 2 })
}

func TestReplyTimeoutReleasesTheSlot(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config) {
		c.replyWait = 100 * time.Millisecond
		c.resultTTL = time.Minute
	})
	conn := dialBridge(t, serveOnTempSocket(t, h))

	if err := json.NewEncoder(conn).Encode(ampRequest{Text: "nobody will answer"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var resp ampResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(resp.Error, "timed out") {
		t.Errorf("error = %q, want a timeout", resp.Error)
	}
	if resp.RequestID == "" {
		t.Fatal("a timeout must return its request id for late-result retrieval")
	}
	waitFor(t, "the timed-out slot to be released", func() bool { return h.b.pendingCount() == 0 })

	if outcome, _ := h.b.resolve(resp.RequestID, "late answer"); outcome != resolveStoredLate {
		t.Fatalf("late resolve = %s, want stored-late", outcome)
	}
	if err := json.NewEncoder(conn).Encode(ampRequest{ResultID: resp.RequestID}); err != nil {
		t.Fatalf("request retained result: %v", err)
	}
	var retained ampResponse
	if err := json.NewDecoder(conn).Decode(&retained); err != nil {
		t.Fatalf("decode retained result: %v", err)
	}
	if retained.Reply != "late answer" {
		t.Errorf("retained reply = %q, want late answer", retained.Reply)
	}
}

func TestPerRequestTimeoutExtendsTheDefaultAndIsCapped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config) {
		c.replyWait = 20 * time.Millisecond
		c.maxReplyWait = 500 * time.Millisecond
	})
	sock := serveOnTempSocket(t, h)

	extended := dialBridge(t, sock)
	if err := json.NewEncoder(extended).Encode(ampRequest{
		Text:          "take longer",
		TimeoutMillis: 500,
	}); err != nil {
		t.Fatalf("send extended request: %v", err)
	}
	waitFor(t, "the extended request", func() bool { return len(h.notifications(t)) == 1 })
	time.Sleep(50 * time.Millisecond)
	params, _ := h.notifications(t)[0]["params"].(map[string]any)
	meta, _ := params["meta"].(map[string]any)
	id, _ := meta["request_id"].(string)
	if got := meta["timeout_ms"]; got != "500" {
		t.Errorf("timeout_ms = %v, want 500", got)
	}
	if outcome, _ := h.b.resolve(id, "done"); outcome != resolveOK {
		t.Fatalf("resolve extended request = %s", outcome)
	}
	var extendedResp ampResponse
	if err := json.NewDecoder(extended).Decode(&extendedResp); err != nil {
		t.Fatalf("decode extended response: %v", err)
	}
	if extendedResp.Reply != "done" {
		t.Errorf("extended reply = %q, want done", extendedResp.Reply)
	}

	capped := dialBridge(t, sock)
	if err := json.NewEncoder(capped).Encode(ampRequest{
		Text:          "cap me",
		TimeoutMillis: int64((5 * time.Second) / time.Millisecond),
	}); err != nil {
		t.Fatalf("send capped request: %v", err)
	}
	var cappedResp ampResponse
	if err := json.NewDecoder(capped).Decode(&cappedResp); err != nil {
		t.Fatalf("decode capped response: %v", err)
	}
	if !strings.Contains(cappedResp.Error, "timed out") {
		t.Errorf("capped error = %q, want timeout", cappedResp.Error)
	}
}

func TestRetainedRepliesExpireAndStayBounded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *config) {
		c.resultTTL = 20 * time.Millisecond
		c.maxResults = 2
	})

	for _, id := range []string{"first", "second", "third"} {
		h.b.pendMu.Lock()
		h.b.pending[id] = make(chan string, 1)
		h.b.pendMu.Unlock()
		if _, ok := h.b.expire(id); !ok {
			t.Fatalf("expire(%s) = false", id)
		}
	}
	if state, _ := h.b.retainedResult("first"); state != retainedMissing {
		t.Errorf("oldest retained state = %d, want missing after cap eviction", state)
	}
	time.Sleep(30 * time.Millisecond)
	if state, _ := h.b.retainedResult("third"); state != retainedMissing {
		t.Errorf("expired retained state = %d, want missing", state)
	}
}

func TestConcurrentCallersDoNotCrossTalk(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sock := serveOnTempSocket(t, h)

	const callers = 4
	type got struct {
		resp ampResponse
		err  error
	}
	results := make(chan got, callers)

	for i := range callers {
		conn := dialBridge(t, sock)
		if err := json.NewEncoder(conn).Encode(ampRequest{Text: "question"}); err != nil {
			t.Fatalf("send: %v", err)
		}
		go func() {
			var r ampResponse
			err := json.NewDecoder(conn).Decode(&r)
			results <- got{r, err}
		}()
		_ = i
	}

	waitFor(t, "all events to be pushed", func() bool { return len(h.notifications(t)) == callers })

	// Answer each id with a distinct body; every caller must get its own.
	want := map[string]string{}
	for _, n := range h.notifications(t) {
		params, _ := n["params"].(map[string]any)
		meta, _ := params["meta"].(map[string]any)
		id, _ := meta["request_id"].(string)
		answer := "answer-for-" + id
		want[id] = answer
		if res, _ := h.b.resolve(id, answer); res != resolveOK {
			t.Fatalf("resolve(%s) = %v", id, res)
		}
	}

	seen := map[string]bool{}
	for range callers {
		r := <-results
		if r.err != nil {
			t.Fatalf("caller decode: %v", r.err)
		}
		if want[r.resp.RequestID] != r.resp.Reply {
			t.Errorf("caller with id %s got %q, want %q",
				r.resp.RequestID, r.resp.Reply, want[r.resp.RequestID])
		}
		if seen[r.resp.Reply] {
			t.Errorf("two callers received the same answer %q", r.resp.Reply)
		}
		seen[r.resp.Reply] = true
	}
}

func TestPushEventCarriesTheThreadID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if _, _, err := h.b.pushEvent("q", "T-42"); err != nil {
		t.Fatalf("pushEvent: %v", err)
	}
	params, _ := h.notifications(t)[0]["params"].(map[string]any)
	meta, _ := params["meta"].(map[string]any)

	// Without this, ask_amp can only guess "whichever thread messaged us last",
	// which routes to the wrong thread when two Amp threads share one session.
	if meta["thread_id"] != "T-42" {
		t.Errorf("meta.thread_id = %v, want T-42", meta["thread_id"])
	}
}

func TestPushEventOmitsAnUnknownThreadID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	if _, _, err := h.b.pushEvent("q", ""); err != nil {
		t.Fatalf("pushEvent: %v", err)
	}
	params, _ := h.notifications(t)[0]["params"].(map[string]any)
	meta, _ := params["meta"].(map[string]any)
	if _, ok := meta["thread_id"]; ok {
		t.Error("an empty thread_id must be omitted, not sent as a blank attribute")
	}
}

// failWriter fails every write, standing in for a broken stdout.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errWriteFailed }

var errWriteFailed = errors.New("stdout is gone")

func TestPushEventUnwindsWhenTheEventCannotBeDelivered(t *testing.T) {
	t.Parallel()
	lg := &syncBuf{}
	b := newBridge(testConfig(), failWriter{}, lg)

	_, _, err := b.pushEvent("nobody will see this", "")
	if err == nil {
		t.Fatal("a failed send must be reported, not swallowed")
	}
	// Otherwise the caller blocks for the full timeout waiting on a message
	// Claude was never given.
	if n := b.pendingCount(); n != 0 {
		t.Errorf("pendingCount = %d, want 0 — the slot leaked", n)
	}
	if !strings.Contains(lg.String(), "EVENT_PUSH_FAILED") {
		t.Error("the delivery failure should be logged")
	}
	if strings.Contains(lg.String(), "EVENT_PUSHED") {
		t.Error("EVENT_PUSHED must not be logged for an event that never went out")
	}
}
