package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAmp writes a stand-in for the Amp CLI and returns its path. The bridge
// invokes `<bin> threads continue <id> --execute <text>`, so the script can
// assert on its own arguments.
func fakeAmp(t *testing.T, script string) string {
	t.Helper()
	dir := shortTempDir(t)
	path := filepath.Join(dir, "amp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
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
