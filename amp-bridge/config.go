package main

import (
	"os"
	"strconv"
	"time"
)

// config is resolved once from the environment at startup and then passed
// explicitly. Nothing reads the environment after that point, which is what
// makes the wire behaviour reproducible under test: a test builds the config it
// wants instead of mutating process-global state.
type config struct {
	// maxInFlight separately caps unanswered channel events and background Amp
	// turns. Without it a runaway loop on either side floods the other session.
	maxInFlight int
	// maxMessageBytes caps a single message. Without it one large payload eats
	// the session's context window.
	maxMessageBytes int
	// replyWait is how long an Amp caller blocks waiting for Claude.
	replyWait time.Duration
	// maxReplyWait caps a timeout requested by an individual Amp caller. The
	// default remains short, while deep consultations can opt into a longer
	// bounded wait without restarting the Claude session.
	maxReplyWait time.Duration
	// resultTTL and maxResults bound the in-memory mailbox for replies that
	// arrive after their original caller timed out.
	resultTTL  time.Duration
	maxResults int
	// logBodies logs whole frames rather than frame shape. Off by default:
	// bodies carry conversation content.
	logBodies bool

	// ampBin is the Amp CLI used by the Claude->Amp tools when no inbox is live.
	ampBin string
	// ampTimeout bounds a single `amp threads continue` turn.
	ampTimeout time.Duration
	// sendTimeout bounds a send_amp turn, which blocks nobody and so has no
	// reason to share ask_amp's nesting budget. Two thirds of outbound requests
	// were being discarded at ampTimeout while Amp was still working.
	//
	// The ceiling is not arbitrary: the plugin clamps any budget we send to its
	// own MAX_TIMEOUT_MS (600 s, amp-bridge-inbox.ts). Past that the plugin
	// answers on its schedule while we wait on ours, so the configured number
	// would stop meaning anything. Keep the two in step.
	sendTimeout time.Duration
	// ampDisabled switches off the Claude->Amp direction entirely.
	ampDisabled bool
}

func loadConfig() config {
	replyWait := envDuration("AMP_BRIDGE_TIMEOUT", 180*time.Second)
	maxReplyWait := envDuration("AMP_BRIDGE_MAX_TIMEOUT", 15*time.Minute)
	maxReplyWait = max(maxReplyWait, replyWait)

	ampTimeout := envDuration("AMP_BRIDGE_AMP_TIMEOUT", 120*time.Second)
	// A send budget below the synchronous one would be a strict downgrade: the
	// asynchronous path is the one that can afford to wait.
	sendTimeout := max(envDuration("AMP_BRIDGE_SEND_TIMEOUT", 10*time.Minute), ampTimeout)

	return config{
		maxInFlight:     envInt("AMP_BRIDGE_MAX_INFLIGHT", 8),
		maxMessageBytes: envInt("AMP_BRIDGE_MAX_BYTES", 64*1024),
		replyWait:       replyWait,
		maxReplyWait:    maxReplyWait,
		resultTTL:       envDuration("AMP_BRIDGE_RESULT_TTL", time.Hour),
		maxResults:      envInt("AMP_BRIDGE_MAX_RESULTS", 64),
		logBodies:       os.Getenv("AMP_BRIDGE_LOG_BODIES") == "1",
		ampBin:          envStr("AMP_BIN", "amp"),
		ampTimeout:      ampTimeout,
		sendTimeout:     sendTimeout,
		ampDisabled:     os.Getenv("AMP_BRIDGE_DISABLE_OUTBOUND") == "1",
	}
}

func (c config) requestWait(milliseconds int64) time.Duration {
	if milliseconds <= 0 {
		return c.replyWait
	}
	maxMillis := c.maxReplyWait.Milliseconds()
	if milliseconds >= maxMillis {
		return c.maxReplyWait
	}
	return time.Duration(milliseconds) * time.Millisecond
}

// envInt reads a positive integer. Zero, negative and unparseable values fall
// back to the default rather than disabling a cap.
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
