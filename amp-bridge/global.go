package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// `amp-bridge init --global` — register for every project, not one.
//
// A user who installed with `go install` has no repository and no Makefile, so
// everything needed to set the bridge up has to live in the binary. The skill is
// embedded for exactly that reason.

// The skill that teaches a Claude session how to use the bridge. Kept in sync
// with .claude/skills/amp-bridge/SKILL.md by `make skill`; `make check` fails if
// the two drift.
//
//go:embed skill.md
var skillDoc string

func cmdInitGlobal() int {
	exe := selfPath()

	// Registering through the CLI rather than editing ~/.claude.json directly:
	// that file is Claude Code's own state, it rewrites it on exit, and a
	// concurrent hand-edit is how the documented `--scope local` clobber
	// happens. `claude mcp add` is the supported door.
	claude, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr,
			"amp-bridge: the `claude` CLI is not on PATH, so the MCP server cannot be\n"+
				"registered automatically. Install Claude Code, then run:\n"+
				"  claude mcp add "+serverName+" --scope user -- "+exe)
		return 1
	}

	// Bounded: `claude mcp add` also health-checks servers, and a hung remote
	// MCP server should not leave the installer waiting forever.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// #nosec G204 -- claude comes from PATH and exe is our own resolved path.
	cmd := exec.CommandContext(ctx, claude, "mcp", "add", serverName,
		"--scope", "user", "--", exe)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "amp-bridge: `claude mcp add` failed: %v\n%s\n", err, out)
		return 1
	}
	fmt.Printf("registered %s for all projects (user scope)\n", serverName)

	if err := installSkill(); err != nil {
		// Not fatal: the bridge works without the skill, Claude just has to be
		// told how to use it each time.
		fmt.Fprintf(os.Stderr, "amp-bridge: could not install the skill: %v\n", err)
	} else {
		fmt.Println("installed the amp-bridge skill for all projects")
	}

	fmt.Println()
	fmt.Println("Start any Claude session with the channel loaded:")
	fmt.Println("  claude --dangerously-load-development-channels server:" + serverName)
	fmt.Println()
	fmt.Println("Then:")
	fmt.Println("  " + serverName + " doctor")
	return 0
}

func installSkill() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".claude", "skills", serverName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillDoc), 0o600)
}
