---
scriptorium: true
action: append
title: "A test that only fails in CI is reading your machine"
type: pattern
domain: general
tags: [testing, hermeticity, go, testmain, false-green]
---

## The third instance, one day later

The same defect returned in the same repo within twenty-four hours, which is the
useful part: the first two fixes were both *local* — each patched the one test
that had been caught. Neither closed the class.

A Go helper built the config every test used but never set `AMP_BRIDGE_DIR`, and
the resolver fell back to `/tmp/amp-bridge-<uid>` whenever the variable was
unset. That fallback existed for the shipped binary, where it is correct. In the
test binary it meant every test consulted the developer's live sockets and
plugin registrations. It had been that way from the start and had never once
failed, because the directory used to hold nothing interesting. The day it
started holding live inboxes, the tests started depending on which agent
sessions happened to be running that afternoon.

## Two moves that close the class rather than the instance

**Set the isolation binary-wide, in `TestMain`.** Not per test, and not in a
helper each test has to remember to call:

```go
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "suite")
	// ...
	os.Setenv("APP_RUNTIME_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
```

`t.Setenv` cannot do this job — it panics in any test that calls `t.Parallel()`,
so the moment one subtest goes parallel the per-test approach is unavailable and
the omission silently returns.

**Then assert the property, so removing the isolation is loud.** Environment
setup is invisible: delete `TestMain` and nothing fails, the suite just quietly
starts reading the machine again. One test recomputes the real fallback path and
fails if the binary resolves it:

```go
func TestSuiteNeverResolvesTheRealRuntimeDir(t *testing.T) {
	real := fmt.Sprintf("/tmp/app-%d", os.Getuid())
	if got := runtimeDir(); got == real {
		t.Fatalf("the test binary resolves the real runtime dir %s", got)
	}
}
```

Verify it by mutation: strip the `Setenv` out of `TestMain` and confirm this one
test goes red for the right reason.

**The general rule.** When a fallback is correct in production and wrong in
tests, the test binary must override it *by default*, and something must fail
when the override is gone. Anything that relies on each new test remembering is
a defect with a delay on it — and the delay ends when the shared resource starts
holding real state.

Related: [[mutation-checked-tests]]
