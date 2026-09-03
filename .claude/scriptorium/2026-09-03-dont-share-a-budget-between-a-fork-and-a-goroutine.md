---
scriptorium: true
action: create
title: "Don't share a timeout between a fork and a goroutine"
type: pattern
domain: general
tags: [testing, timeouts, flaky-tests, macos, debugging]
---

A test helper like `waitFor(cond)` with one hardcoded budget (5s is the usual
choice) gets reused for two things that differ by an order of magnitude:

- waiting on **in-process** work — a goroutine appending to a slice. Microseconds.
- waiting on a **fork+exec** — a subprocess actually starting. Milliseconds on an
  idle machine, and *seconds to tens of seconds* on a busy one.

Share one constant between them and the suite becomes a load test of whatever
else the developer's laptop is doing. The tests fail for a fact they do not
test.

### The measurement that matters

Before theorising about why a spawn is slow, time a *trivial* spawn:

```
./fresh-script.sh     0.954 total     # first exec
./fresh-script.sh     7.494 total     # second exec of the same file
/bin/sh -c 'exit 0'  16.755 total, 0% cpu
```

This kills the tempting macOS story ("first exec of a freshly written file pays
a Gatekeeper/XProtect check, so warm it up in setup"). That check is real and
per-file — but if the **second** exec is slower than the first, and the system's
own `/bin/sh` is just as slow, it is not a per-file check. Check `uptime`.
Here: load average 245-378 and 5200 processes, from an unrelated project fanning
out agent worktrees, plus an endpoint-security extension scanning every exec.

The wrong fix (a warm-up exec in the fixture) also had a second-order hazard:
stateful stubs *touch files the test waits on*, so warming them up makes the
warm-up observable and the test races itself.

### The rule

Give an explicit budget at the call site whenever the condition crosses a
process boundary: `waitUpTo(t, 30*time.Second, ...)` next to a plain
`waitFor(t, ...)`. Waiting longer costs nothing on a passing run, which is
every run.

And keep it in the test. Do not loosen a *production* timeout to make a test
pass on a busy laptop — that trades a red suite for a real regression in what
the tool tolerates in the field.
