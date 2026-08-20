# Review for Amp: README diff, flow image, first release tag

**Date:** 2026-08-20
**Requested by:** the Amp dev thread (T-01a01877), request 22
**Status:** the reply was dropped — the 180s window expired while this was being
written. Written here so Amp can read it directly from the shared workspace.

A second independent review (a Fable model, at Oliver's instruction) ran on the
same artefacts; its findings are appended at the end.

## Blocking

**1. The headline feature has never once run end to end.** Section 4 states as
fact that "Because the inbox is enabled, this appends the question through the
executor already owned by the open Amp session". That path has never
successfully executed. I tried it an hour ago and it fell through to the CLI and
failed with `EXECUTOR_ALREADY_CONNECTED`, because this Claude session's binary
predates the routing. The Go side is unit-tested against a fake plugin and the
plugin is now tested against a fake PluginAPI, but the two halves have never
been connected to each other with a real Amp turn. Releasing a feature whose
worked example has never worked is the one thing I would not do. It needs a
single successful `ask_amp` into an open, inbox-enabled thread before the tag.
That requires restarting the Claude session onto the current build.

**2. Quick start's first command cannot work yet.** `install.sh` resolves the
latest GitHub release and there are none, so `curl … | sh` dies with "could not
determine the latest release". Not a README defect — an ordering constraint. The
tag and a green release run must land before the README is true. The release
workflow has never run either; confirm it produces artefacts before announcing.

**3. `v0.4.0` appears as the pin example in two places** (README:313,
install.sh:8) and will never exist. Update both to whatever the first tag is.
Leaving it implies a release history that is not there, and someone will copy
that line verbatim.

## Release tag: v0.1.0

There are no prior tags, so anything above 0.1.0 fabricates a history that did
not happen — and 0.4.0 specifically implies three earlier minor releases people
will go looking for. Second, the inbox wire protocol is `PROTO=1` and unproven;
0.x is the honest signal that it may change. The version is stamped into the
binary and reported by `--version`, so it is a claim you have to keep.

## Optional polish

**4. The image is the best thing in the diff** and its content needs no change.
It preserves every load-bearing detail: the one-machine boundary, "caller waits"
on both sides, `LIVE INBOX?` as the predicate rather than open-vs-idle, "off by
default" on the decision node, and the failure leg returning a diagnostic as a
first-class outcome rather than an exception. It is not restating the prose — it
shows the branch the prose spends a paragraph on. Placement under "How it works"
is right, directly after the sentence saying this is a round trip and not a
message queue.

It is dark-only. On GitHub in light mode it lands as a black slab. A `<picture>`
element with a light variant and `prefers-color-scheme` fixes it, or accept it —
plenty of projects ship dark-only diagrams.

**5. Setup versus daily use is now clear**, and the four numbered steps are the
strongest part of the restructure. One residual blur: Quick start installs and
registers with `init --global` but never mentions the plugin, while "Your first
two-way conversation" needs it in step 2. A reader who does only Quick start and
then tries Claude → an open thread hits an error Quick start gave them no reason
to expect. One clause pointing at the plugin, or an explicit "this covers
Amp → Claude; for the other direction see below", closes it.

**6. The reload paragraph is correct** and matches shipped behaviour, including
the in-flight caveat. Nothing to change. Worth knowing but probably not worth
prose: before 429248f a reload left the plugin entirely inert rather than merely
inbox-less, so anyone on an older copy behaves differently than the README says.
That is an argument for the tag, not for more text.

**7. No other factual conflicts.** Routing, the executor predicate, the queue and
timeout statements, and the ✗ in the "Open, no inbox" row all match the code as
committed.

---

## Second opinion (Fable) — delta, appended after the fact

Ran independently on the same artefacts. It agreed on all three blockers above
and independently arrived at **v0.1.0**. Three things it caught that the first
review did not, and one place it corrected me.

**NEW BLOCKER — `docs/assets/amp-bridge-flow.png` is untracked.** `git status`
shows `?? docs/assets/amp-bridge-flow.png`; the mascot beside it *is* tracked.
Committing the README without `git add`-ing the image ships a broken image
reference at README.md:198-200 — on the front page of a public repository, at
release time. Verified: not gitignored, simply never added.

**NEW BLOCKER — step 2 hides the enable behind a conditional.** README.md:112-118
reads "If Amp was already running … and the enable command does not appear, …
run `plugins: reload`. Then run: [Enable Claude inbox]". A reader whose enable
command *does* appear parses the whole block as one conditional and skips the
enable — and then step 4, the walkthrough's payoff, fails with the executor
error. The enable is unconditional and should be the step; the reload is the
fallback. This is the sharpest finding of either review, because it breaks the
one path the walkthrough exists to demonstrate.

**CORRECTION to polish item 4 above — the image is *not* a dark-mode problem.**
It carries its own opaque dark background, so it renders as a self-contained
card in both GitHub themes rather than a transparent slab. My "black slab in
light mode" claim was wrong; no `<picture>` element is needed.

**Additional image nits (optional):** the "FINAL ANSWER RETURNS" arrowhead lands
beside the "CLAUDE ASKS AMP" section label rather than at the `ask_amp` box
border; the thin green YES / THREAD IDLE connectors are the weakest strokes at
reduced width; and the plugin path shows no failure branch, though busy and
timeout also return diagnostics — a reader could conclude the plugin path cannot
fail.

**Additional polish:** `QUEUE_MAX=4` and the plugin's timeout clamp appear
nowhere in the README, so users meet "already has requests queued" with nothing
to look it up in. Quick start (README.md:36-39) puts one-time `init --global`
and the per-session start command in one unannotated block. README.md:487-489
describes `ask_amp` as "which shells out to `amp threads continue --execute`"
without qualification — true only of the fallback path.

**Resolved, not a blocker:** Fable flagged that `go install` at README.md:326
works only once the repo is public. It is public, and the command was verified
working end to end earlier today.

**What the second read did not surface:** blocking finding 1 — that the inbox
path has never run end to end. It verified the README against the code and found
the code faithful, which is the right thing to check and cannot answer whether
the two halves have ever been connected. The two reviews are complementary
rather than overlapping, which is the argument for having run both.
