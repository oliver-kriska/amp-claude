package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// The Amp plugin inbox: the path by which Claude's outbound tools reach a thread
// that is *open* in an interactive Amp session.
//
// Amp permits one executor per thread, so `amp threads continue --execute`
// cannot attach to a thread somebody is sitting in — see §18 of the research
// notes. A plugin loaded inside that session can append to it, though, and §20
// records that mechanism working. This file is the client half: it finds the
// plugin's socket, checks it is really serving the thread we want, and speaks
// the newline-delimited JSON protocol the plugin implements.
//
// Absence is not an error. When no inbox exists, delivery falls through to the
// CLI and behaves exactly as it did before this file existed.

const (
	// inboxProto is the wire version this build speaks. A plugin announcing a
	// different one is refused loudly rather than misparsed.
	inboxProto = 1

	inboxDialTimeout   = 5 * time.Second
	inboxStatusTimeout = 3 * time.Second

	// The plugin's own deadline sits this far inside ours, so its richer
	// diagnostic ("thread is awaiting-approval") wins the race against our bare
	// read deadline instead of arriving after we have given up.
	inboxTimeoutLead = 10 * time.Second
)

// inboxEntry is one enabled thread, written by the plugin and read by us.
type inboxEntry struct {
	ThreadID  string `json:"thread_id"`
	Socket    string `json:"socket"`
	PluginPID int    `json:"plugin_pid"`
	Proto     int    `json:"proto"`
	EnabledAt string `json:"enabled_at"`
	Note      string `json:"note"`
}

type inboxAskRequest struct {
	Op        string `json:"op"`
	ID        string `json:"id"`
	ThreadID  string `json:"thread_id"`
	Text      string `json:"text"`
	From      string `json:"from"`
	TimeoutMS int64  `json:"timeout_ms"`
}

type inboxStatusRequest struct {
	Op string `json:"op"`
}

type inboxStatusReply struct {
	PID            int      `json:"pid"`
	Proto          int      `json:"proto"`
	EnabledThreads []string `json:"enabled_threads"`
	StartedAt      string   `json:"started_at"`
}

type inboxReply struct {
	ID    string `json:"id"`
	Reply string `json:"reply"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

// trustedSubdir applies the runtime directory's own predicate one level down.
//
// The plugin writes these directories from TypeScript, so the code cannot be
// shared with identity.go — but the checks must not diverge, or the plugin
// could create exactly the states this side is written to refuse. Ownership is
// the load-bearing half: mode 0700 on a directory belonging to someone else
// passes any permissions check and is still theirs.
func trustedSubdir(parent, name string) (string, error) {
	path := filepath.Join(parent, name)
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink — refusing to use it", path)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s exists but is not a directory", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if uid := os.Getuid(); int(st.Uid) != uid {
			return "", fmt.Errorf(
				"%s is owned by uid %d, not %d — refusing to use it", path, st.Uid, uid,
			)
		}
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf(
			"%s is mode %04o — group/other access must be off (chmod 700 %s)",
			path, perm, path,
		)
	}
	return path, nil
}

// trustedInboxDir returns <runtime>/inbox, or an error if either level fails the
// trust predicate.
//
// inbox/ is a subdirectory rather than a set of files beside the bridge
// registrations for a specific reason: listBridges globs <runtime>/*.json and
// glob does not descend. Flat inbox files would be parsed as phantom bridge
// sessions.
func trustedInboxDir() (string, error) {
	dir, err := trustedRuntimeDir()
	if err != nil {
		return "", err
	}
	return trustedSubdir(dir, "inbox")
}

// newRequestID returns a short random suffix used for inbox requests and async
// handles. Randomness keeps ids distinct across concurrent bridge processes.
func newRequestID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("request id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// inboxStatus performs the status handshake on its own short-lived connection.
//
// Deliberately separate from the ask connection. The fallback rule in delivery
// turns on whether any `ask` bytes could have been accepted; probing and
// delivering over one connection would make that unprovable after the fact.
func inboxStatus(ctx context.Context, sock string) (inboxStatusReply, error) {
	var out inboxStatusReply
	dialCtx, cancel := context.WithTimeout(ctx, inboxDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "unix", sock)
	if err != nil {
		return out, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(inboxStatusTimeout)); err != nil {
		return out, err
	}
	if err := json.NewEncoder(conn).Encode(inboxStatusRequest{Op: "status"}); err != nil {
		return out, err
	}
	if err := json.NewDecoder(conn).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

// lookupInbox reports whether threadID has a live plugin inbox.
//
// Three outcomes, and the difference between the second and third matters:
//   - (entry, true, nil)  — a plugin is serving this thread right now
//   - (_, false, nil)     — no inbox; the caller should use the CLI path
//   - (_, false, err)     — the runtime directory could not be trusted
//
// The last is not "nothing is running". Reporting a refusal as an absence would
// silently downgrade a security decision into a fallback.
func (b *bridge) lookupInbox(threadID string) (inboxEntry, bool, error) {
	dir, err := trustedInboxDir()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return inboxEntry{}, false, nil // no plugin has ever enabled an inbox
		}
		return inboxEntry{}, false, err
	}
	threads, err := trustedSubdir(dir, "threads")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return inboxEntry{}, false, nil
		}
		return inboxEntry{}, false, err
	}
	// Validated before it becomes a path component. The leading-alnum anchor is
	// what rules out "..", so this is a path-safety check as well as a sanity one.
	if !threadIDRE.MatchString(threadID) {
		return inboxEntry{}, false, fmt.Errorf("implausible Amp thread id %q", threadID)
	}

	path := filepath.Join(threads, threadID+".json")
	// #nosec G304 -- threadID is matched against threadIDRE above and the
	// directory is our own 0700 runtime tree.
	data, err := os.ReadFile(path)
	if err != nil {
		// Absence by design: no entry for this thread means no inbox, which is
		// the ordinary case and must degrade to the CLI path rather than fail.
		//nolint:nilerr // a missing or unreadable entry is an absence, not an error
		return inboxEntry{}, false, nil
	}
	var e inboxEntry
	if err := json.Unmarshal(data, &e); err != nil {
		b.logf("INBOX_UNREADABLE thread=%s path=%s %v", threadID, path, err)
		return inboxEntry{}, false, nil
	}

	// Tier one: is anything listening at all?
	if !socketIsLive(e.Socket) {
		b.logf("INBOX_STALE thread=%s socket=%s pid=%d — sweeping", threadID, e.Socket, e.PluginPID)
		_ = os.Remove(path)
		return inboxEntry{}, false, nil
	}

	// Tier two: is it serving *this* thread? onDispose does not run on SIGKILL,
	// and a pid-named socket path could in principle be reused, so a live socket
	// is not by itself proof that our thread is enabled behind it.
	st, err := inboxStatus(b.ctx, e.Socket)
	if err != nil {
		b.logf("INBOX_UNRESPONSIVE thread=%s socket=%s %v", threadID, e.Socket, err)
		return inboxEntry{}, false, nil
	}
	if st.Proto != inboxProto {
		return inboxEntry{}, false, fmt.Errorf(
			"the installed Amp plugin speaks inbox protocol v%d, this bridge expects v%d — "+
				"run `amp-bridge init --amp-plugin` and reload the plugin in Amp",
			st.Proto, inboxProto,
		)
	}
	if !slices.Contains(st.EnabledThreads, threadID) {
		b.logf("INBOX_MISMATCH thread=%s socket=%s serves=%v", threadID, e.Socket, st.EnabledThreads)
		return inboxEntry{}, false, nil
	}
	return e, true, nil
}

// askViaInbox delivers one question through the plugin and waits for the answer.
//
// There is no retry and no fallback here, by design: once the ask frame has been
// written the message may already be in the thread, so a second attempt risks
// delivering it twice. Every failure is reported, never papered over.
func (b *bridge) askViaInbox(e inboxEntry, threadID, text string) (string, error) {
	id, err := newRequestID()
	if err != nil {
		return "", err
	}

	dialCtx, cancel := context.WithTimeout(b.ctx, inboxDialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "unix", e.Socket)
	if err != nil {
		// Nothing was written, so this is still a safe place to fall back from.
		return "", fmt.Errorf("plugin inbox for %s did not accept a connection: %w", threadID, err)
	}
	defer func() { _ = conn.Close() }()
	// A deadline alone does not react when the bridge shuts down. Close the
	// connection on context cancellation so synchronous and background asks
	// unwind instead of holding shutdown until the socket deadline.
	stopCancel := context.AfterFunc(b.ctx, func() { _ = conn.Close() })
	defer stopCancel()

	// Slightly past the plugin's own deadline, so its diagnosis arrives first.
	if err := conn.SetDeadline(time.Now().Add(b.cfg.ampTimeout + inboxTimeoutLead)); err != nil {
		return "", err
	}

	pluginBudget := max(b.cfg.ampTimeout-inboxTimeoutLead, time.Second)
	req := inboxAskRequest{
		Op:        "ask",
		ID:        id,
		ThreadID:  threadID,
		Text:      text,
		From:      b.identityName(),
		TimeoutMS: pluginBudget.Milliseconds(),
	}
	b.logf("INBOX_ASK thread=%s req=%s pid=%d bytes=%d", threadID, id, e.PluginPID, len(text))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		if b.ctx.Err() != nil {
			return "", errors.New("bridge is shutting down; the Amp turn was cancelled")
		}
		// Ambiguous by construction: some bytes may have been accepted.
		return "", fmt.Errorf(
			"writing to the plugin inbox failed partway (%w). The message may or may not "+
				"have reached thread %s — check it before resending", err, threadID,
		)
	}

	var r inboxReply
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		if b.ctx.Err() != nil {
			return "", errors.New("bridge is shutting down; the Amp turn was cancelled")
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "", fmt.Errorf(
				"the plugin inbox closed mid-request (the Amp session may have exited or "+
					"reloaded its plugins). The message may or may not have reached thread %s "+
					"— check it before resending", threadID,
			)
		}
		return "", fmt.Errorf("reading the plugin inbox reply failed: %w", err)
	}
	if r.Error != "" {
		b.logf("INBOX_FAILED thread=%s req=%s code=%s", threadID, id, r.Code)
		return "", inboxCodeError(r, threadID)
	}
	b.logf("INBOX_OK thread=%s req=%s bytes=%d", threadID, id, len(r.Reply))
	return r.Reply, nil
}

// inboxCodeError turns the plugin's machine code into something an operator can
// act on. The plugin's own message is preserved; the code decides what to add.
func inboxCodeError(r inboxReply, threadID string) error {
	switch r.Code {
	case "not-enabled":
		return fmt.Errorf(
			"thread %s has not enabled its Claude inbox — press Ctrl+O in that Amp session "+
				"and run 'amp-bridge: Enable Claude inbox for this thread'", threadID,
		)
	case "busy":
		return fmt.Errorf(
			"the Claude inbox for %s already has requests queued — Amp has not finished the "+
				"earlier turns yet", threadID,
		)
	case "disabled":
		return fmt.Errorf(
			"the Claude inbox for %s was disabled or the plugin reloaded while the request "+
				"was in flight. The message may or may not have reached the thread — check it "+
				"before resending", threadID,
		)
	case "no-turn":
		// Distinct from a timeout: the question is in the thread, so resending
		// it duplicates rather than retries. Say that plainly — the caller
		// cannot see the thread and would otherwise assume nothing landed.
		return fmt.Errorf(
			"%s — do not resend the same question, it is already there", r.Error,
		)
	case "turn-error", "turn-cancelled":
		return fmt.Errorf("the Amp turn %s before answering — check thread %s",
			strings.TrimPrefix(r.Code, "turn-"), threadID)
	case "timeout":
		return fmt.Errorf("the Amp turn did not finish in time: %s", r.Error)
	case "append-failed":
		return fmt.Errorf("the plugin could not append to thread %s: %s", threadID, r.Error)
	default:
		if r.Code != "" {
			return fmt.Errorf("plugin inbox error (%s): %s", r.Code, r.Error)
		}
		return fmt.Errorf("plugin inbox error: %s", r.Error)
	}
}

// identityName is the label the Amp side sees on an incoming message. With
// several bridges live across different projects, "which Claude is asking" is
// not a detail.
func (b *bridge) identityName() string {
	b.regMu.Lock()
	defer b.regMu.Unlock()
	if b.reg.Name != "" {
		return b.reg.Name
	}
	return "claude-code"
}
