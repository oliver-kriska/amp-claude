package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

const timeoutRecoveryMargin = 30 * time.Second

func cmdDoctor(cfg config, dir string, strict bool) int {
	checks := []check{
		checkBinary(),
		checkMCPConfig(dir),
		checkRuntimeDir(),
		checkLiveSessions(dir),
		checkPluginInboxes(),
		checkInstalledPlugin(dir),
		checkAmpCLI(cfg),
		checkTimeoutOrdering(cfg),
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

// checkTimeoutOrdering keeps a nested Claude->Amp consultation inside the
// Amp->Claude caller's deadline with enough time left for Claude to deliver its
// final reply. The bridge cannot enforce this per call because ask_amp exposes
// no public timeout argument, so doctor makes a bad environment override loud.
func checkTimeoutOrdering(cfg config) check {
	const name = "timeout ordering"
	if cfg.ampDisabled {
		return check{name, statusOK, "outbound disabled", ""}
	}
	// The plugin's hard socket deadline intentionally sits beyond its own Amp
	// turn budget so a richer plugin diagnosis wins the race. Doctor must use
	// that outer bound, not the optimistic inner budget, at this safety margin.
	outboundBound := cfg.ampTimeout + inboxTimeoutLead
	remaining := cfg.replyWait - outboundBound
	if remaining <= timeoutRecoveryMargin {
		detail := fmt.Sprintf("ask_amp worst-case %s leaves %s before the %s Claude reply deadline (need >%s)",
			outboundBound, remaining, cfg.replyWait, timeoutRecoveryMargin)
		if remaining <= 0 {
			detail = fmt.Sprintf("ask_amp worst-case %s can outlive the %s Claude reply deadline",
				outboundBound, cfg.replyWait)
		}
		return check{
			name, statusWarn,
			detail,
			"lower AMP_BRIDGE_AMP_TIMEOUT or raise AMP_BRIDGE_TIMEOUT so more than 30s remains",
		}
	}
	return check{
		name, statusOK,
		fmt.Sprintf("ask_amp worst-case %s leaves %s before the %s Claude reply deadline",
			outboundBound, remaining, cfg.replyWait), "",
	}
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

// mcpProbeTimeout bounds the --help probe. Generous on purpose: macOS runs a
// security check the first time a freshly installed binary is executed, which
// can take seconds on a loaded machine, and reporting a healthy binary as hung
// would be its own false alarm. Overridden in tests.
var mcpProbeTimeout = 20 * time.Second

// checkMCPTarget runs the configured binary. Everything above this point can
// pass while the file is unrunnable — a Mach-O whose signature `cp` invalidated
// is a regular, executable file that the kernel SIGKILLs at exec. That failure
// is only observable by executing it, and claiming to diagnose it without doing
// so would be exactly the false green this command exists to prevent.
//
// Executing is proportionate here: this is the binary Claude already launches
// unattended, and `--help` is its most inert entry point.
func checkMCPTarget(command, self string) check {
	ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout)
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
			"mcp config", statusFail,
			fmt.Sprintf("%s did not respond to --help within %s", command, mcpProbeTimeout),
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

// checkPluginInboxes reports which Amp threads have opened their Claude inbox,
// and sweeps entries whose plugin is gone.
//
// Absence is never a failure. The plugin is an optional component: most projects
// will not have one, and a doctor that cries about a feature nobody installed
// teaches people to ignore it. What this catches is the state that *is* wrong —
// an entry that looks live but is not, which would otherwise surface later as an
// ask_amp failure with no obvious cause.
func checkPluginInboxes() check {
	const name = "plugin inboxes"

	dir, err := trustedInboxDir()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return check{name, statusOK, "none (no thread has enabled its Claude inbox)", ""}
		}
		return check{
			name, statusFail, err.Error(),
			"the inbox directory cannot be trusted — remove it and re-enable in Amp",
		}
	}
	threads, err := trustedSubdir(dir, "threads")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return check{name, statusOK, "none (no thread has enabled its Claude inbox)", ""}
		}
		return check{
			name, statusFail, err.Error(),
			"the inbox directory cannot be trusted — remove it and re-enable in Amp",
		}
	}

	files, err := filepath.Glob(filepath.Join(threads, "*.json"))
	if err != nil {
		return check{name, statusWarn, err.Error(), ""}
	}
	if len(files) == 0 {
		return check{name, statusOK, "none (no thread has enabled its Claude inbox)", ""}
	}

	var live, stale []string
	for _, f := range files {
		ok, desc := classifyInbox(f)
		if desc == "" {
			continue
		}
		if ok {
			live = append(live, desc)
		} else {
			stale = append(stale, desc)
		}
	}

	switch {
	case len(stale) == 0:
		return check{name, statusOK, strings.Join(live, ", "), ""}
	case len(live) == 0:
		return check{
			name, statusWarn,
			"stale: " + strings.Join(stale, ", "),
			"those Amp sessions are gone or reloaded — re-enable the inbox there (Ctrl+O)",
		}
	default:
		return check{
			name, statusWarn,
			strings.Join(live, ", ") + "; stale: " + strings.Join(stale, ", "),
			"re-enable the inbox in the Amp sessions listed as stale (Ctrl+O)",
		}
	}
}

// classifyInbox decides whether one inbox entry is really serving its thread,
// sweeping it when it is not. It returns ("", "") for an entry it cannot read at
// all, which is neither evidence of health nor worth reporting.
//
// A live socket is not sufficient evidence: onDispose does not run on SIGKILL,
// and a pid-named path could be reused by a later plugin process. Only the
// status handshake proves the plugin behind the socket still serves this thread.
func classifyInbox(path string) (live bool, desc string) {
	// #nosec G304 -- path comes from our own glob of our own 0700 inbox tree.
	data, err := os.ReadFile(path)
	if err != nil {
		return false, ""
	}
	var e inboxEntry
	if json.Unmarshal(data, &e) != nil {
		_ = os.Remove(path)
		return false, filepath.Base(path) + " (unreadable)"
	}
	if !socketIsLive(e.Socket) {
		_ = os.Remove(path) // the session is gone; the entry is noise
		return false, e.ThreadID
	}
	st, err := inboxStatus(context.Background(), e.Socket)
	switch {
	case err != nil:
		return false, e.ThreadID + " (unresponsive)"
	case st.Proto != inboxProto:
		return false, fmt.Sprintf("%s (protocol v%d, want v%d)", e.ThreadID, st.Proto, inboxProto)
	case !slices.Contains(st.EnabledThreads, e.ThreadID):
		return false, e.ThreadID + " (plugin no longer serves it)"
	default:
		return true, fmt.Sprintf("%s (plugin pid %d)", e.ThreadID, e.PluginPID)
	}
}

// checkInstalledPlugin compares a project's installed Amp plugin against the one
// embedded in this binary.
//
// Absent is fine — the plugin is optional and most projects will not have it.
// What matters is a copy that exists and is stale, because that is invisible:
// Amp loads it happily, the socket appears, and only the protocol handshake or a
// missing field eventually gives it away.
func checkInstalledPlugin(dir string) check {
	const name = "amp plugin"
	want := pluginDigest()

	type site struct {
		label, path, force string
	}
	sites := []site{
		{
			"this project", filepath.Join(dir, ".amp", "plugins", pluginFileName),
			"amp-bridge init --amp-plugin --force",
		},
	}
	if g, err := ampGlobalPluginDir(); err == nil {
		sites = append(sites, site{
			"all Amp sessions", filepath.Join(g, pluginFileName),
			"amp-bridge init --amp-plugin --global --force",
		})
	}

	var good, stale []string
	var fix string
	for _, s := range sites {
		have, ok := fileDigest(s.path)
		switch {
		case !ok:
			continue
		case have == want:
			good = append(good, s.label)
		default:
			stale = append(stale, fmt.Sprintf("%s (%s, this build embeds %s)", s.label, have, want))
			if fix == "" {
				fix = "run `" + s.force + "`, then reload it in Amp"
			}
		}
	}

	switch {
	case len(good) == 0 && len(stale) == 0:
		// Optional component. Say where it would go rather than warning about
		// its absence: a cross-project user needs the global form, and finding
		// that out only by failing to enable an inbox is the DX bug we just hit.
		return check{
			name, statusOK,
			"not installed (optional — `init --amp-plugin` per project, or `--amp-plugin --global` for all)", "",
		}
	case len(stale) == 0:
		return check{name, statusOK, "installed for " + strings.Join(good, " and "), ""}
	case len(good) == 0:
		return check{name, statusWarn, "stale: " + strings.Join(stale, "; "), fix}
	default:
		return check{
			name, statusWarn,
			"installed for " + strings.Join(good, " and ") + "; stale: " + strings.Join(stale, "; "), fix,
		}
	}
}
