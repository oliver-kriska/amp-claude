---
scriptorium: true
action: create
title: "AgentChute"
type: tool
domain: general
tags: [agentchute, herdr, multi-agent, orchestration, claude-code, codex, inbox-protocol]
verdict: evaluate
---

An intra-agent communication protocol for coding agents — per-agent Markdown
inboxes, pull-only delivery. Researched 2026-08-24 after Oliver heard it
described secondhand as "for Claude and Codex I use a Herdr plugin, start
Claude or Codex as main orchestrator, they send hashes and agent IDs and read
each other's outputs." That description turns out to conflate three separate
things, none of which it matches cleanly.

See [[herdr]] for the multiplexer itself (already graded `evaluate`, 2026-05-07).

## Three candidates, routinely conflated

| | what it is |
|---|---|
| **herdr core** | The Rust terminal multiplexer. Separate concern — see [[herdr]]. |
| **herdr skill** | A Markdown file teaching an agent to drive the `herdr` CLI for the session it is already inside. Gated on `HERDR_ENV=1`. Not a protocol. |
| **AgentChute** (v1.6.1) | The communication protocol. **Not a herdr extension** — herdr was a historical *wake adapter*, removed in the v0.8 pull-only redesign. |
| **`yigitkonur/claude-code-herdr-plugin`** (v1.3.0) | Separate third-party plugin. Claude→Codex controller, **one-way not symmetric**, using plugin session IDs plus transient content/screen signatures. Spawn API broken against Herdr 0.7.5+ (open issue #2). |

**The description Oliver heard matches none of them cleanly.** If it is the
`yigitkonur` plugin, the symmetry is wrong — it is Claude→Codex only, and its
spawn API is currently broken. Ask which tool and which versions before
evaluating further.

## The "hashes" are message IDs

AgentChute message IDs are **timestamp + 128-bit random suffix**. No output
hashes, no cursors. Identifiers that *name* messages — opaque enough to sound
like hashes when relayed secondhand.

(The herdr layer separately exposes `pane_id` from `agent start`, and
`HERDR_ENV` / `HERDR_PANE_ID` / `HERDR_BIN_PATH` / `HERDR_SOCKET_PATH` inside a
pane. Also addresses, not hashes.)

## Against amp-bridge — same problem, opposite delivery guarantee

The instinct is to frame this as ambient-shared-state versus point-to-point.
That framing is wrong. Per-agent inboxes are *addressed delivery*, and message
IDs are a request-identity primitive structurally like amp-bridge's
`amp-bridge-req-<12hex>`. The two designs are closer than they look.

The real divergence is delivery:

- **amp-bridge** — push with a deadline. The message is steered into a thread,
  correlated to a turn (`agent.start.message` → `agent.end`), and a watchdog
  fails fast when no turn starts. The sender is blocked and needs an answer.
- **AgentChute** — deposit without one. Pull-only: the message sits in the
  inbox until the recipient volunteers to look, and a coding agent mid-task has
  no reason to look.

**Pull-only makes [[an-append-is-not-a-send]] structural rather than
accidental.** amp-bridge's version of that failure was a bug — a missing
`steer` flag — and was fixed. In a pull-only protocol it is the design. That
AgentChute *had* a wake adapter and deliberately removed it in v0.8 is the most
informative fact available: worth asking what replaced it. If the answer is
"the orchestrator tells the agent to check its inbox", the guarantee is
orchestrator-in-the-loop, not protocol-level, and it does not survive the
orchestrator being busy.

Two open questions in the same area:

- **A unique ID names a message; it does not correlate a reply to a request.**
  That needs reply-to semantics. amp-bridge's marker is *echoed back*, not
  merely assigned.
- **No cursors** leaves open how a reader knows what it already consumed —
  destructive read, or dedup pushed onto the reader. 128-bit IDs at least make
  reader-side dedup possible.

## The concession worth keeping

Pull-only is the *right* design for fan-out: leave notes for five agents, block
on none of them. amp-bridge would be the wrong tool for that — one thread, one
deadline, one blocked caller. Choose by whether the sender needs an answer or
merely needs to have said it.

**Verdict: evaluate**, pending the wake-replacement and reply-to answers.

## Method note

My first pass got two things wrong, both from stopping too early. I called
AgentChute a herdr extension (true historically, removed in v0.8), and I
reported that the KB had nothing on herdr when `tools/herdr.md` had existed
since May — one qmd query returned unrelated hits and I read that as absence.
**A miss is not a gap.** Before reporting a KB gap, check the obvious path
directly; before describing an integration, check whether it is current or
historical.

Related: [[herdr]], [[an-append-is-not-a-send]], [[amp-cli-one-executor-per-thread]]
