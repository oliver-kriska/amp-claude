package main

import (
	"encoding/json"
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

func cmdDoctor(cfg config, dir string) int {
	checks := []check{
		checkBinary(),
		checkMCPConfig(dir),
		checkRuntimeDir(),
		checkLiveSessions(),
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

	if worst == statusFail {
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
	return check{"binary", statusOK, exe, ""}
}

// checkMCPConfig compares what Claude will launch against what is installed.
// Rebuilding without installing leaves these pointing at different binaries,
// and the only symptom is that new behaviour never appears.
func checkMCPConfig(dir string) check {
	path := filepath.Join(dir, ".mcp.json")
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied project dir
	if err != nil {
		return check{
			"mcp config", statusWarn, "no .mcp.json in " + dir,
			"run `" + serverName + " init` here to register the channel",
		}
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return check{"mcp config", statusFail, path + " is not valid JSON: " + err.Error(), ""}
	}
	entry, ok := cfg.MCPServers[serverName]
	if !ok {
		return check{
			"mcp config", statusFail, path + " has no " + serverName + " server",
			"run `" + serverName + " init` to add it",
		}
	}
	if _, err := os.Stat(entry.Command); err != nil {
		return check{
			"mcp config", statusFail, "points at " + entry.Command + ", which does not exist",
			"run `" + serverName + " init` to repoint it at the installed binary",
		}
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	target, _ := filepath.EvalSymlinks(entry.Command)
	if exe != "" && target != "" && exe != target {
		return check{
			"mcp config", statusWarn,
			"Claude launches " + target + ", but this is " + exe,
			"if you rebuilt, run `make install` — Claude is running the older binary",
		}
	}
	return check{"mcp config", statusOK, entry.Command, ""}
}

func checkRuntimeDir() check {
	dir := runtimeDir()
	fi, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		return check{"runtime dir", statusOK, dir + " (not created yet — no bridge has run)", ""}
	case err != nil:
		return check{"runtime dir", statusFail, err.Error(), ""}
	case fi.Mode()&os.ModeSymlink != 0:
		return check{
			"runtime dir", statusFail, dir + " is a symlink",
			"another user may have pre-created it; remove it and restart",
		}
	case fi.Mode().Perm() != 0o700:
		return check{
			"runtime dir", statusWarn,
			fmt.Sprintf("%s is mode %o, want 700", dir, fi.Mode().Perm()),
			"chmod 700 " + dir,
		}
	}
	return check{"runtime dir", statusOK, dir, ""}
}

func checkLiveSessions() check {
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
	names := make([]string, 0, len(bridges))
	for _, e := range bridges {
		names = append(names, fmt.Sprintf("%s (claude_pid=%d)", e.Name, e.ClaudePID))
	}
	return check{"live sessions", statusOK, strings.Join(names, ", "), ""}
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

// checkLog looks for CLIENT_INITIALIZED, which is the only positive evidence
// that Claude registered the channel listener rather than merely starting us.
func checkLog() check {
	path := defaultLogPath()
	fi, err := os.Stat(path)
	if err != nil {
		return check{"log", statusOK, path + " (no run yet)", ""}
	}
	data, err := os.ReadFile(path) // #nosec G304 -- our own log path
	if err != nil {
		return check{"log", statusWarn, "cannot read " + path, ""}
	}
	age := time.Since(fi.ModTime()).Round(time.Second)
	if !strings.Contains(string(data), "CLIENT_INITIALIZED") {
		return check{
			"log", statusWarn,
			fmt.Sprintf("%s has no CLIENT_INITIALIZED (last write %s ago)", path, age),
			"the server started but Claude never registered the channel — " +
				"was the session launched with the channel flag?",
		}
	}
	return check{"log", statusOK, fmt.Sprintf("%s (last write %s ago)", path, age), ""}
}
