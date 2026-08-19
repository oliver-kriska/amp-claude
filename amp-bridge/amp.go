package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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

	// Derived from the bridge context, not Background: on shutdown the Amp
	// subprocess must die with us rather than linger as an orphan.
	ctx, cancel := context.WithTimeout(b.ctx, b.cfg.ampTimeout)
	defer cancel()

	// Give amp a log file of our own. Its stderr on failure is famously unhelpful
	// ("Unexpected error inside Amp CLI"); the actual cause is only in the log,
	// and pointing it at a private file keeps us out of the shared one, which
	// other amp processes are writing to concurrently.
	ampLog := ""
	if f, err := os.CreateTemp("", "amp-bridge-run-*.log"); err == nil {
		ampLog = f.Name()
		_ = f.Close()
		defer func() { _ = os.Remove(ampLog) }()
	}

	args := []string{"threads", "continue", threadID, "--execute", text}
	if ampLog != "" {
		args = append([]string{"--log-file", ampLog}, args...)
	}
	// #nosec G204 -- the binary is operator-configured (AMP_BIN) and the
	// arguments are passed as an argv slice, never through a shell.
	cmd := exec.CommandContext(ctx, b.cfg.ampBin, args...)
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
	if errors.Is(ctx.Err(), context.Canceled) {
		return out, errors.New("bridge is shutting down; the Amp turn was cancelled")
	}
	if err != nil {
		// The log knows what stderr will not say.
		if why := ampDiagnosis(ampLog); why != "" {
			b.logf("AMP_FAILED thread=%s %s", threadID, why)
			return out, fmt.Errorf("amp failed: %s", why)
		}
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

// Amp writes structured JSON logs; on failure the useful line is in there while
// stderr says only "Unexpected error inside Amp CLI. Use 'amp threads report'…".
var (
	existingExecutorRE = regexp.MustCompile(`(?s)"existingExecutorInfo".*?"pid":(\d+)`)
	ampErrorMessageRE  = regexp.MustCompile(`"level":"ERROR".*?"message":"([^"]{1,300})"`)
)

// ampDiagnosis turns the log amp wrote for one run into an actionable sentence,
// or "" if it has nothing to add.
func ampDiagnosis(logPath string) string {
	if logPath == "" {
		return ""
	}
	// #nosec G304 -- logPath is a temp file this process just created.
	data, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	// The interesting lines are at the end, and a single run writes ~1500 of them.
	const tail = 256 << 10
	if len(data) > tail {
		data = data[len(data)-tail:]
	}
	log := string(data)

	// The one worth naming explicitly. Amp allows a single executor per thread,
	// and an interactive `amp` session sitting in that thread holds it — which is
	// exactly the thread the bridge most wants to reach. There is no retry that
	// helps and no bug to fix on our side, so say so precisely.
	if strings.Contains(log, "EXECUTOR_ALREADY_CONNECTED") {
		who := "another Amp process"
		if m := existingExecutorRE.FindStringSubmatch(log); m != nil {
			who = "an Amp session (pid " + m[1] + ")"
		}
		return "that thread is already open in " + who + " — Amp allows one " +
			"executor per thread, so ask_amp cannot attach a second one. Ask from " +
			"that session instead (it can reach this bridge with `amp-bridge --ask`), " +
			"or pass thread_id for a thread nobody has open"
	}

	if m := ampErrorMessageRE.FindStringSubmatch(log); m != nil {
		return truncate(m[1], 300)
	}
	return ""
}
