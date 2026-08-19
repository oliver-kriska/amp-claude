package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// `amp-bridge doctor` — because every failure this bridge has ever had was
// silent.
//
// Wrong protocol era, wrong capability key, `_meta` instead of `meta`, a stale
// MCP config, a binary whose code signature was invalidated by cp, .mcp.json
// pointing at a path that no longer holds the current build: in every one of
// those cases the server started, reported healthy, and delivered nothing. The
// point of this command is that none of them should cost a day twice.
//
// A diagnostic that reports green when the system is broken is worse than no
// diagnostic, so each check here compares against observed reality — the running
// build's fingerprint, a completed handshake, an executable that actually
// executes — rather than against configuration that merely parses.

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
)

func (s checkStatus) symbol() string {
	switch s {
	case statusOK:
		return "ok  "
	case statusWarn:
		return "warn"
	default:
		return "FAIL"
	}
}

type check struct {
	name   string
	status checkStatus
	detail string
	fix    string
}

func cmdDoctor(cfg config, dir string, strict bool) int {
	checks := []check{
		checkBinary(),
		checkMCPConfig(dir),
		checkRuntimeDir(),
		checkLiveSessions(dir),
		checkAmpCLI(cfg),
		checkLog(),
	}

	worst := statusOK
	fmt.Println("amp-bridge doctor")
	fmt.Println()
	for _, c := range checks {
		fmt.Printf("  [%s] %-22s %s\n", c.status.symbol(), c.name, c.detail)
		if c.status != statusOK && c.fix != "" {
			fmt.Printf("         %s\n", c.fix)
		}
		if c.status > worst {
			worst = c.status
		}
	}

	fmt.Println()
	fmt.Println("To start a Claude session with the channel loaded:")
	fmt.Println("  claude --dangerously-load-development-channels server:" + serverName)

	// A warning is a state the operator may be in on purpose — no session started
	// yet is the expected state of a pre-flight check, and exiting non-zero on
	// the happy path teaches people to ignore the tool. --strict is for the
	// caller who wants a gate rather than a report.
	if worst == statusFail || (strict && worst == statusWarn) {
		if strict && worst == statusWarn {
			fmt.Println()
			fmt.Println("(--strict: warnings are failures)")
		}
		return 1
	}
	return 0
}

// checkBinary catches the drift that bites hardest: several copies of the
// binary at different paths, only one of which Claude actually launches.
func checkBinary() check {
	exe, err := os.Executable()
	if err != nil {
		return check{"binary", statusWarn, "cannot resolve own path: " + err.Error(), ""}
	}
	exe, _ = filepath.EvalSymlinks(exe)

	onPath, lookErr := exec.LookPath(serverName)
	if lookErr != nil {
		return check{
			"binary", statusWarn, exe + " (not on PATH)",
			"add its directory to PATH so `" + serverName + " --list` works from anywhere",
		}
	}
	onPath, _ = filepath.EvalSymlinks(onPath)
	if onPath != exe {
		return check{
			"binary", statusWarn,
			fmt.Sprintf("running %s, but PATH resolves to %s", exe, onPath),
			"two builds are installed; `make install` to make them agree",
		}
	}
	return check{"binary", statusOK, exe + " (build " + buildFingerprint() + ")", ""}
}

// mcpEntry is the whole server entry, not just the command. Validating one field
// and ignoring the rest is checking the lock and not the door: `type: "http"` or
// `args: ["--list"]` both produce a config that parses, points at the right
// binary, and cannot serve the channel.
type mcpEntry struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

func checkMCPConfig(dir string) check {
	path := filepath.Join(dir, ".mcp.json")
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied project dir
	if err != nil {
		// No project config is not a problem if the bridge is registered for
		// every project. Reporting "no .mcp.json" at someone who deliberately
		// installed it user-wide would be a false warning, and the whole point
		// of this command is not to cry wolf.
		if entry, ok := userScopeEntry(); ok {
			if c := checkMCPShape(entry, dir); c.status != statusOK {
				return c
			}
			c := checkMCPTarget(entry.Command, selfPath())
			c.detail = "user config, all projects: " + c.detail
			return c
		}
		return check{
			"mcp config", statusWarn, "no .mcp.json in " + dir + " and no user-scope entry",
			"run `" + serverName + " init " + dir + "` for this project, " +
				"or `claude mcp add " + serverName + " --scope user -- " + selfPath() + "` for all of them",
		}
	}
	var cfg struct {
		MCPServers map[string]mcpEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return check{"mcp config", statusFail, path + " is not valid JSON: " + err.Error(), ""}
	}
	entry, ok := cfg.MCPServers[serverName]
	if !ok {
		return check{
			"mcp config", statusFail, path + " has no " + serverName + " server",
			"run `" + serverName + " init " + dir + "` to add it",
		}
	}
	if c := checkMCPShape(entry, dir); c.status != statusOK {
		return c
	}
	return checkMCPTarget(entry.Command, selfPath())
}

func selfPath() string {
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

// userScopeEntry reads the bridge's registration from ~/.claude.json, which is
// where `claude mcp add --scope user` puts a server that applies to every
// project rather than one.
func userScopeEntry() (mcpEntry, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return mcpEntry{}, false
	}
	// #nosec G304 -- a fixed filename in the user's own home directory.
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return mcpEntry{}, false
	}
	var cfg struct {
		MCPServers map[string]mcpEntry `json:"mcpServers"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return mcpEntry{}, false
	}
	entry, ok := cfg.MCPServers[serverName]
	return entry, ok
}

// checkMCPShape validates everything about the entry except whether the binary
// runs.
func checkMCPShape(entry mcpEntry, dir string) check {
	fix := "run `" + serverName + " init " + dir + "` to rewrite the entry"

	if entry.Type != "" && entry.Type != "stdio" {
		return check{
			"mcp config", statusFail,
			"type is " + entry.Type + `, but a channel must be "stdio"`,
			fix,
		}
	}
	if len(entry.Args) > 0 {
		// With no arguments the binary serves. With any, it does something else
		// and exits, and Claude reports only that the server went away.
		return check{
			"mcp config", statusFail,
			"has args " + strings.Join(entry.Args, " ") + ", which makes it exit instead of serve",
			fix,
		}
	}
	if entry.Command == "" {
		return check{"mcp config", statusFail, "has no command", fix}
	}
	fi, err := os.Stat(entry.Command)
	if err != nil {
		return check{
			"mcp config", statusFail,
			"points at " + entry.Command + ", which does not exist",
			"run `" + serverName + " init " + dir + "` to repoint it at the installed binary",
		}
	}
	if !fi.Mode().IsRegular() {
		return check{"mcp config", statusFail, entry.Command + " is not a regular file", fix}
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return check{
			"mcp config", statusFail,
			fmt.Sprintf("%s is mode %04o — not executable", entry.Command, fi.Mode().Perm()),
			"chmod +x " + entry.Command,
		}
	}
	return check{"mcp config", statusOK, "", ""}
}

// checkMCPTarget runs the configured binary. Everything above this point can
// pass while the file is unrunnable — a Mach-O whose signature `cp` invalidated
// is a regular, executable file that the kernel SIGKILLs at exec. That failure
// is only observable by executing it, and claiming to diagnose it without doing
// so would be exactly the false green this command exists to prevent.
//
// Executing is proportionate here: this is the binary Claude already launches
// unattended, and `--help` is its most inert entry point.
func checkMCPTarget(command, self string) check {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// #nosec G204 -- the path comes from the project's own .mcp.json, which is
	// the file Claude reads to spawn this same binary.
	cmd := exec.CommandContext(ctx, command, "--help")
	// Cancelling the context kills the process we started, but not its children,
	// and CombinedOutput waits for the output pipes to close — which a surviving
	// grandchild holds open. Without WaitDelay a `sleep 30` wrapper made this
	// check wait 30 seconds despite its 5-second deadline, which is exactly the
	// kind of quiet unbounded wait this command exists to expose.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	switch {
	case ctx.Err() != nil:
		return check{
			"mcp config", statusFail, command + " did not respond to --help within 5s",
			"it may be hanging on startup; run it by hand",
		}
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == -1 {
			return check{
				"mcp config", statusFail,
				command + " was killed by the OS at exec (signal, not exit code)",
				"on macOS this is an invalidated code signature — rebuild with " +
					"`make install`, never `cp`, over a running binary",
			}
		}
		return check{
			"mcp config", statusFail,
			fmt.Sprintf("%s exited %v running --help: %s", command, err, firstLine(out)),
			"the configured binary does not run; `make install` to replace it",
		}
	}

	// Resolve both sides: /tmp is a symlink to /private/tmp on macOS, so
	// comparing a resolved path against an unresolved one reports drift between
	// a binary and itself.
	target, _ := filepath.EvalSymlinks(command)
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if self != "" && target != "" && self != target {
		return check{
			"mcp config", statusWarn,
			"Claude launches " + target + ", but this is " + self,
			"if you rebuilt, run `make install` — Claude is running the older binary",
		}
	}
	return check{"mcp config", statusOK, command, ""}
}

func checkRuntimeDir() check {
	dir := runtimeDir()
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return check{"runtime dir", statusOK, dir + " (not created yet — no bridge has run)", ""}
	}
	// Same predicate the client uses before dialling anything inside it, so this
	// check cannot report green on a directory --ask would refuse (or worse,
	// one it would trust when it should not).
	if _, err := trustedRuntimeDir(); err != nil {
		return check{
			"runtime dir", statusFail, err.Error(),
			"another user may have pre-created it; remove it and restart the bridge",
		}
	}
	return check{"runtime dir", statusOK, dir, ""}
}

// checkLiveSessions reports the sessions that can actually serve this project.
//
// Three things can be wrong while a session is listed and alive: it belongs to a
// different project, it is running a build that is no longer the installed one,
// or Claude never completed the handshake so the channel was never registered.
// Counting such a session as green is how "installed but not restarted" went
// undetected.
func checkLiveSessions(dir string) check {
	bridges, err := listBridges()
	if err != nil {
		return check{
			"live sessions", statusFail, err.Error(),
			"the runtime directory cannot be trusted — remove it and restart",
		}
	}
	if len(bridges) == 0 {
		return check{
			"live sessions", statusWarn, "none",
			"start Claude with the channel flag shown below",
		}
	}

	want, _ := filepath.Abs(dir)
	want, _ = filepath.EvalSymlinks(want)
	fingerprint := buildFingerprint()

	var mine, others []string
	status, fix := statusOK, ""
	for _, e := range bridges {
		desc := fmt.Sprintf("%s (claude_pid=%d)", e.Name, e.ClaudePID)
		cwd, _ := filepath.EvalSymlinks(e.CWD)
		if e.CWD != "" && want != "" && cwd != want {
			others = append(others, desc)
			continue
		}
		switch {
		case e.Fingerprint == "":
			// Published by a build from before fingerprinting existed, which is
			// itself proof the session predates the installed binary. Saying so
			// beats both a false green and a misleading handshake complaint.
			desc += " — predates build fingerprinting"
			status, fix = statusWarn, "that session is running an older build; restart it"
		case fingerprint != "" && e.Fingerprint != fingerprint:
			desc += " — running build " + e.Fingerprint + ", installed is " + fingerprint
			status, fix = statusWarn, "that session predates the current binary; restart it to pick up the new build"
		case e.InitializedAt == "":
			desc += " — started but Claude never completed the handshake"
			status, fix = statusWarn, "the session may not have been launched with the channel flag"
		}
		mine = append(mine, desc)
	}

	switch {
	case len(mine) == 0:
		return check{
			"live sessions", statusWarn,
			fmt.Sprintf("none for %s (%d elsewhere: %s)", dir, len(others), strings.Join(others, ", ")),
			"start Claude in this project with the channel flag shown below",
		}
	case len(others) > 0:
		return check{
			"live sessions", status,
			strings.Join(mine, ", ") + fmt.Sprintf(" (+%d in other projects)", len(others)), fix,
		}
	}
	return check{"live sessions", status, strings.Join(mine, ", "), fix}
}

func checkAmpCLI(cfg config) check {
	if cfg.ampDisabled {
		return check{"amp cli", statusOK, "outbound disabled (AMP_BRIDGE_DISABLE_OUTBOUND=1)", ""}
	}
	path, err := exec.LookPath(cfg.ampBin)
	if err != nil {
		return check{
			"amp cli", statusWarn, cfg.ampBin + " not found on PATH",
			"the ask_amp tool will fail; install Amp or set AMP_BIN",
		}
	}
	return check{"amp cli", statusOK, path, ""}
}

// checkLog reports where to look. It deliberately does NOT read evidence of a
// working channel out of the log: the file is append-only, so a CLIENT_INITIALIZED
// line written months ago satisfies a substring search forever. That evidence
// now lives per-session in the registry, where it expires with the process.
func checkLog() check {
	path := defaultLogPath()
	fi, err := os.Stat(path)
	if err != nil {
		return check{"log", statusOK, path + " (no run yet)", ""}
	}
	age := time.Since(fi.ModTime()).Round(time.Second)
	return check{"log", statusOK, fmt.Sprintf("%s (last write %s ago)", path, age), ""}
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 120)
}
