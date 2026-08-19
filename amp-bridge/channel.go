package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Channel egress (bridge -> Claude) and Amp ingress (Amp -> bridge).

// ── egress: bridge -> Claude ────────────────────────────────────────────────

// pushEvent registers a pending request and pushes it to Claude as an
// unsolicited channel notification. The returned channel receives Claude's
// reply exactly once; the caller must drop the id if it stops waiting.
func (b *bridge) pushEvent(text string) (string, chan string, error) {
	b.pendMu.Lock()
	if len(b.pending) >= b.cfg.maxInFlight {
		n := len(b.pending)
		b.pendMu.Unlock()
		return "", nil, fmt.Errorf(
			"too many requests in flight (%d/%d); Claude has not answered the earlier ones yet",
			n, b.cfg.maxInFlight)
	}
	b.seq++
	id := fmt.Sprintf("amp-%d-%d", time.Now().UnixNano(), b.seq)
	sink := make(chan string, 1)
	b.pending[id] = sink
	b.pendMu.Unlock()

	b.send(rpc{
		Method: "notifications/claude/channel",
		Params: mustJSON(map[string]any{
			"content": text,
			// It is `meta`, NOT the MCP-standard `_meta`. Keys must be
			// identifiers and values render as attributes on the <channel> tag.
			// Do NOT use "source" — Claude sets that attribute itself, and a
			// duplicate emits it twice. Confirmed live:
			//   <channel source="amp-bridge" request_id="..." source="amp">
			"meta": map[string]string{"request_id": id},
		}),
	})
	b.logf("EVENT_PUSHED request_id=%s bytes=%d", id, len(text))
	return id, sink, nil
}

func (b *bridge) drop(id string) {
	b.pendMu.Lock()
	delete(b.pending, id)
	b.pendMu.Unlock()
}

type resolveResult int

const (
	resolveOK resolveResult = iota
	resolveUnknownID
	resolveAmbiguous
)

func (r resolveResult) String() string {
	switch r {
	case resolveOK:
		return "ok"
	case resolveUnknownID:
		return "unknown-id"
	case resolveAmbiguous:
		return "ambiguous"
	default:
		return "unknown"
	}
}

// resolve routes Claude's reply to the Amp request waiting on it.
//
// request_id is required whenever more than one request is in flight. Falling
// back to "the most recent" would silently mis-route concurrent calls, so an
// omitted id is honoured only when exactly one request is pending.
func (b *bridge) resolve(id, text string) (resolveResult, string) {
	b.pendMu.Lock()
	defer b.pendMu.Unlock()

	if id == "" {
		if len(b.pending) != 1 {
			return resolveAmbiguous, ""
		}
		for k := range b.pending {
			id = k
		}
	}
	sink, ok := b.pending[id]
	if !ok {
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
	Text string `json:"text"`
	// Optional: lets Claude reach back into this Amp thread via ask_amp.
	ThreadID string `json:"thread_id,omitempty"`
}

type ampResponse struct {
	RequestID string `json:"request_id"`
	Reply     string `json:"reply,omitempty"`
	Error     string `json:"error,omitempty"`
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
		b.rememberThread(req.ThreadID)

		if msg, ok := b.rejectRequest(req); !ok {
			respond(ampResponse{Error: msg})
			continue
		}

		id, sink, err := b.pushEvent(req.Text)
		if err != nil {
			b.logf("AMP_REJECTED %v", err)
			respond(ampResponse{Error: err.Error()})
			continue
		}
		b.logf("AMP_REQUEST bytes=%d inflight=%d", len(req.Text), b.pendingCount())

		wg.Add(1)
		go func(id string, sink chan string) {
			defer wg.Done()
			b.awaitReply(id, sink, gone, respond)
		}(id, sink)
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
func (b *bridge) awaitReply(id string, sink chan string, gone <-chan struct{}, respond func(ampResponse)) {
	timer := time.NewTimer(b.cfg.replyWait)
	defer timer.Stop()

	select {
	case r := <-sink:
		respond(ampResponse{RequestID: id, Reply: r})
		b.logf("AMP_REPLIED request_id=%s", id)
	case <-timer.C:
		b.drop(id)
		respond(ampResponse{RequestID: id, Error: "timed out waiting for Claude"})
		b.logf("AMP_TIMEOUT request_id=%s", id)
	case <-gone:
		b.drop(id)
		b.logf("AMP_ABANDONED request_id=%s (caller disconnected)", id)
	}
}
