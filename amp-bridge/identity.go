package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Identity resolution.
//
// A Claude-spawned MCP server inherits no CLAUDE_* environment variables, so the
// bridge cannot learn which session owns it from the environment. It can from its
// parent: Claude Code spawns MCP servers directly, so our PPID is the session's
// PID, and Claude publishes ~/.claude/sessions/<pid>.json describing itself.
//
// That file is an internal Claude Code format (see the research doc) — treat it
// as best-effort enrichment, never a hard dependency. Every failure degrades to
// a PID-derived name rather than refusing to start.

type sessionInfo struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Name      string `json:"name"`
}

// registryEntry is what we publish so the Amp side can discover and choose a
// session. This is our own format, not Claude's.
type registryEntry struct {
	Name       string `json:"name"`
	SessionID  string `json:"session_id,omitempty"`
	ClaudePID  int    `json:"claude_pid,omitempty"`
	BridgePID  int    `json:"bridge_pid"`
	CWD        string `json:"cwd,omitempty"`
	Socket     string `json:"socket"`
	StartedAt  string `json:"started_at"`
	SchemaNote string `json:"note,omitempty"`
}

var safeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// runtimeDir is per-uid so a shared /tmp cannot cross-wire two users.
func runtimeDir() string {
	if v := os.Getenv("AMP_BRIDGE_DIR"); v != "" {
		return v
	}
	return fmt.Sprintf("/tmp/amp-bridge-%d", os.Getuid())
}

func ensureRuntimeDir() (string, error) {
	dir := runtimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// Re-assert perms in case the directory predates us.
	// #nosec G302 -- 0700 is the tightest mode a directory can have and still
	// be traversable; the socket and registry inside it are 0600.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("runtime dir %s is a symlink — refusing to use it", dir)
	}
	return dir, nil
}

// claudeSession reads the parent Claude session's self-description, if present.
func claudeSession() *sessionInfo {
	ppid := os.Getppid()
	if ppid <= 1 {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", ppid))
	// #nosec G304 -- the path is built from our own ppid inside the user's home.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var si sessionInfo
	if err := json.Unmarshal(data, &si); err != nil {
		return nil
	}
	if si.PID == 0 {
		si.PID = ppid
	}
	return &si
}

// resolveIdentity picks the bridge's addressable name and socket path.
func resolveIdentity() (name, sock string, entry registryEntry) {
	si := claudeSession()

	switch {
	case si != nil && si.Name != "":
		name = si.Name
	case si != nil && si.SessionID != "":
		name = "cc-" + shortID(si.SessionID)
	default:
		// No session file (e.g. spawned by `claude mcp get`, or a future
		// Claude Code that stops publishing this). Stay unique, stay usable.
		name = fmt.Sprintf("cc-ppid%d", os.Getppid())
	}
	name = strings.Trim(safeName.ReplaceAllString(name, "-"), "-")
	if name == "" {
		name = fmt.Sprintf("cc-%d", os.Getpid())
	}

	dir := runtimeDir()
	sock = filepath.Join(dir, name+".sock")
	if v := os.Getenv("AMP_BRIDGE_SOCKET"); v != "" {
		sock = v // explicit override wins, single-session mode
	}

	entry = registryEntry{
		Name:       name,
		BridgePID:  os.Getpid(),
		Socket:     sock,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SchemaNote: "amp-bridge registry; discovered via `amp-bridge --list`",
	}
	if si != nil {
		entry.SessionID = si.SessionID
		entry.ClaudePID = si.PID
		entry.CWD = si.CWD
	}
	return name, sock, entry
}

func shortID(s string) string {
	s = safeName.ReplaceAllString(s, "")
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// bindSocket binds the listener, refusing to steal a socket another live bridge
// still owns. A stale socket (nothing listening) is cleared and reused.
func bindSocket(sock string) (net.Listener, error) {
	if _, err := os.Lstat(sock); err == nil {
		if socketIsLive(sock) {
			return nil, fmt.Errorf(
				"another amp-bridge is already listening on %s — "+
					"refusing to hijack it (set AMP_BRIDGE_SOCKET to use a different path)", sock)
		}
		_ = os.Remove(sock) // stale
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", sock, err)
	}
	return ln, nil
}

func socketIsLive(sock string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (e registryEntry) publish() (string, error) {
	dir, err := ensureRuntimeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, e.Name+".json")
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// listBridges returns every live bridge, dropping entries whose socket is dead.
func listBridges() []registryEntry {
	dir := runtimeDir()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil
	}
	var out []registryEntry
	for _, f := range files {
		// #nosec G304 -- f comes from our own glob of our own 0700 runtime dir.
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var e registryEntry
		if json.Unmarshal(data, &e) != nil {
			continue
		}
		if !socketIsLive(e.Socket) {
			_ = os.Remove(f) // sweep stale registration
			continue
		}
		out = append(out, e)
	}
	return out
}
