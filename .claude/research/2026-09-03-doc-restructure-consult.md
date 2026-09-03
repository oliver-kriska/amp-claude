# Doc restructure after the long development stretch

Consulted Amp on thread `T-01a042ab-f21a-73dc-a12a-d710bd174124`, 2026-09-03.
Same pattern as the previous two consults: the call died at ~110s on the pre-fix
build, the message landed, and the answer came back via
`amp threads markdown <id>` (read-only, starts no turn).

## The question

Three overlapping prose documents had drifted after a long development stretch:
README.md (891 lines, 24 top-level headings), AGENTS.md (222), and the embedded
skill. What is the division of labour, is the README too long, and what would a
reader most likely get wrong?

## Consensus

**Division of labour.** My working theory was right except on one point: AGENTS.md
has *two* jobs here, not one — an agent working **on** the repo, and Amp
**operating** the bridge from it. So it needs build/protocol invariants *and*
concise operational rules.

- README — human evaluation, install, first exchange, the few safety rules
- AGENTS.md — repo work + Amp's operational rules
- Skill — authoritative runtime instructions for Claude

Delivery semantics belong in all three because each audience must act on them.
**Duplicate the invariant, not the exposition**: README gets the plain-language
consequence, AGENTS a concise operational rule, the skill the exact statuses and
recovery action. Neither README nor AGENTS reproduces the skill's troubleshooting
matrix.

**Two stale facts, both verified against the source before acting:**

1. `no-turn` maps to `deliveryPending` (`inbox.go`) and then to `status="pending"`
   (`mcp.go`), but README:257 and SKILL:209 still said `status="error"`. Stale
   since `0ed553f`. This one directly drives resend behaviour, so it was the most
   dangerous thing in the docs.
2. README's configuration table said `AMP_BRIDGE_AMP_TIMEOUT` bounds both
   `ask_amp` and `send_amp`. It has bounded only `ask_amp` since `0ed553f`;
   `AMP_BRIDGE_SEND_TIMEOUT` was mentioned in the FAQ but missing from the table.

**Broader mental-model risk** Amp named: *"inbox enabled means background
worker."* Enablement grants **addressability, not wake-up**. Worth repeating
wherever it can be misread.

**What not to document.** Do not put in user-facing prose: test-budget mechanics
(`4c92c67`), the additive `delivered` wire field, internal delivery-state type
names where they differ from external channel statuses, orphan timers and lane
internals, or the `max_timeout` advertisement protocol. Document observable
contracts instead. `timeout_ms` belongs only in the skill, because Claude sees
that attribute and must respect it. Stale-skill detection deserves one sentence,
no fingerprint mechanics.

I checked one point of possible disagreement — AGENTS.md's "Before changing the
protocol" section — and Amp is right: that section is about MCP handshake quirks,
not the inbox wire protocol, so the `delivered` field does not belong there
either. Its reasoning lives in the code comment.

## What was implemented

README 891 → **261 lines**. New structure: identity/status, quick start, daily
use, three-parts responsibility table, how it works + thread-state matrix, five
critical rules, compressed "Why bridge them?", requirements, installing,
development pointer, documentation index.

Three new files, sections moved **verbatim** so the existing prose survived:

- `docs/getting-started.md` (409) — register, check, the first two-way
  conversation, use it, pairing, reaching an open thread, reload/restart scope
- `docs/operations.md` (177) — scopes, configuration, security, uninstall, FAQ
- `docs/development.md` (38) — build/test, the three deliberate protocol
  oddities, further reading

Plus: both stale facts fixed; a `not-delivered` row added to the skill's channel
table (it was the only completion status missing, and the only one where
resending *is* correct); an `empty-answer` triage row in AGENTS.md; a docs
pointer at the top of AGENTS.md; every relative link and anchor re-checked after
the move.

`make check` green.

## Note for next time

The three consults on this thread all timed out at exactly 110s and all
succeeded anyway. Until this session's bridge is restarted on the post-`0ed553f`
build, treat the timeout as expected and go straight to `amp threads markdown`.
A background poll loop watching for the last `## Assistant` block with no
`Tool Use:` after it is a reliable way to wait for the answer.
