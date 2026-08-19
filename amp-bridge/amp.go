package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Claude -> Amp direction.
//
// The channel gives Claude a way to answer Amp. It gives it no way to *start* a
// conversation, so on its own the bridge is half-duplex. This closes that by
// shelling out to the Amp CLI, which is the only supported way into an existing
// thread from outside.
//
// Thread identity: Amp's client may pass --thread <id>, which we remember. That
// lets Claude say "ask Amp about X" without knowing the id, while still allowing
// an explicit override.

var (
	errOutboundDisabled = errors.New(
		"outbound to Amp is disabled (AMP_BRIDGE_DISABLE_OUTBOUND=1)")
	errNoThread = errors.New(
		"no Amp thread id known. Either the Amp side has not sent a message yet " +
			"(the bridge learns the id from `--thread`), or you must pass thread_id explicitly")
	errEmptyText = errors.New("text is empty")

	// Amp thread ids look like T-01a01877-2274-734d-8306-7c37b33f2a7f. Anything
	// starting with a dash would be read by the CLI as a flag rather than an id.
	threadIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func (b *bridge) rememberThread(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	b.threadMu.Lock()
	b.lastThread = id
	b.threadMu.Unlock()
}

func (b *bridge) knownThread() string {
	b.threadMu.Lock()
	defer b.threadMu.Unlock()
	return b.lastThread
}

// askAmp runs one turn in an Amp thread and returns its output.
func (b *bridge) askAmp(threadID, text string) (string, error) {
	if b.cfg.ampDisabled {
		return "", errOutboundDisabled
	}
	if threadID == "" {
		threadID = b.knownThread()
	}
	if threadID == "" {
		return "", errNoThread
	}
	if !threadIDRE.MatchString(threadID) {
		return "", fmt.Errorf("implausible Amp thread id %q", threadID)
	}
	if strings.TrimSpace(text) == "" {
		return "", errEmptyText
	}

	// One Amp turn at a time: concurrent `threads continue` runs against the
	// same thread would interleave writes into one conversation.
	b.askMu.Lock()
	defer b.askMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), b.cfg.ampTimeout)
	defer cancel()

	// #nosec G204 -- the binary is operator-configured (AMP_BIN) and the
	// arguments are passed as an argv slice, never through a shell.
	cmd := exec.CommandContext(ctx, b.cfg.ampBin, "threads", "continue", threadID, "--execute", text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Never inherit our stdin: that is the MCP transport.
	cmd.Stdin = nil

	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	errOut := strings.TrimSpace(stderr.String())

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("amp timed out after %s", b.cfg.ampTimeout)
	}
	if err != nil {
		if errOut != "" {
			return out, fmt.Errorf("amp failed: %w: %s", err, truncate(errOut, 2000))
		}
		return out, fmt.Errorf("amp failed: %w", err)
	}
	// Some Amp builds report on stderr while leaving stdout empty; prefer any
	// output at all over returning nothing.
	if out == "" && errOut != "" {
		return errOut, nil
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "… (truncated)"
}
