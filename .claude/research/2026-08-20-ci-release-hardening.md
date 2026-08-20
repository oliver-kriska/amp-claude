# Making the release pipeline mean something

**Date:** 2026-08-20
**Companion to:** `2026-08-20-amp-bridge-inbox-plugin-plan.md`
**Consulted:** the Amp dev thread (T-01a01877), which set the scope below.

## The finding that reframes everything else

CI had been red for three consecutive commits and nobody noticed, because the
failure was in the gate rather than in the code. `make check` died at the
`vuln` target with `govulncheck missing` on both runners. The install step ran,
succeeded, and appended `$(go env GOPATH)/bin` to `GITHUB_PATH` — but the Go
toolchain is mise-managed, so that path was not where the binary landed.

A gate that cannot find its own tools is worse than no gate at all. It goes red,
stays red, and its redness carries no information; after two or three commits
you stop reading it. The fix is to stop trusting a computed path: set `GOBIN`
explicitly so the same string installs the binary and finds it, and assert
`test -x` in the step that installs it. The check job now also resolves
`govulncheck`, `golangci-lint` and `bun` before running `make check`, so the
next such breakage names the missing tool instead of dying minutes later inside
a Makefile.

**Then the gate started working and immediately found two real defects.** Both
had been present for as long as the code, and both passed locally every time.

## Defect 1 — `ampTimeout` bounded nothing

`ask_amp` promises Claude's turn is bounded at five minutes. It was not bounded
at all.

`cmd.Stdout` is a `bytes.Buffer`, so `os/exec` gives amp a pipe and copies from
it. `Wait` cannot return until that pipe reaches EOF, and EOF needs *every*
holder of the write end to close it. amp is a Node CLI whose children inherit
that pipe. Killing amp alone leaves them holding it, so `Wait` blocks for as
long as the longest-lived grandchild, whatever the deadline said.

Fix: `SysProcAttr{Setpgid: true}` so amp gets a process group of its own, a
`cmd.Cancel` that signals the group rather than the pid, and `WaitDelay` as the
backstop for a child that ignores the signal or has already been reparented.

The instructive part is why it only failed on Linux. Some `/bin/sh` implementations
exec-optimise the last command of a script away, so the shell *becomes* the
`sleep` and killing the direct child is accidentally sufficient. On macOS the
test passed for that reason and could not be made to fail even with the sleep
moved into a grandchild — CI is the only place the mutation is visible. Recorded
honestly rather than claiming a mutation check that did not reproduce.

## Defect 2 — an integration test reading the developer's own machine

`exec.Command(p.bin, "--list")` was the single invocation in
`TestProcessClientListAndAsk` not given `p.env`, so it read the ambient
`AMP_BRIDGE_DIR` instead of the harness's. On a developer machine that directory
contains a real live session, so the assertion passed against a bridge the test
never started and knew nothing about. On CI there is no such session and it
failed.

Reproduced locally by pointing the ambient `AMP_BRIDGE_DIR` at an empty
directory, which gives CI's exact error. That reproduction is the reason the
one-line fix can be believed.

**The general shape:** a test that passes on every developer machine and fails
only in a clean environment is not flaky. It is telling you it has been reading
ambient state, and that it has never actually tested what it claims to.

## Release pipeline — what was added and why

Amp was asked which of four candidate additions to make, and answered "all
four", adding three of its own. The result:

| Addition | Reason |
|---|---|
| Gate publishing on `make check` | The workflow ran `gh release create` off a build with nothing behind it, so a tag on a red commit shipped. Running the gate again at tag time is what makes a tag mean something on its own. |
| Reject malformed tags | `v*` matches `vibes` as happily as `v1.2.0`. |
| Require the tag on main | A tag on a commit that never landed ships unreviewed code the next release silently drops. |
| Verify `checksums.txt` | `install.sh` verifies downloads against it, so a wrong checksum makes every install fail in a way that looks like a compromised download. |
| Build provenance attestation | For a binary people curl onto their PATH, a checksum only proves the file did not change — provenance proves where it was built. Needs `id-token: write` and `attestations: write` scoped to the publishing job. |
| Smoke-test the artefact | Unpack the linux/amd64 tarball and run the binary out of it. Proves the tarball is well-formed and the binary starts. |
| Byte-compare the embedded plugin | `make plugin-check` proves the two checked-in copies agree. Only running the built artefact proves the *binary* carries the same bytes. This is the half of the drift gate a source diff cannot reach. |

Rejected, deliberately: shipping the plugin as a separate release artifact. It is
embedded in the binary and `init --amp-plugin` writes it out, which is the entire
reason for embedding it.

That smoke test also forced `--version` into existence. Releases stamped
`main.serverVersion` via ldflags and nothing could read it back; a mistyped
symbol path fails silently and ships a binary reporting `dev` forever. The
release now asserts the stamp matches the tag.

## Open blocker — the repository name

`go.mod` declares `github.com/oliver-kriska/amp-claude`, and `install.sh` and
README agree, but the repository is `oliver-kriska/amp_claude`. So
`go install github.com/oliver-kriska/amp-claude/amp-bridge@latest` resolves to
nothing — verified, it returns `repository not found`.

Amp's recommendation and mine agree: rename the repository to `amp-claude`. The
module path, the installer, the documentation and Go's own convention already
say so, and the module path is baked into every import. There are **no tags and
no releases yet**, so the rename costs nothing today and gets more expensive
with every published artefact. GitHub keeps a redirect from the old name.

Needs Oliver's approval; it is his repository and it is outward-facing.
