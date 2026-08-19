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
	// maxInFlight caps unanswered channel events. Without it a runaway loop on
	// the Amp side floods the Claude session.
	maxInFlight int
	// maxMessageBytes caps a single message. Without it one large payload eats
	// the session's context window.
	maxMessageBytes int
	// replyWait is how long an Amp caller blocks waiting for Claude.
	replyWait time.Duration
	// logBodies logs whole frames rather than frame shape. Off by default:
	// bodies carry conversation content.
	logBodies bool

	// ampBin is the Amp CLI invoked by the ask_amp tool.
	ampBin string
	// ampTimeout bounds a single `amp threads continue` turn.
	ampTimeout time.Duration
	// ampDisabled switches off the Claude->Amp direction entirely.
	ampDisabled bool
}

func loadConfig() config {
	return config{
		maxInFlight:     envInt("AMP_BRIDGE_MAX_INFLIGHT", 8),
		maxMessageBytes: envInt("AMP_BRIDGE_MAX_BYTES", 64*1024),
		replyWait:       envDuration("AMP_BRIDGE_TIMEOUT", 180*time.Second),
		logBodies:       os.Getenv("AMP_BRIDGE_LOG_BODIES") == "1",
		ampBin:          envStr("AMP_BIN", "amp"),
		ampTimeout:      envDuration("AMP_BRIDGE_AMP_TIMEOUT", 300*time.Second),
		ampDisabled:     os.Getenv("AMP_BRIDGE_DISABLE_OUTBOUND") == "1",
	}
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
