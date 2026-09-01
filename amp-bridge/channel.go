package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Channel egress (bridge -> Claude) and Amp ingress (Amp -> bridge).

// ── egress: bridge -> Claude ────────────────────────────────────────────────

// pushEvent registers a pending request and pushes it to Claude as an
// unsolicited channel notification. The returned channel receives Claude's
// reply exactly once; the caller must drop the id if it stops waiting.
func (b *bridge) pushEvent(text, threadID string) (string, chan string, error) {
	return b.pushEventWithWait(text, threadID, b.cfg.replyWait)
}

func (b *bridge) pushEventWithWait(
	text, threadID string,
	wait time.Duration,
) (string, chan string, error) {
	b.pendMu.Lock()
	if len(b.pending) >= b.cfg.maxInFlight {
		n := len(b.pending)
		b.pendMu.Unlock()
		return "", nil, fmt.Errorf(
			"too many requests in flight (%d/%d); Claude has not answered the earlier ones yet",
			n, b.cfg.maxInFlight,
		)
	}
	b.seq++
	id := fmt.Sprintf("amp-%d-%d", time.Now().UnixNano(), b.seq)
	sink := make(chan string, 1)
	b.pending[id] = sink
	b.pendMu.Unlock()

	// It is `meta`, NOT the MCP-standard `_meta`. Keys must be identifiers and
	// values render as attributes on the <channel> tag. Do NOT use "source" —
	// Claude sets that attribute itself, and a duplicate emits it twice.
	// Confirmed live: <channel source="amp-bridge" request_id="..." source="amp">
	meta := map[string]string{
		"request_id": id,
		"timeout_ms": strconv.FormatInt(wait.Milliseconds(), 10),
	}
	// Carrying the originating thread lets Claude answer the right Amp thread
	// with ask_amp when several share one session, instead of relying on
	// "whichever messaged us last".
	if threadID != "" {
		meta["thread_id"] = threadID
	}

	if err := b.pushChannelNotification(text, meta); err != nil {
		// The event never reached Claude, so nothing will ever answer it.
		// Holding the slot would cost the caller the full timeout for a message
		// that was never delivered.
		b.drop(id)
		b.logf("EVENT_PUSH_FAILED request_id=%s %v", id, err)
		return "", nil, fmt.Errorf("could not deliver the event to Claude: %w", err)
	}
	b.logf("EVENT_PUSHED request_id=%s bytes=%d", id, len(text))
	return id, sink, nil
}

// pushChannelNotification sends an unsolicited event without assuming it needs
// a reply. Amp->Claude requests add request_id and own a pending slot in
// pushEvent; send_amp completions deliberately do neither.
func (b *bridge) pushChannelNotification(content string, meta map[string]string) error {
	return b.send(rpc{
		Method: "notifications/claude/channel",
		Params: mustJSON(map[string]any{"content": content, "meta": meta}),
	})
}

func (b *bridge) drop(id string) {
	b.pendMu.Lock()
	delete(b.pending, id)
	b.pendMu.Unlock()
}

type retainedReply struct {
	text      string
	expiresAt time.Time
}

func (b *bridge) expire(id string) (time.Time, bool) {
	b.pendMu.Lock()
	defer b.pendMu.Unlock()

	if _, ok := b.pending[id]; !ok {
		return time.Time{}, false
	}
	delete(b.pending, id)
	b.pruneRetainedLocked(time.Now())
	expires := time.Now().Add(b.cfg.resultTTL)
	b.retained[id] = retainedReply{expiresAt: expires}
	b.trimRetainedLocked()
	return expires, true
}

func (b *bridge) pruneRetainedLocked(now time.Time) {
	for id, result := range b.retained {
		if !now.Before(result.expiresAt) {
			delete(b.retained, id)
		}
	}
}

func (b *bridge) trimRetainedLocked() {
	for len(b.retained) > b.cfg.maxResults {
		var oldestID string
		var oldestExpiry time.Time
		for id, result := range b.retained {
			if oldestID == "" || result.expiresAt.Before(oldestExpiry) {
				oldestID, oldestExpiry = id, result.expiresAt
			}
		}
		delete(b.retained, oldestID)
	}
}

type retainedState int

const (
	retainedMissing retainedState = iota
	retainedWaiting
	retainedReady
)

func (b *bridge) retainedResult(id string) (retainedState, retainedReply) {
	b.pendMu.Lock()
	defer b.pendMu.Unlock()
	b.pruneRetainedLocked(time.Now())

	result, ok := b.retained[id]
	if !ok {
		return retainedMissing, retainedReply{}
	}
	if result.text == "" {
		return retainedWaiting, result
	}
	return retainedReady, result
}

type resolveResult int

const (
	resolveOK resolveResult = iota
	resolveStoredLate
	resolveUnknownID
	resolveMissingID
)

func (r resolveResult) String() string {
	switch r {
	case resolveOK:
		return "ok"
	case resolveStoredLate:
		return "stored-late"
	case resolveUnknownID:
		return "unknown-id"
	case resolveMissingID:
		return "missing-id"
	default:
		return "unknown"
	}
}

// resolve routes Claude's reply to the Amp request waiting on it.
//
// request_id is always required. An earlier version accepted an omitted id when
// exactly one request happened to be pending, which is unsound across a timeout
// boundary: A times out and is dropped, B becomes the sole pending request, and
// a belated id-less answer meant for A is handed to B. The tool schema marks
// request_id required, so inferring it only ever serves a client that is already
// violating the contract — at the cost of the exact mis-routing this correlation
// exists to prevent.
func (b *bridge) resolve(id, text string) (resolveResult, string) {
	b.pendMu.Lock()
	defer b.pendMu.Unlock()

	if id == "" {
		return resolveMissingID, ""
	}
	b.pruneRetainedLocked(time.Now())
	sink, ok := b.pending[id]
	if !ok {
		if result, retained := b.retained[id]; retained {
			if result.text != "" {
				return resolveUnknownID, id
			}
			result.text = truncateMessage(text, b.cfg.maxMessageBytes)
			b.retained[id] = result
			return resolveStoredLate, id
		}
		return resolveUnknownID, id
	}
	delete(b.pending, id)
	// Buffered and delivered once; the default arm keeps a vanished waiter from
	// blocking the handler.
	select {
	case sink <- text:
	default:
	}
	return resolveOK, id
}

func (b *bridge) pendingCount() int {
	b.pendMu.Lock()
	defer b.pendMu.Unlock()
	return len(b.pending)
}

// ── ingress: Amp -> bridge ──────────────────────────────────────────────────

type ampRequest struct {
	Text          string `json:"text,omitempty"`
	TimeoutMillis int64  `json:"timeout_ms,omitempty"`
	ResultID      string `json:"result_id,omitempty"`
	// Optional: lets Claude reach back into this Amp thread via outbound tools.
	ThreadID string `json:"thread_id,omitempty"`
}

type ampResponse struct {
	RequestID string `json:"request_id"`
	Reply     string `json:"reply,omitempty"`
	Error     string `json:"error,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

func (b *bridge) serveSocket(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Accept fails permanently once the listener is closed, which is
			// the normal shutdown path.
			b.logf("SOCKET_ACCEPT_ERROR %v", err)
			return
		}
		go b.handleAmpConn(conn)
	}
}

// handleAmpConn reads requests continuously and answers each in its own
// goroutine. Reading never blocks on a pending reply, which is what lets us
// notice the caller hanging up: the decode loop returns immediately on EOF and
// closes `gone`, so in-flight waits are abandoned instead of holding a slot for
// the full timeout.
func (b *bridge) handleAmpConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	// One misbehaving caller must not take the channel down for the session.
	b.guard("amp connection", func() { b.serveAmpConn(conn) })
}

func (b *bridge) serveAmpConn(conn net.Conn) {
	var (
		enc    = json.NewEncoder(conn)
		encMu  sync.Mutex
		wg     sync.WaitGroup
		gone   = make(chan struct{})
		closed sync.Once
	)
	respond := func(r ampResponse) {
		encMu.Lock()
		defer encMu.Unlock()
		if err := enc.Encode(r); err != nil {
			b.logf("AMP_WRITE_ERROR request_id=%s %v", r.RequestID, err)
		}
	}

	dec := json.NewDecoder(conn)
	for {
		var req ampRequest
		if err := dec.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "use of closed") {
				b.logf("AMP_DECODE_ERROR %v", err)
			}
			closed.Do(func() { close(gone) })
			break
		}
		if req.ResultID != "" {
			if strings.TrimSpace(req.Text) != "" || req.ThreadID != "" || req.TimeoutMillis != 0 {
				respond(ampResponse{
					RequestID: req.ResultID,
					Error:     "result lookup cannot include text, thread_id, or timeout_ms",
				})
				continue
			}
			respond(b.retainedResponse(req.ResultID))
			continue
		}
		b.rememberThread(req.ThreadID)

		if msg, ok := b.rejectRequest(req); !ok {
			respond(ampResponse{Error: msg})
			continue
		}

		wait := b.cfg.requestWait(req.TimeoutMillis)
		id, sink, err := b.pushEventWithWait(req.Text, req.ThreadID, wait)
		if err != nil {
			b.logf("AMP_REJECTED %v", err)
			respond(ampResponse{Error: err.Error()})
			continue
		}
		b.logf("AMP_REQUEST bytes=%d timeout=%s inflight=%d", len(req.Text), wait, b.pendingCount())

		wg.Add(1)
		go func(id string, sink chan string, wait time.Duration) {
			defer wg.Done()
			b.guard("reply waiter "+id, func() { b.awaitReply(id, sink, wait, gone, respond) })
		}(id, sink, wait)
	}
	wg.Wait()
}

// rejectRequest applies the cheap validity checks that do not need a slot.
func (b *bridge) rejectRequest(req ampRequest) (string, bool) {
	switch {
	case strings.TrimSpace(req.Text) == "":
		return "empty text", false
	case len(req.Text) > b.cfg.maxMessageBytes:
		b.logf("AMP_REJECTED oversized bytes=%d max=%d", len(req.Text), b.cfg.maxMessageBytes)
		return fmt.Sprintf("message too large: %d bytes (max %d)",
			len(req.Text), b.cfg.maxMessageBytes), false
	}
	return "", true
}

// awaitReply blocks on one of three outcomes: Claude answers, the deadline
// passes, or the Amp caller disconnects. All three release the slot.
func (b *bridge) awaitReply(
	id string,
	sink chan string,
	wait time.Duration,
	gone <-chan struct{},
	respond func(ampResponse),
) {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case r := <-sink:
		respond(ampResponse{RequestID: id, Reply: r})
		b.logf("AMP_REPLIED request_id=%s", id)
	case <-timer.C:
		// expire takes the same lock resolve holds while it buffers into sink, so
		// once it returns any delivery that won the race is already visible.
		// Without this re-check Claude is told "delivered" while Amp is told
		// "timed out", and the answer is lost in between.
		expires, retained := b.expire(id)
		if !retained {
			select {
			case r := <-sink:
				respond(ampResponse{RequestID: id, Reply: r})
				b.logf("AMP_REPLIED request_id=%s (arrived at the deadline)", id)
			default:
				respond(ampResponse{RequestID: id, Error: "timed out waiting for Claude"})
				b.logf("AMP_TIMEOUT request_id=%s", id)
			}
		} else {
			respond(ampResponse{
				RequestID: id,
				Error:     "timed out waiting for Claude; a late reply will be retained",
				ExpiresAt: expires.UTC().Format(time.RFC3339),
			})
			b.logf("AMP_TIMEOUT request_id=%s retained_until=%s", id, expires.UTC().Format(time.RFC3339))
		}
	case <-gone:
		b.drop(id)
		b.logf("AMP_ABANDONED request_id=%s (caller disconnected)", id)
	}
}

func (b *bridge) retainedResponse(id string) ampResponse {
	state, result := b.retainedResult(id)
	switch state {
	case retainedReady:
		b.logf("AMP_RESULT request_id=%s outcome=ready", id)
		return ampResponse{
			RequestID: id,
			Reply:     result.text,
			ExpiresAt: result.expiresAt.UTC().Format(time.RFC3339),
		}
	case retainedWaiting:
		return ampResponse{
			RequestID: id,
			Error:     "Claude has not supplied a late reply yet",
			ExpiresAt: result.expiresAt.UTC().Format(time.RFC3339),
		}
	default:
		return ampResponse{
			RequestID: id,
			Error: "no retained request matches this id; it may have expired or the bridge " +
				"process may have restarted",
		}
	}
}
