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
		{"no arguments means serve", nil, options{mode: modeServe}},
		{"list", []string{"--list"}, options{mode: modeList}},
		{"help", []string{"--help"}, options{mode: modeHelp}},
		{"ask", []string{"--ask", "hello"}, options{mode: modeAsk, text: "hello"}},
		{
			"everything after --ask is the message",
			[]string{"--ask", "what", "is", "2+2?"},
			options{mode: modeAsk, text: "what is 2+2?"},
		},
		{
			"session and thread",
			[]string{"--session", "b1", "--thread", "T-7", "--ask", "hi"},
			options{mode: modeAsk, session: "b1", thread: "T-7", text: "hi"},
		},
		{
			"flags after --ask belong to the message",
			[]string{"--ask", "use --list to see them"},
			options{mode: modeAsk, text: "use --list to see them"},
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
		{"ask without a message", []string{"--ask"}, "needs a message"},
		{"ask with only spaces", []string{"--ask", "   "}, "needs a message"},
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
		logBodies:       false,
		ampBin:          "amp",
		ampTimeout:      300 * time.Second,
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
	t.Setenv("AMP_BRIDGE_LOG_BODIES", "1")
	t.Setenv("AMP_BIN", "/usr/local/bin/amp")
	t.Setenv("AMP_BRIDGE_AMP_TIMEOUT", "30s")
	t.Setenv("AMP_BRIDGE_DISABLE_OUTBOUND", "1")

	got := loadConfig()
	want := config{
		maxInFlight:     3,
		maxMessageBytes: 2048,
		replyWait:       5 * time.Second,
		logBodies:       true,
		ampBin:          "/usr/local/bin/amp",
		ampTimeout:      30 * time.Second,
		ampDisabled:     true,
	}
	if got != want {
		t.Errorf("loadConfig() = %+v, want %+v", got, want)
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
		resolveOK:        "ok",
		resolveUnknownID: "unknown-id",
		resolveAmbiguous: "ambiguous",
	}
	for res, want := range tests {
		if got := res.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(res), got, want)
		}
	}
}
