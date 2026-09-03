package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want options
	}{
		{"no arguments means serve", nil, options{mode: modeServe, dir: "."}},
		{"list", []string{"--list"}, options{mode: modeList, dir: "."}},
		{"verbose list", []string{"--list", "--verbose"}, options{mode: modeList, dir: ".", verbose: true}},
		{"verbose before list", []string{"--verbose", "--list"}, options{mode: modeList, dir: ".", verbose: true}},
		{"help", []string{"--help"}, options{mode: modeHelp, dir: "."}},
		{"version", []string{"--version"}, options{mode: modeVersion, dir: "."}},
		{"ask", []string{"--ask", "hello"}, options{mode: modeAsk, text: "hello", dir: "."}},
		{
			"ask with timeout",
			[]string{"--timeout", "10m", "--ask", "hello"},
			options{mode: modeAsk, text: "hello", timeout: 10 * time.Minute, dir: "."},
		},
		{
			"late result",
			[]string{"--session", "b1", "--result", "amp-123-1"},
			options{mode: modeResult, session: "b1", requestID: "amp-123-1", dir: "."},
		},
		{
			"everything after --ask is the message",
			[]string{"--ask", "what", "is", "2+2?"},
			options{mode: modeAsk, text: "what is 2+2?", dir: "."},
		},
		{
			"session and thread",
			[]string{"--session", "b1", "--thread", "T-01a01877-2274-734d-8306-7c37b33f2a7f", "--ask", "hi"},
			options{
				mode:    modeAsk,
				session: "b1",
				thread:  "T-01a01877-2274-734d-8306-7c37b33f2a7f",
				text:    "hi",
				dir:     ".",
			},
		},
		{
			"flags after --ask belong to the message",
			[]string{"--ask", "use --list to see them"},
			options{mode: modeAsk, text: "use --list to see them", dir: "."},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseArgs(tc.args)
			if err != nil {
				t.Fatalf("parseArgs(%v) = %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseArgsErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		// A typo'd flag used to be ignored, which looked like the feature
		// silently failing rather than a mistake in the command.
		{"unknown flag", []string{"--sesion", "b1"}, "unknown argument"},
		{"stray value", []string{"hello"}, "unknown argument"},
		{"session without a value", []string{"--session"}, "needs a value"},
		{"thread without a value", []string{"--thread"}, "needs a value"},
		{"timeout without a value", []string{"--timeout"}, "needs a value"},
		{"invalid timeout", []string{"--timeout", "later", "--ask", "hello"}, "duration"},
		{"sub-millisecond timeout", []string{"--timeout", "1ns", "--ask", "hello"}, "at least 1ms"},
		{"timeout without ask", []string{"--timeout", "10m"}, "only valid with --ask"},
		{"result then ask", []string{"--result", "amp-1", "--ask", "hello"}, "cannot be combined"},
		{"list then ask", []string{"--list", "--ask", "hello"}, "cannot be combined"},
		{"result then list", []string{"--result", "amp-1", "--list"}, "cannot be combined"},
		{"result without a value", []string{"--result"}, "needs a value"},
		{"result with an empty value", []string{"--result", "  "}, "non-empty request id"},
		{
			"thread with result",
			[]string{"--thread", "T-01a01877-2274-734d-8306-7c37b33f2a7f", "--result", "amp-1"},
			"not valid with --result",
		},
		{"ask without a message", []string{"--ask"}, "needs a message"},
		{"ask with only spaces", []string{"--ask", "   "}, "needs a message"},
		{"verbose without list", []string{"--verbose"}, "only valid with --list"},
		{"verbose ask", []string{"--verbose", "--ask", "hello"}, "only valid with --list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseArgs(tc.args)
			if err == nil {
				t.Fatalf("parseArgs(%v) should have failed", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestEnvInt(t *testing.T) {
	tests := []struct {
		name, set string
		want      int
	}{
		{"unset falls back", "", 7},
		{"valid", "12", 12},
		{"garbage falls back", "banana", 7},
		// A cap of zero or below would disable the protection entirely, which
		// is never what an operator means by setting the variable.
		{"zero falls back", "0", 7},
		{"negative falls back", "-3", 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AMP_BRIDGE_TEST_INT", tc.set)
			if got := envInt("AMP_BRIDGE_TEST_INT", 7); got != tc.want {
				t.Errorf("envInt = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEnvDuration(t *testing.T) {
	tests := []struct {
		name, set string
		want      time.Duration
	}{
		{"unset falls back", "", time.Minute},
		{"valid", "90s", 90 * time.Second},
		{"garbage falls back", "soon", time.Minute},
		{"zero falls back", "0s", time.Minute},
		{"negative falls back", "-5s", time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AMP_BRIDGE_TEST_DUR", tc.set)
			if got := envDuration("AMP_BRIDGE_TEST_DUR", time.Minute); got != tc.want {
				t.Errorf("envDuration = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEnvStr(t *testing.T) {
	t.Setenv("AMP_BRIDGE_TEST_STR", "")
	if got := envStr("AMP_BRIDGE_TEST_STR", "fallback"); got != "fallback" {
		t.Errorf("envStr = %q, want fallback", got)
	}
	t.Setenv("AMP_BRIDGE_TEST_STR", "set")
	if got := envStr("AMP_BRIDGE_TEST_STR", "fallback"); got != "set" {
		t.Errorf("envStr = %q, want set", got)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	for _, k := range []string{
		"AMP_BRIDGE_MAX_INFLIGHT", "AMP_BRIDGE_MAX_BYTES", "AMP_BRIDGE_TIMEOUT",
		"AMP_BRIDGE_MAX_TIMEOUT", "AMP_BRIDGE_RESULT_TTL", "AMP_BRIDGE_MAX_RESULTS",
		"AMP_BRIDGE_LOG_BODIES", "AMP_BIN", "AMP_BRIDGE_AMP_TIMEOUT",
		"AMP_BRIDGE_DISABLE_OUTBOUND",
	} {
		t.Setenv(k, "")
	}

	got := loadConfig()
	want := config{
		maxInFlight:     8,
		maxMessageBytes: 64 * 1024,
		replyWait:       180 * time.Second,
		maxReplyWait:    15 * time.Minute,
		resultTTL:       time.Hour,
		maxResults:      64,
		logBodies:       false,
		ampBin:          "amp",
		ampTimeout:      120 * time.Second,
		sendTimeout:     10 * time.Minute,
		ampDisabled:     false,
	}
	if got != want {
		t.Errorf("loadConfig() = %+v, want %+v", got, want)
	}
}

func TestLoadConfigFromEnvironment(t *testing.T) {
	t.Setenv("AMP_BRIDGE_MAX_INFLIGHT", "3")
	t.Setenv("AMP_BRIDGE_MAX_BYTES", "2048")
	t.Setenv("AMP_BRIDGE_TIMEOUT", "5s")
	t.Setenv("AMP_BRIDGE_MAX_TIMEOUT", "20s")
	t.Setenv("AMP_BRIDGE_RESULT_TTL", "30m")
	t.Setenv("AMP_BRIDGE_MAX_RESULTS", "12")
	t.Setenv("AMP_BRIDGE_LOG_BODIES", "1")
	t.Setenv("AMP_BIN", "/usr/local/bin/amp")
	t.Setenv("AMP_BRIDGE_AMP_TIMEOUT", "30s")
	t.Setenv("AMP_BRIDGE_SEND_TIMEOUT", "4m")
	t.Setenv("AMP_BRIDGE_DISABLE_OUTBOUND", "1")

	got := loadConfig()
	want := config{
		maxInFlight:     3,
		maxMessageBytes: 2048,
		replyWait:       5 * time.Second,
		maxReplyWait:    20 * time.Second,
		resultTTL:       30 * time.Minute,
		maxResults:      12,
		logBodies:       true,
		ampBin:          "/usr/local/bin/amp",
		ampTimeout:      30 * time.Second,
		sendTimeout:     4 * time.Minute,
		ampDisabled:     true,
	}
	if got != want {
		t.Errorf("loadConfig() = %+v, want %+v", got, want)
	}
}

func TestRequestWait(t *testing.T) {
	t.Parallel()
	cfg := config{replyWait: 3 * time.Minute, maxReplyWait: 15 * time.Minute}
	for _, tc := range []struct {
		name         string
		milliseconds int64
		want         time.Duration
	}{
		{"default", 0, 3 * time.Minute},
		{"requested", int64((10 * time.Minute) / time.Millisecond), 10 * time.Minute},
		{"clamped", int64((20 * time.Minute) / time.Millisecond), 15 * time.Minute},
		{"overflow safe", int64(^uint64(0) >> 1), 15 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := cfg.requestWait(tc.milliseconds); got != tc.want {
				t.Errorf("requestWait(%d) = %s, want %s", tc.milliseconds, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate = %q, want it untouched", got)
	}
	got := truncate(strings.Repeat("x", 20), 5)
	if !strings.HasPrefix(got, "xxxxx") || !strings.Contains(got, "truncated") {
		t.Errorf("truncate = %q, want a marked-up prefix", got)
	}
}

func TestResolveResultString(t *testing.T) {
	t.Parallel()
	// The log records this by name; a bare integer is unreadable at 3am.
	tests := map[resolveResult]string{
		resolveOK:         "ok",
		resolveStoredLate: "stored-late",
		resolveUnknownID:  "unknown-id",
		resolveMissingID:  "missing-id",
	}
	for res, want := range tests {
		if got := res.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(res), got, want)
		}
	}
}
