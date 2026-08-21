---
scriptorium: true
action: create
title: "An append is not a send"
type: solution
domain: general
tags: [amp, agent-protocols, timeouts, watchdog, api-semantics]
---

Amp's plugin API exposes `thread.appendUserMessage(...)`. It does what it says:
it appends a message to a thread. It does **not** start a turn. The only option
it takes is `{ steer?: boolean }` — "prefer this message when it is queued
behind in-progress work."

Anything built on it that assumes delivery implies a reply will hang for its
full timeout instead of failing.

## The two states that look identical from outside

| thread state at append | what happens | how long |
|---|---|---|
| `running` | the message queues behind the in-progress turn | until that turn ends — possibly past your budget |
| `idle` | nothing is in progress to finish, so nothing triggers a turn — the message just sits there | forever |

The caller sees the same thing in both cases: silence, then a timeout. The
second case is strictly worse than a failure, because the message *did* arrive.
The natural response to a timeout is to retry, and retrying posts the question
into the thread a second time.

## What to do

**Pass `steer: true`.** It is the documented lever for the queued case and costs
nothing when the thread is idle.

**Watchdog the state, don't watchdog the clock.** After appending, poll the
thread state until a turn starts. `running` — or a state you cannot read — means
keep waiting; the caller's own timeout is the bound. Any other state with no
turn started means it will never be answered, so fail immediately. Scale the
poll interval to the caller's budget (`clamp(timeout/4, 250ms, 20s)`) so a short
ask isn't told slowly and a long ask doesn't poll pointlessly, and bound the
probe itself so a wedged state call doesn't become the new hang.

**Make the error say "do not resend."** An error that only reports the failure
invites the exact retry that duplicates the message. Say where the message is,
say what to do about it, and say plainly not to send it again. If an agent
consumes this error, put the same instruction in its skill table — the agent
decides whether to retry before it ever reads your prose.

## The general lesson

When an API's verb is precise — *append*, not *send* — believe the verb. The gap
between "delivered" and "acted on" is where unbounded waits live, and it closes
only when something actively observes the other side's state. A timeout is not
an observation; it is the absence of one.

Related: [[amp-cli-one-executor-per-thread]]
