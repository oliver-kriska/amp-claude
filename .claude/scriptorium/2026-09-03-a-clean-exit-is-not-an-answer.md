---
scriptorium: true
action: create
title: "A clean exit is not an answer"
type: pattern
domain: general
tags: [agent-protocols, delegation, error-design, api-semantics, version-skew]
---

When one agent delegates to another — a subprocess, a plugin, a bridged session
— the delegator needs evidence that *work happened*, and a success status is not
it. The two things a tool usually checks are both wrong on their own:

- **Exit code 0** says the process terminated normally. It says nothing about
  whether the agent produced a result.
- **A non-error response** says the far side answered the protocol. It says
  nothing about whether the answer has content.

Return either as success and the caller reads "considered it, had nothing to
add" — a claim you are in no position to make. The likelier causes are a turn
that emitted only tool calls, a request the agent could not act on, or a
configuration rule that made it decline.

## The two known instances

**codex** (fable-advisor's `codex-implementer`, observed live 2026-08-04):
`codex exec` loads `~/.codex/AGENTS.md` on every invocation, so a machine-wide
rule written for one project governs every delegation on the machine. When such
a rule makes codex decline, it returns **exit 0, an empty diff, and a polite
refusal in the final message**. Their rule: *"An empty diff is never `complete`.
A clean exit code is not evidence that work happened."*

**amp-bridge** (found by analogy, 2026-09-03): both delivery paths returned an
empty agent answer as a successful zero-byte reply.

## Where the check has to go — after normalization, not before

The amp-bridge case is the instructive one, because a guard already existed and
was still wrong. The plugin checked `texts.length === 0` on the extracted
content blocks and only *then* normalized:

```ts
const reply = texts.join('\n').split(req.marker).join('').trim()
```

A block that is empty, whitespace, or nothing but the protocol's own bookkeeping
marker passes `texts.length > 0` and becomes blank only here. **Judge blankness
on what you would actually send, not on what you extracted.** Every
transformation between the two — joining, stripping, trimming, decoding — is a
chance for content to disappear after the guard has already passed.

## Name it for what you can observe

`refused` overclaims: empty output does not prove refusal, it could equally be a
model or output-contract defect. `empty-answer` states exactly what was seen and
leaves the cause open. It should mean: delivered, the turn reached a terminal
state, no usable answer, do **not** auto-resend — inspect first. A legitimate
terse "ack" stays a success because it is non-empty.

## The newer end enforces the invariant

When the fix belongs on the far side, put a guard on the near side too — not as
belt-and-braces, but because **the two ends upgrade at different times**. In
amp-bridge, picking up the plugin-side check requires a manual reload inside each
running session, so at the moment of shipping, *every* loaded plugin was old.
A near-side guard is the difference between a fix that works today and one that
works after everybody restarts.

Related: [[An append is not a send]], [[A timeout is not an outcome]] — the same
family, all three about a status that describes the transport rather than the
work.
