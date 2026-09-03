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
	"syscall"
	"time"
)

// Claude -> Amp direction.
//
// The channel gives Claude a way to answer Amp. It gives it no way to *start* a
// conversation, so on its own the bridge is half-duplex. The outbound tools
// close that through a live plugin inbox when available, or the Amp CLI when no
// interactive session owns the target thread.
//
// Thread identity: Amp's client may pass --thread <id>, which we remember. That
// lets Claude say "ask Amp about X" without knowing the id, while still allowing
// an explicit override.

// ampWaitDelay bounds how long Wait may keep reading amp's pipes after the
// context has been cancelled. Two seconds is generous for a child that is about
// to die anyway and short enough that Claude's turn is not held on it.
const ampWaitDelay = 2 * time.Second

var (
	errOutboundDisabled = errors.New(
		"outbound to Amp is disabled (AMP_BRIDGE_DISABLE_OUTBOUND=1)",
	)
	errNoThread = errors.New(
		"no Amp thread id known. Either the Amp side has not sent a message yet " +
			"(the bridge learns the id from `--thread`), or you must pass thread_id explicitly",
	)
	errEmptyText = errors.New("text is empty")

	// Amp thread ids look like T-01a01877-2274-734d-8306-7c37b33f2a7f. Anything
	// starting with a dash would be read by the CLI as a flag rather than an id.
	threadIDRE          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	canonicalThreadIDRE = regexp.MustCompile(`^T-[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
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
	threadID, err := b.resolveThread(threadID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", errEmptyText
	}

	// Remember an explicitly addressed thread, so pairing works from either
	// side: Amp binds the pair with `--ask --thread`, and Claude binds it by
	// naming the thread once. Without this, only the Amp side could establish
	// the pair and Claude had to repeat the id on every call.
	b.rememberThread(threadID)
	// The synchronous budget: this call is holding a Claude turn open, and that
	// turn may itself be answering an Amp request with its own deadline.
	return b.deliverToAmp(threadID, text, true, b.cfg.ampTimeout)
}

// askAmpExplicit runs one turn without changing the bridge's remembered pair.
// send_amp requires this form: concurrent background work must not make a later
// thread_id-less ask_amp silently target whichever job happened to finish last.
func (b *bridge) askAmpExplicit(threadID, text string) (string, error) {
	if b.cfg.ampDisabled {
		return "", errOutboundDisabled
	}
	if strings.TrimSpace(threadID) == "" {
		return "", errors.New("thread_id is required for send_amp")
	}
	if !threadIDRE.MatchString(threadID) {
		return "", fmt.Errorf("implausible Amp thread id %q", threadID)
	}
	if strings.TrimSpace(text) == "" {
		return "", errEmptyText
	}
	// The asynchronous budget. Nothing is blocked on this call, so bounding it
	// by the synchronous deadline only threw away turns Amp was still running.
	return b.deliverToAmp(threadID, text, false, b.cfg.sendTimeout)
}

// deliverToAmp chooses the live plugin inbox when one serves the thread and
// otherwise falls back to the Amp CLI. Callers validate the target first.
// Synchronous calls serialize the CLI fallback; background calls are already
// bounded by asyncSlots and must not sit ahead of an ask_amp whose caller has a
// shorter end-to-end deadline.
func (b *bridge) deliverToAmp(
	threadID, text string, serializeCLI bool, budget time.Duration,
) (string, error) {
	// Logged here, not at the call site: by this point an empty request has been
	// resolved through knownThread(), and a log line reading thread="" cannot be
	// classified after the fact.
	b.logf("ASK_AMP thread=%s bytes=%d budget=%s", threadID, len(text), budget)

	// A thread open in an interactive Amp session cannot be reached by `threads
	// continue`: Amp allows one executor per thread and that session holds it.
	// If the session has a plugin inbox enabled, deliver through it instead.
	// Absence falls through to the CLI path below, byte for byte unchanged.
	entry, viaInbox, err := b.lookupInbox(threadID)
	if err != nil {
		// A refusal to trust the runtime directory is not "nothing is running",
		// and must not quietly become a fallback.
		return "", err
	}

	if viaInbox {
		// Final — deliberately no CLI fallback on error. The CLI would hit
		// EXECUTOR_ALREADY_CONNECTED anyway, since a thread with a live inbox is
		// by construction open, and once the ask frame is written the message may
		// already be appended. A loud error beats a possible double delivery.
		return b.askViaInbox(entry, threadID, text, budget)
	}

	if serializeCLI {
		// Preserve synchronous call ordering. Background CLI turns bypass this
		// mutex: they have their own bounded slots, and queueing them here could
		// hold a foreground ask beyond the caller's reply deadline.
		b.askMu.Lock()
		defer b.askMu.Unlock()
	}
	if !b.beginCLIThread(threadID) {
		return "", deliveryFault(deliveryNotSent,
			"another CLI fallback turn for thread %s is already in flight in this bridge; "+
				"wait for it to finish or enable the thread inbox to queue turns", threadID,
		)
	}
	defer b.endCLIThread(threadID)

	// Derived from the bridge context, not Background: on shutdown the Amp
	// subprocess must die with us rather than linger as an orphan.
	ctx, cancel := context.WithTimeout(b.ctx, budget)
	defer cancel()

	// Give amp a log file of our own. Its stderr on failure is famously unhelpful
	// ("Unexpected error inside Amp CLI"); the actual cause is only in the log,
	// and pointing it at a private file keeps us out of the shared one, which
	// other amp processes are writing to concurrently.
	//
	// Keep it when the failure turns out to be one we cannot explain: deleting
	// the only evidence of an undiagnosed failure is how a bug becomes
	// unreproducible. Every other path removes it.
	ampLog, keepLog := "", false
	if f, err := os.CreateTemp("", "amp-bridge-run-*.log"); err == nil {
		ampLog = f.Name()
		_ = f.Close()
		defer func() {
			if keepLog {
				return
			}
			_ = os.Remove(ampLog)
		}()
	}

	args := []string{"threads", "continue", threadID, "--execute", text}
	if ampLog != "" {
		args = append([]string{"--log-file", ampLog}, args...)
	}
	out, errOut, err := runAmp(ctx, b.cfg.ampBin, args)

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// The CLI path owns the thread outright, so a deadline here leaves a turn
		// running under our own subprocess: delivered, outcome unseen.
		return out, deliveryFault(deliveryUnknown,
			"amp timed out after %s — the turn may have run in thread %s; check it before "+
				"resending", budget, threadID)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return out, errors.New("bridge is shutting down; the Amp turn was cancelled")
	}
	if err != nil {
		keep, failure := b.ampFailure(err, threadID, ampLog, errOut)
		keepLog = keep
		return out, failure
	}
	// Some Amp builds report on stderr while leaving stdout empty; prefer any
	// output at all over returning nothing.
	if out == "" && errOut != "" {
		return errOut, nil
	}
	return out, nil
}

func (b *bridge) beginCLIThread(threadID string) bool {
	b.cliMu.Lock()
	defer b.cliMu.Unlock()
	if _, exists := b.cliActive[threadID]; exists {
		return false
	}
	b.cliActive[threadID] = struct{}{}
	return true
}

func (b *bridge) endCLIThread(threadID string) {
	b.cliMu.Lock()
	delete(b.cliActive, threadID)
	b.cliMu.Unlock()
}

// resolveThread turns the caller's (possibly empty) thread id into the one that
// will actually be used, or explains why there isn't one.
//
// Empty means "the thread this bridge is paired with", which is how pairing
// works from the Claude side: name the thread once, then omit it.
func (b *bridge) resolveThread(threadID string) (string, error) {
	if threadID == "" {
		threadID = b.knownThread()
	}
	if threadID == "" {
		return "", errNoThread
	}
	if !threadIDRE.MatchString(threadID) {
		return "", fmt.Errorf("implausible Amp thread id %q", threadID)
	}
	return threadID, nil
}

// runAmp executes the Amp CLI under ctx and returns its trimmed output.
//
// The process-group handling is what makes a deadline on ctx mean anything.
// Stdout is a bytes.Buffer, so os/exec hands amp a pipe and copies from it, and
// Wait cannot return until that pipe reaches EOF — which needs every holder of
// the write end to close it. amp is a Node CLI that spawns children, and they
// inherit it. Killing amp alone leaves them holding the pipe, so Wait blocks for
// as long as the longest-lived grandchild however short the timeout was. CI
// caught this with a fake amp whose child slept 30s: a 150ms timeout took the
// full 30 seconds, and only on Linux, because some /bin/sh exec-optimise the
// last command away and accidentally make the kill sufficient.
//
// Setpgid puts amp in a group of its own and Cancel signals the group, so its
// children die with it. WaitDelay is the backstop for a child that ignores the
// signal or has already been reparented: it forces the pipes closed so Wait
// returns regardless.
func runAmp(ctx context.Context, bin string, args []string) (out, errOut string, err error) {
	// #nosec G204 -- the binary is operator-configured (AMP_BIN) and the
	// arguments are passed as an argv slice, never through a shell.
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Never inherit our stdin: that is the MCP transport.
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = ampWaitDelay

	err = cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

// ampFailure explains a non-zero amp exit as precisely as the evidence allows,
// and reports whether amp's log must be kept.
//
// The keep decision is the point. Amp's stderr on failure is famously
// "Unexpected error inside Amp CLI", which classifies nothing; the cause is only
// ever in its log. Deleting that log after failing to read anything useful out
// of it leaves a failure that can be counted but never explained — which is
// exactly what happened to four real runs before this existed.
func (b *bridge) ampFailure(runErr error, threadID, ampLog, errOut string) (bool, error) {
	if why := ampDiagnosis(ampLog); why != "" {
		b.logf("AMP_FAILED thread=%s %s", threadID, why)
		return false, fmt.Errorf("amp failed: %s", why)
	}

	b.logf("AMP_UNDIAGNOSED thread=%s log=%s stderr=%q", threadID, ampLog, truncate(errOut, 300))
	detail := errOut
	if detail == "" {
		detail = runErr.Error()
	}
	if ampLog != "" {
		return true, fmt.Errorf("amp failed and its log did not say why: %s "+
			"(amp's own log kept at %s — that file is the only record of the cause)",
			truncate(detail, 2000), ampLog)
	}
	if errOut != "" {
		return false, fmt.Errorf("amp failed: %w: %s", runErr, truncate(errOut, 2000))
	}
	return false, fmt.Errorf("amp failed: %w", runErr)
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
			"executor per thread, so ask_amp cannot attach a second one. Install the " +
			"inbox plugin (`amp-bridge init --amp-plugin`) and press Ctrl+O in that " +
			"session → 'amp-bridge: Enable Claude inbox for this thread', and ask_amp " +
			"reaches it directly. Otherwise ask from that session (it can reach this " +
			"bridge with `amp-bridge --ask`), or pass thread_id for a thread nobody has open"
	}

	if m := ampErrorMessageRE.FindStringSubmatch(log); m != nil {
		return truncate(m[1], 300)
	}
	return ""
}
