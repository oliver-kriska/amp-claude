---
scriptorium: true
action: create
title: "A test that only fails in CI is reading your machine"
type: pattern
domain: general
tags: [testing, ci, subprocess, go, false-green]
---

Two defects found on the same day, both years-old in effect, both passing on
every local run. Neither was flaky. Both were tests that had never once
executed in an environment that did not already contain the thing they were
asserting about.

## The shape

A test passes everywhere developers run it and fails the first time it runs
clean. The instinct is to call it flaky, or environment-specific, and pin or
skip it. That instinct is wrong almost every time. The correct reading is:

> The test has been passing for a reason unrelated to the code under test, and
> the clean environment has just removed that reason.

Which means the test was never testing what it claimed, and whatever it was
supposed to protect has been unprotected the whole time.

## Case 1 — ambient state stood in for the fixture

An integration harness spawned a real server process into a temp directory and
exported that directory through an env var. Every subprocess in the test was
given `cmd.Env = harness.env` — except one, which was not. That one read the
*developer's* ambient directory instead, found their own live long-running
process there, and asserted successfully against a server the test never
started.

On CI there is no such process, so it failed. The fix was one line; the value
was entirely in noticing that the assertion had been meaningless.

**Generalisation:** any test touching a discovery mechanism — an env var, a
registry directory, a well-known socket path, `~/.config`, a running daemon —
must be given a hermetic version of it explicitly. Auditing for this is cheap:
grep for the subprocess constructor and check that *every* call site sets the
environment, not most of them. The odd one out is the bug.

**Cheap local reproduction:** point the ambient variable at an empty directory
and run the suite. That is what CI is, minus the wait.

## Case 2 — a subprocess timeout that bounded nothing

Go's `exec.CommandContext` with a context deadline looks like it bounds
execution. It does not, if `cmd.Stdout` is anything other than an `*os.File`.

With a `bytes.Buffer`, `os/exec` creates an OS pipe and copies from it, and
`Wait` cannot return until that pipe reaches EOF. EOF requires *every* holder of
the write end to close it — including grandchildren that inherited it. Cancelling
the context kills the direct child only. If that child spawned anything (a Node
CLI will), `Wait` blocks for as long as the longest-lived descendant, no matter
what the deadline said.

```go
cmd := exec.CommandContext(ctx, bin, args...)
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}   // own process group
cmd.Cancel = func() error {                              // signal the group
    return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
cmd.WaitDelay = 2 * time.Second                          // backstop
```

`WaitDelay` (Go 1.20+) alone is the minimum fix: it forces the pipes closed and
lets `Wait` return. The process group is what actually reaps the descendants
rather than orphaning them.

**Why it hid on macOS:** some `/bin/sh` implementations exec-optimise the last
command of a script, so the shell *becomes* the child process and killing the
direct pid is accidentally sufficient. A test using `sleep 30` as a fake slow
subprocess therefore passes or fails depending on the platform's shell, not on
whether the code is correct.

**Generalisation:** if you set a timeout on a subprocess and capture its output
into a buffer, you have not set a timeout. Ask what happens when the child
spawns a child. Test it with a fake that deliberately leaves a grandchild
holding the pipe — and do not trust a `sleep` in a shell script to be that fake.

## The third lesson, which enabled both

Neither defect was findable while the gate itself was broken. CI had been red
for three commits with `govulncheck missing` — the install step wrote a computed
`$(go env GOPATH)/bin` to `GITHUB_PATH`, and under a version-managed toolchain
that is not where the binary lands. `make check` never reached the test tiers.

**A gate that cannot find its own tools is worse than no gate.** It goes red,
stays red, and its redness stops carrying information — which is precisely when
people stop reading it. Set the install destination explicitly rather than
computing it, assert the binary exists in the step that installs it, and resolve
every required tool in one step before the long one, so a missing tool is named
in ten seconds instead of failing five minutes later inside a Makefile.

Related: [[mutation-checked-tests]], [[verify-against-reality-not-configuration]]
