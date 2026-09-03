package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spawnWait bounds the tests that wait on the fake Amp CLI actually running.
// They assert on routing, not on how fast this machine can fork; a budget sized
// for in-process work makes them fail whenever something else is busy.
const spawnWait = 30 * time.Second

// fakeAmp writes a stand-in for the Amp CLI and returns its path. The bridge
// invokes `<bin> [--log-file <path>] threads continue <id> --execute <text>`, so
// the script can assert on its own arguments.
//
// The stub consumes the leading global option the way the real CLI does, which
// keeps the subcommand at $1 for every caller. Without that, adding a global
// flag silently shifts every positional assertion in this file by two.
func fakeAmp(t *testing.T, script string) string {
	t.Helper()
	dir := shortTempDir(t)
	path := filepath.Join(dir, "amp")
	preamble := "#!/bin/sh\nif [ \"$1\" = \"--log-file\" ]; then shift 2; fi\n"
	if err := os.WriteFile(path, []byte(preamble+script+"\n"), 0o755); err != nil {
		t.Fatalf("write fake amp: %v", err)
	}
	return path
}

func ampHarness(t *testing.T, script string) *harness {
	t.Helper()
	bin := fakeAmp(t, script)
	return newHarness(t, func(c *config) {
		c.ampDisabled = false
		c.ampBin = bin
		// Generous on purpose. macOS pays a first-exec security check on each
		// freshly written script, which can take seconds under parallel load;
		// a tight budget here makes every test that shells out flaky. The tests
		// that actually exercise the timeout set their own short one.
		c.ampTimeout = 60 * time.Second
		// The asynchronous budget needs the same headroom, and then some: it is
		// larger than the synchronous one in production for the same reason.
		c.sendTimeout = 120 * time.Second
	})
}

func TestAskAmpSuccess(t *testing.T) {
	t.Parallel()
	h := ampHarness(t, `echo "args: $*"`)

	out, err := h.b.askAmp("T-42", "what did you find?")
	if err != nil {
		t.Fatalf("askAmp: %v", err)
	}
	// The argv shape is the contract with the Amp CLI.
	for _, want := range []string{"threads", "continue", "T-42", "--execute", "what did you find?"} {
		if !strings.Contains(out, want) {
			t.Errorf("amp was invoked as %q, missing %q", out, want)
		}
	}
}

func TestAskAmpUsesTheRememberedThread(t *testing.T) {
	t.Parallel()
	h := ampHarness(t, `echo "$3"`)

	if _, err := h.b.askAmp("", "hi"); err == nil {
		t.Fatal("with no thread known and none passed, askAmp must fail")
	}

	// The bridge learns the id from the Amp client's --thread flag.
	h.b.rememberThread("T-remembered")
	out, err := h.b.askAmp("", "hi")
	if err != nil {
		t.Fatalf("askAmp: %v", err)
	}
	if strings.TrimSpace(out) != "T-remembered" {
		t.Errorf("thread = %q, want T-remembered", out)
	}

	// An explicit id still overrides it.
	out, err = h.b.askAmp("T-explicit", "hi")
	if err != nil {
		t.Fatalf("askAmp: %v", err)
	}
	if strings.TrimSpace(out) != "T-explicit" {
		t.Errorf("thread = %q, want the explicit override", out)
	}
}

func TestAskAmpErrors(t *testing.T) {
	t.Parallel()

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t) // testConfig disables outbound
		if _, err := h.b.askAmp("T-1", "hi"); err == nil {
			t.Fatal("askAmp must fail when outbound is disabled")
		}
	})

	t.Run("empty text", func(t *testing.T) {
		t.Parallel()
		h := ampHarness(t, `echo ok`)
		if _, err := h.b.askAmp("T-1", "   "); err == nil {
			t.Fatal("askAmp must reject an empty message")
		}
	})

	t.Run("non-zero exit carries the stderr", func(t *testing.T) {
		t.Parallel()
		h := ampHarness(t, `echo "thread not found" >&2; exit 3`)
		_, err := h.b.askAmp("T-1", "hi")
		if err == nil {
			t.Fatal("a failing amp invocation must surface as an error")
		}
		if !strings.Contains(err.Error(), "thread not found") {
			t.Errorf("error = %q, want it to include amp's own message", err)
		}
	})

	t.Run("missing binary", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, func(c *config) {
			c.ampDisabled = false
			c.ampBin = "/nonexistent/amp"
		})
		if _, err := h.b.askAmp("T-1", "hi"); err == nil {
			t.Fatal("a missing amp binary must be reported")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		// A bare `sleep 30` makes this test a coin toss on shell semantics: some
		// /bin/sh exec-optimise the last command, so killing amp's pid kills the
		// sleep and the wait is bounded by accident. That is why it passed on
		// macOS and hung for the full 30s on CI.
		//
		// This script removes the shell from the question. amp exits immediately;
		// a grandchild holds the inherited stdout pipe for 30 seconds. Wait
		// cannot return until that pipe reaches EOF no matter who gets killed,
		// so only an explicit bound gets us out — which is the real shape of the
		// problem, amp being a Node CLI that spawns children.
		bin := fakeAmp(t, "sh -c 'sleep 30' &\nexit 0")
		h := newHarness(t, func(c *config) {
			c.ampDisabled = false
			c.ampBin = bin
			c.ampTimeout = 150 * time.Millisecond
		})
		start := time.Now()
		_, err := h.b.askAmp("T-1", "hi")
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("askAmp should time out, got %v", err)
		}
		// The point of the timeout is that Claude's turn is not held hostage.
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("askAmp took %v; the timeout did not bound it", elapsed)
		}
	})
}

func TestAskAmpFallsBackToStderr(t *testing.T) {
	t.Parallel()
	// Some Amp builds report on stderr and leave stdout empty; returning
	// nothing at all would look to Claude like a successful empty answer.
	h := ampHarness(t, `echo "written to stderr" >&2; exit 0`)

	out, err := h.b.askAmp("T-1", "hi")
	if err != nil {
		t.Fatalf("askAmp: %v", err)
	}
	if !strings.Contains(out, "written to stderr") {
		t.Errorf("out = %q, want the stderr text", out)
	}
}

func TestCLIRequestsRemainSerialized(t *testing.T) {
	lock := filepath.Join(shortTempDir(t), "amp-running")
	t.Setenv("AMP_TEST_LOCK", lock)
	h := ampHarness(t, `
if ! mkdir "$AMP_TEST_LOCK" 2>/dev/null; then
  echo "CLI requests overlapped" >&2
  exit 9
fi
sleep 0.2
rmdir "$AMP_TEST_LOCK"
echo ok`)

	errs := make(chan error, 2)
	for _, threadID := range []string{"T-one", "T-two"} {
		go func(threadID string) {
			_, err := h.b.askAmp(threadID, "work")
			errs <- err
		}(threadID)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("CLI fallback lost its process bound: %v", err)
		}
	}
}

func TestSendAmpUsesTheCLIFallbackWithoutRebinding(t *testing.T) {
	h := ampHarness(t, `echo "background CLI answer"`)
	h.b.rememberThread("T-existing-pair")
	h.b.askMu.Lock()
	defer h.b.askMu.Unlock()

	h.call(t, sendAmpCall("async-cli", "T-async-cli-fallback-test", "do independent work"))
	res := result(t, h.response(t, "async-cli"))
	if isToolError(t, res) || !strings.Contains(toolText(t, res), "amp-async-") {
		t.Fatalf("send_amp did not return an async handle: %v", res)
	}
	// The synchronous CLI mutex is deliberately held for this whole assertion.
	// Background work has its own bounded slots and must not queue ahead of a
	// later foreground ask whose caller has a shorter deadline.
	waitUpTo(t, spawnWait, "the CLI async completion event", func() bool { return len(h.notifications(t)) == 1 })

	params, _ := h.notifications(t)[0]["params"].(map[string]any)
	if content, _ := params["content"].(string); !strings.Contains(content, "background CLI answer") {
		t.Errorf("completion omitted CLI output: %q", content)
	}
	if got := h.b.knownThread(); got != "T-existing-pair" {
		t.Errorf("CLI send_amp rebound the default thread to %q", got)
	}
}

func TestSameThreadCLIFallbackOverlapFailsLocally(t *testing.T) {
	dir := shortTempDir(t)
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	t.Setenv("AMP_TEST_READY", ready)
	t.Setenv("AMP_TEST_RELEASE", release)
	h := ampHarness(t, `
touch "$AMP_TEST_READY"
while [ ! -f "$AMP_TEST_RELEASE" ]; do sleep 0.01; done
echo done`)

	background := make(chan error, 1)
	go func() {
		_, err := h.b.askAmpExplicit("T-same", "background")
		background <- err
	}()
	waitUpTo(t, spawnWait, "the first CLI turn to start", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})

	if _, err := h.b.askAmp("T-same", "foreground"); err == nil || !strings.Contains(err.Error(), "already in flight in this bridge") {
		t.Errorf("same-thread overlap should fail locally, got %v", err)
	}
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-background; err != nil {
		t.Fatalf("first CLI turn failed: %v", err)
	}
}

// The two log shapes below are trimmed from real amp runs captured on
// 2026-08-19; the point of the parser is that it reads what amp actually
// writes, so the fixtures must stay verbatim in structure.

const executorBusyLog = `{"@timestamp":"2026-08-19T14:46:07.863Z","level":"INFO","message":"Starting executor bootstrap","logger":"executor","pid":32334}
{"@timestamp":"2026-08-19T14:46:08.004Z","level":"ERROR","message":"[thread-client] Executor handshake rejected by server","logger":"","error":{"code":"EXECUTOR_ALREADY_CONNECTED","payload":{"type":"executor_error","message":"Executor already connected","code":"EXECUTOR_ALREADY_CONNECTED","existingExecutorInfo":{"capabilities":{"workspaceId":"/Users/o/Projects/amp_claude","workingDirectory":"/Users/o/Projects/amp_claude","pid":50778,"environment":{"os":"darwin"}},"executorType":"local-client"}}},"code":"EXECUTOR_ALREADY_CONNECTED","pid":32334}
`

const stdinTimeoutLog = `{"@timestamp":"2026-08-19T14:44:02.101Z","level":"INFO","message":"booting","pid":31000}
{"@timestamp":"2026-08-19T14:45:02.400Z","level":"ERROR","message":"Timeout while reading from stdin","logger":"","pid":31000}
`

func writeLog(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "amp.log")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestAmpDiagnosisNamesTheExecutorHolder(t *testing.T) {
	t.Parallel()
	got := ampDiagnosis(writeLog(t, executorBusyLog))

	// Amp's own stderr for this says only "Unexpected error inside Amp CLI",
	// which reads as a broken bridge. It is neither broken nor retryable: the
	// thread is open somewhere else.
	for _, want := range []string{"already open", "pid 50778", "one executor per thread"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnosis should mention %q, got: %s", want, got)
		}
	}
}

func TestAmpDiagnosisFallsBackToTheFirstError(t *testing.T) {
	t.Parallel()
	if got := ampDiagnosis(writeLog(t, stdinTimeoutLog)); got != "Timeout while reading from stdin" {
		t.Errorf("got %q", got)
	}
}

func TestAmpDiagnosisStaysQuietWhenItKnowsNothing(t *testing.T) {
	t.Parallel()
	// No ERROR line: say nothing rather than invent a cause, so the caller falls
	// back to amp's own stderr.
	if got := ampDiagnosis(writeLog(t, `{"level":"INFO","message":"all fine"}`)); got != "" {
		t.Errorf("want empty, got %q", got)
	}
	if got := ampDiagnosis(filepath.Join(t.TempDir(), "absent.log")); got != "" {
		t.Errorf("missing file should yield no diagnosis, got %q", got)
	}
	if got := ampDiagnosis(""); got != "" {
		t.Errorf("empty path should yield no diagnosis, got %q", got)
	}
}

func TestAmpDiagnosisReadsTheTailOfABigLog(t *testing.T) {
	t.Parallel()
	// A single run writes ~1500 lines; the failure is at the end. Only the tail
	// is read, so the error must still be found behind a lot of noise.
	noise := strings.Repeat(`{"level":"INFO","message":"chatter"}`+"\n", 20000)
	if got := ampDiagnosis(writeLog(t, noise+executorBusyLog)); !strings.Contains(got, "pid 50778") {
		t.Errorf("lost the error behind %d bytes of noise: %q", len(noise), got)
	}
}

func TestAskAmpRemembersAThreadClaudeNamed(t *testing.T) {
	t.Parallel()
	h := ampHarness(t, `echo "$3"`)

	// Pairing has to work from either end. Amp binds it with `--ask --thread`;
	// Claude binds it by naming the thread once, and should not have to repeat
	// the id on every later call.
	if _, err := h.b.askAmp("T-claude-chose", "hello"); err != nil {
		t.Fatalf("askAmp: %v", err)
	}
	if got := h.b.knownThread(); got != "T-claude-chose" {
		t.Errorf("knownThread = %q, want the thread ask_amp just used", got)
	}

	out, err := h.b.askAmp("", "follow-up")
	if err != nil {
		t.Fatalf("follow-up: %v", err)
	}
	if strings.TrimSpace(out) != "T-claude-chose" {
		t.Errorf("follow-up went to %q, want the remembered thread", strings.TrimSpace(out))
	}
}

func TestAskAmpDoesNotRememberARejectedThread(t *testing.T) {
	t.Parallel()
	h := ampHarness(t, `echo "$3"`)

	// A thread id that fails validation must not become the default target —
	// that would turn one bad call into every later call failing.
	if _, err := h.b.askAmp("-not-an-id", "hi"); err == nil {
		t.Fatal("an implausible thread id must be rejected")
	}
	if got := h.b.knownThread(); got != "" {
		t.Errorf("remembered a rejected id: %q", got)
	}
}

// fakeAmpLogAware is fakeAmp for tests that care about the --log-file argument
// rather than the positional ones: it keeps the path in $AMPLOG before shifting,
// so the script can write into the log the bridge will later read (or refuse to
// delete). $MARKER receives that path so the test can check the file's fate.
func fakeAmpLogAware(t *testing.T, script string) string {
	t.Helper()
	dir := shortTempDir(t)
	path := filepath.Join(dir, "amp")
	preamble := "#!/bin/sh\n" +
		"AMPLOG=\"\"\n" +
		"if [ \"$1\" = \"--log-file\" ]; then AMPLOG=\"$2\"; shift 2; fi\n" +
		"if [ -n \"$MARKER\" ]; then printf '%s' \"$AMPLOG\" > \"$MARKER\"; fi\n"
	if err := os.WriteFile(path, []byte(preamble+script+"\n"), 0o755); err != nil {
		t.Fatalf("write fake amp: %v", err)
	}
	return path
}

// An undiagnosed failure must keep amp's log. Deleting the only record of a
// failure nobody can explain is how "Unexpected error inside Amp CLI" stayed
// unclassifiable across four real runs.
func TestAskAmpKeepsAmpLogWhenItCannotDiagnose(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "logpath")
	t.Setenv("MARKER", marker)

	// A log with nothing diagnosable in it, and amp's trademark useless stderr.
	bin := fakeAmpLogAware(t, `printf '{"level":"INFO","message":"nothing useful"}\n' > "$AMPLOG"
echo "Unexpected error inside Amp CLI." >&2
exit 1`)
	h := newHarness(t, func(c *config) {
		c.ampDisabled = false
		c.ampBin = bin
		c.ampTimeout = 60 * time.Second
	})

	_, err := h.b.askAmp("T-42", "hi")
	if err == nil {
		t.Fatal("askAmp must fail when amp exits non-zero")
	}
	if !strings.Contains(err.Error(), "did not say why") {
		t.Errorf("error must admit the failure is undiagnosed, got %q", err)
	}

	kept, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatalf("stub did not record the log path: %v", readErr)
	}
	logPath := string(kept)
	if logPath == "" {
		t.Fatal("bridge did not pass --log-file")
	}
	// The point of the change: the evidence survives.
	if _, statErr := os.Stat(logPath); statErr != nil {
		t.Errorf("amp's log was deleted despite an undiagnosed failure: %v", statErr)
	}
	// And the caller is told where to look.
	if !strings.Contains(err.Error(), logPath) {
		t.Errorf("error must name the kept log %q, got %q", logPath, err)
	}
}

// The mirror image: when the failure IS diagnosed, the log has served its
// purpose and must not accumulate in the temp directory.
func TestAskAmpRemovesAmpLogOnceDiagnosed(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "logpath")
	t.Setenv("MARKER", marker)

	bin := fakeAmpLogAware(t, `printf '{"level":"ERROR","message":"EXECUTOR_ALREADY_CONNECTED","existingExecutorInfo":{"pid":4242}}\n' > "$AMPLOG"
echo "Unexpected error inside Amp CLI." >&2
exit 1`)
	h := newHarness(t, func(c *config) {
		c.ampDisabled = false
		c.ampBin = bin
		c.ampTimeout = 60 * time.Second
	})

	_, err := h.b.askAmp("T-42", "hi")
	if err == nil {
		t.Fatal("askAmp must fail when amp exits non-zero")
	}
	if !strings.Contains(err.Error(), "pid 4242") {
		t.Fatalf("diagnosis must name the holder, got %q", err)
	}

	kept, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatalf("stub did not record the log path: %v", readErr)
	}
	if _, statErr := os.Stat(string(kept)); statErr == nil {
		t.Errorf("a diagnosed failure must not leave %s behind", string(kept))
	}
}

// The bridge log must record the thread that was actually used. Logging the
// requested id left real entries reading thread="" that could never afterwards
// be attributed to a thread.
func TestAskAmpLogsTheResolvedThread(t *testing.T) {
	t.Parallel()
	h := ampHarness(t, `echo ok`)

	h.b.rememberThread("T-remembered")
	if _, err := h.b.askAmp("", "hi"); err != nil {
		t.Fatalf("askAmp: %v", err)
	}

	logged := h.log.String()
	if !strings.Contains(logged, "ASK_AMP thread=T-remembered") {
		t.Errorf("log must name the resolved thread, got %q", logged)
	}
	if strings.Contains(logged, `thread=""`) {
		t.Error(`log still records the unresolved thread="" form`)
	}
}
