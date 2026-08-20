---
scriptorium: true
action: create
title: "One-shot reply channels need a visible deadline"
type: pattern
domain: general
tags: [agents, ipc, timeouts, protocol-design, failure-modes]
---

# One-shot reply channels need a visible deadline

When two agents talk over a request/reply channel where the answering side gets **one**
chance to respond per request id, the answering side must be able to see how much time it
has. Otherwise a turn that does real work silently overruns the caller's deadline, and the
answerer only discovers it at the moment it tries to reply — after all the work is done.

## How it shows up

Observed building the Amp ↔ Claude Code bridge (`amp-claude`). An Amp thread sent a review
request; the Claude side spent several minutes revising a plan document *before* calling
`reply`, and got:

```
no pending Amp request matches request_id … — it may have already timed out. Reply dropped.
```

Nothing malfunctioned. The caller's `replyWait` was 180 s, and the channel design assumed a
turn *answers* rather than *works*. The failure is invisible until the very end, which is the
worst possible time to learn about it.

## Why the obvious fix is wrong

Raising the timeout moves the cliff without making it visible. The next long turn hits it
again, just later, and the answering side still cannot tell how close it is.

## The pattern

1. **Put the deadline in the event metadata.** The request should carry its own budget so
   the answering side can see it on arrival and decide up front: answer now, or acknowledge
   and defer.
2. **Failing that, adopt "reply first, then work."** If a request will take longer than a
   short answer, send a pointer immediately (to a file, a path, a ticket) and do the work
   after. This is a workaround, not a fix — it depends on the answerer *guessing* the budget.
3. **A one-shot reply id cannot carry a holding message.** There is no way to send "working
   on it" and a result on the same id, so either the protocol grows a progress frame or the
   answerer must pick one message and make it count.
4. **Design the timeout to be loud at the start, not silent at the end.** Same principle as
   a `doctor` subcommand for silent config failures: surface the constraint when it can still
   change behaviour.

## Related

Applies to any agent-to-agent messaging where one side blocks on a read deadline: MCP tool
calls with long-running handlers, subagent request/response, cron-triggered work queues. The
tell is a protocol where the *caller's* patience is configuration and the *answerer's*
workload is unbounded.

See also [[amp-cli-one-executor-per-thread]], [[doctor-subcommand-for-silent-failures]].
