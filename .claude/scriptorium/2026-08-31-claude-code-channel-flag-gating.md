---
scriptorium: true
action: create
title: "Claude Code channel gating: --channels vs --dangerously-load-development-channels"
type: research
domain: general
tags: [claude-code, mcp, channels, cli-flags, managed-settings, amp-bridge]
---

# Claude Code channel gating: `--channels` vs `--dangerously-load-development-channels`

Verified against Claude Code **2.1.251** (`~/.local/share/claude/versions/2.1.251`,
Mach-O arm64) on 2026-08-31.

## The question

Starting a session with a locally-developed MCP channel server prints:

```
WARNING: Loading development channels
--dangerously-load-development-channels is for local channel development only.
Do not use this option to run channels you have downloaded off the internet.
Please use --channels to run a list of approved channels.
```

The warning reads like `--channels` is a drop-in replacement. It is not.

## Answer

`--channels` cannot load a `server:` entry. The CLI carries an explicit rejection
reason for exactly that case:

```
server: entries need --dangerously-load-development-channels
```

It sits in a list of entry-resolution failure reasons alongside
`no MCP server configured with that name`, `plugin not installed`,
`not on your org's approved channels list`, and
`not on the approved channels allowlist (use --dangerously-load-development-channels for local dev)`.

So for an MCP server you wrote yourself, the dangerous flag is the only path, and
the warning is expected rather than a symptom of misconfiguration.

## Both flags are hidden but real

Neither appears in `claude --help` (274 lines, checked in full). Both are accepted
by the parser — `claude --channels server:amp-bridge --version` returns the version
with no "unknown option". Their real help text lives in the binary:

- `--channels <servers...>` — "MCP servers whose channel notifications (inbound
  push) should register this session. Space-separated server names."
- `--dangerously-load-development-channels <servers...>` — "Load channel servers
  not on the approved allowlist. For local channel development only. Shows a
  confirmation dialog at startup."

## The failure mode is silence

Passing `--channels server:amp-bridge` is **not** an error. The entry is unmatched,
no channel registers, and the session starts normally. Nothing in the transcript
says the channel is missing — it presents identically to a broken bridge. This is
the trap worth remembering: the "safe" flag fails quietly, the "dangerous" flag
works loudly.

## There is no user-level config key

Both settings that govern the allowlist are **managed-org** policy, not user
settings:

- `channelsEnabled` — "Managed-org opt-in for channel notifications (MCP servers
  with the `claude/channel` capability pushing inbound messages). claude.ai
  Teams/Enterprise: default off. Console: default on unless managed settings
  exist. Set true to allow; users then select servers via `--channels`."
- `allowedChannelPlugins` — "Managed-org allowlist of channel **plugins** … Requires
  `channelsEnabled: true`."

Note the asymmetry: `channelsEnabled` gates *MCP servers*, but the allowlist that
`--channels` consults is a **plugin** allowlist. An MCP server has no allowlist to
be added to at all, which is why `server:` entries are hard-routed to the
development flag. On this machine neither
`/Library/Application Support/ClaudeCode/managed-settings.json` nor
`/etc/claude-code/managed-settings.json` exists.

**Packaging as a plugin does not escape the flag.** The
[channels reference](https://code.claude.com/docs/en/channels-reference#package-as-a-plugin)
is explicit: "A channel published to your own marketplace still needs
`--dangerously-load-development-channels` to run, since it isn't on the approved
allowlist." While channels are in research preview the default allowlist is the
channel plugins in `claude-plugins-official`, curated by Anthropic at its
discretion — and the in-app community-marketplace submission forms do **not** put
a plugin on it. The only alternatives are an official-marketplace listing via an
Anthropic partner contact, or a Team/Enterprise admin's `allowedChannelPlugins`,
which replaces the default allowlist for that org.

The docs also state the bypass is **per entry**: combining
`--dangerously-load-development-channels` with `--channels` does not extend the
bypass to the `--channels` entries.

There is also an undocumented `--managed-settings` flag (rejects with
"`--managed-settings ignored: invalid JSON object`"), but self-granting policy
through it is still a per-session flag — no gain over the flag it would replace,
and a worse shape.

## Why the empirical route failed

Four probes could not distinguish the two flags, because the *mode* was the
confound, not the flag:

| Probe | Result |
|---|---|
| `--channels … -p "…"` | ran, no registration |
| `--dangerously-… … -p "…"` | ran, no registration |
| `--channels … --bg "…"` | session started, no registration |
| `--dangerously-… … --bg "…"` | session started, no registration |

Print mode and background mode register no channel under **either** flag, and
neither renders the startup info notice. Only an interactive session does. Reading
the binary's strings settled in one pass what four sessions could not.

**Transferable method:** when two CLI flags cannot be told apart behaviourally,
`strings` on the bundled binary recovers the hidden help text *and* the internal
rejection reasons. Rejection reasons are the higher-value find — they state the
precondition directly, in the vendor's own words, without needing to trigger it.

## Related

- [[Amp ↔ Claude Code bridge — research from the Claude Code side]] — already
  records the `channelsEnabled: true` gate as a step-0 blocker; this extends it
  with the flag-level detail.
