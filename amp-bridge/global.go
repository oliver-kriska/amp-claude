package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	switch {
	case err == nil:
		fmt.Printf("registered %s for all projects (user scope)\n", serverName)
	case mcpServerAlreadyExists(out):
		// Upgrades routinely run init again to refresh the embedded skill. Claude
		// refuses to replace an existing registration; keep it and let doctor
		// report if its command points anywhere other than this binary.
		fmt.Printf("%s already registered in Claude's user config; keeping it\n", serverName)
	default:
		fmt.Fprintf(os.Stderr, "amp-bridge: `claude mcp add` failed: %v\n%s\n", err, out)
		return 1
	}

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

func mcpServerAlreadyExists(out []byte) bool {
	return strings.Contains(strings.ToLower(string(out)), "mcp server "+serverName+" already exists")
}

func installSkill() error {
	path, err := installedSkillPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(skillDoc), 0o600)
}

// installedSkillPath is the single place this binary writes the skill, and the
// single place doctor looks for it. Two constructions of the same path is how
// an installer and its own diagnostic end up disagreeing.
func installedSkillPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills", serverName, "SKILL.md"), nil
}

// skillDigest fingerprints the embedded copy so doctor can tell whether the
// installed file came from this build.
func skillDigest() string {
	sum := sha256.Sum256([]byte(skillDoc))
	return hex.EncodeToString(sum[:])[:16]
}
