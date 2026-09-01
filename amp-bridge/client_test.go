package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPickBridge(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", dir)

	// Nothing registered yet.
	if _, err := pickBridge(""); err == nil {
		t.Error("pickBridge with nothing live must fail")
	} else if !strings.Contains(err.Error(), "no live amp-bridge sessions") {
		t.Errorf("error = %q, want the standard explanation", err)
	}

	alpha := listenAs(t, dir, "alpha")
	if got, err := pickBridge(""); err != nil || got.Name != "alpha" {
		t.Errorf("with one bridge live, --session should be optional: %+v %v", got, err)
	}
	if got, err := pickBridge("alpha"); err != nil || got.Socket != alpha {
		t.Errorf("pickBridge(alpha) = %+v %v", got, err)
	}

	listenAs(t, dir, "beta")

	// Two live bridges: guessing would send the message to the wrong session.
	_, err := pickBridge("")
	if err == nil {
		t.Fatal("with two bridges live, pickBridge must refuse to guess")
	}
	for _, want := range []string{"--session", "alpha", "beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	if got, err := pickBridge("beta"); err != nil || got.Name != "beta" {
		t.Errorf("an explicit name must still resolve: %+v %v", got, err)
	}

	_, err = pickBridge("gamma")
	if err == nil {
		t.Fatal("an unknown session name must fail")
	}
	if !strings.Contains(err.Error(), "gamma") || !strings.Contains(err.Error(), "alpha") {
		t.Errorf("error %q should name both the miss and what is available", err)
	}
}

// listenAs registers a live bridge under the given name and returns its socket.
func listenAs(t *testing.T, dir, name string) string {
	t.Helper()
	sock := filepath.Join(dir, name+".sock")
	ln, err := bindSocket(sock)
	if err != nil {
		t.Fatalf("bindSocket(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	entry := registryEntry{Name: name, Socket: sock, ClaudePID: 1234, CWD: "/tmp"}
	if _, err := entry.publish(); err != nil {
		t.Fatalf("publish(%s): %v", name, err)
	}
	return sock
}

func TestBridgeNames(t *testing.T) {
	t.Parallel()
	got := bridgeNames([]registryEntry{{Name: "a"}, {Name: "b"}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("bridgeNames = %v, want [a b]", got)
	}
	if got := bridgeNames(nil); len(got) != 0 {
		t.Errorf("bridgeNames(nil) = %v, want empty", got)
	}
}

func TestCmdListExitCodes(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", dir)

	if got := cmdList(false); got != 1 {
		t.Errorf("cmdList with nothing live = %d, want 1 so scripts can branch on it", got)
	}
	listenAs(t, dir, "alpha")
	if got := cmdList(false); got != 0 {
		t.Errorf("cmdList with a live bridge = %d, want 0", got)
	}
}

func TestValueOrDash(t *testing.T) {
	t.Parallel()
	if got := valueOrDash(""); got != "-" {
		t.Errorf("valueOrDash(empty) = %q, want -", got)
	}
	if got := valueOrDash("v0.2.0"); got != "v0.2.0" {
		t.Errorf("valueOrDash(value) = %q", got)
	}
}

func TestAdvertisedWait(t *testing.T) {
	t.Parallel()
	target := registryEntry{ReplyTimeout: "3m", MaxReplyTimeout: "15m"}
	for _, tc := range []struct {
		name      string
		requested time.Duration
		want      time.Duration
	}{
		{"advertised default", 0, 3 * time.Minute},
		{"requested", 10 * time.Minute, 10 * time.Minute},
		{"advertised cap", 20 * time.Minute, 15 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := advertisedWait(target, time.Minute, tc.requested); got != tc.want {
				t.Errorf("advertisedWait = %s, want %s", got, tc.want)
			}
		})
	}
	if got := advertisedWait(registryEntry{}, 30*time.Second, 0); got != 3*time.Minute {
		t.Errorf("legacy advertisedWait = %s, want the fixed legacy 3m deadline", got)
	}
}

func TestSupportsRequestedTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		target registryEntry
		want   bool
	}{
		{"current server", registryEntry{ReplyTimeout: "3m", MaxReplyTimeout: "15m"}, true},
		{"legacy server", registryEntry{}, false},
		{"missing maximum", registryEntry{ReplyTimeout: "3m"}, false},
		{"invalid duration", registryEntry{ReplyTimeout: "later", MaxReplyTimeout: "15m"}, false},
		{"maximum below default", registryEntry{ReplyTimeout: "15m", MaxReplyTimeout: "3m"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := supportsRequestedTimeout(tc.target); got != tc.want {
				t.Errorf("supportsRequestedTimeout(%+v) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

func TestRunDispatch(t *testing.T) {
	t.Setenv("AMP_BRIDGE_DIR", shortTempDir(t))

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"help", []string{"--help"}, 0},
		{"unknown flag", []string{"--bogus"}, 2},
		{"list with nothing live", []string{"--list"}, 1},
		{"ask with nothing live", []string{"--ask", "hello"}, 2},
		{
			"ask with truncated source thread",
			[]string{"--thread", "T-01a0335c-7794-769d-b5b4-f8a8b8bb234", "--ask", "hello"},
			2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != tc.want {
				t.Errorf("run(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestDefaultLogPath(t *testing.T) {
	t.Setenv("AMP_BRIDGE_LOG", "/tmp/explicit.log")
	if got := defaultLogPath(); got != "/tmp/explicit.log" {
		t.Errorf("defaultLogPath = %q, want the override", got)
	}

	// With no override the log belongs in a state directory, never beside the
	// binary: installed to ~/.local/bin, that would litter a bin directory.
	t.Setenv("AMP_BRIDGE_LOG", "")
	t.Setenv("XDG_STATE_HOME", "")
	got := defaultLogPath()
	if filepath.Base(got) != "amp-bridge.log" {
		t.Errorf("defaultLogPath = %q, want it to end in amp-bridge.log", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("defaultLogPath = %q, want an absolute path", got)
	}
	exe, err := os.Executable()
	if err == nil && filepath.Dir(got) == filepath.Dir(exe) {
		t.Errorf("defaultLogPath = %q, must not sit beside the binary", got)
	}

	t.Setenv("XDG_STATE_HOME", "/tmp/state")
	if got := defaultLogPath(); got != "/tmp/state/amp-bridge/amp-bridge.log" {
		t.Errorf("defaultLogPath = %q, want it under XDG_STATE_HOME", got)
	}
}

func TestOpenLogCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "nested", "deeper", "bridge.log")

	lf, err := openLog(path)
	if err != nil {
		t.Fatalf("openLog must create the state directory: %v", err)
	}
	defer func() { _ = lf.Close() }()

	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("state dir mode = %o, want 700", perm)
	}
}

func TestOpenLogTightensPermissions(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "bridge.log")

	// A log left world-readable by an earlier run must not stay that way: with
	// AMP_BRIDGE_LOG_BODIES=1 it holds conversation text.
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	lf, err := openLog(path)
	if err != nil {
		t.Fatalf("openLog: %v", err)
	}
	defer func() { _ = lf.Close() }()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}

	// And it appends rather than truncating.
	if _, err := lf.WriteString("new\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "old") {
		t.Error("openLog must append; the previous run's log was truncated")
	}
}
