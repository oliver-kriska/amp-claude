package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
		c.ampTimeout = 5 * time.Second
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
		bin := fakeAmp(t, `sleep 30`)
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
