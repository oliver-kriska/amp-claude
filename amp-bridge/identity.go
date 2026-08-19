package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
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
	Name      string `json:"name"`
	SessionID string `json:"session_id,omitempty"`
	ClaudePID int    `json:"claude_pid,omitempty"`
	BridgePID int    `json:"bridge_pid"`
	CWD       string `json:"cwd,omitempty"`
	Socket    string `json:"socket"`
	StartedAt string `json:"started_at"`
	// Version and Fingerprint identify the build actually running. Comparing
	// paths cannot detect "installed but not restarted" — the path is the same
	// string before and after, while the inode behind it has changed.
	Version     string `json:"version,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// InitializedAt is set once Claude completes the MCP handshake. Without it
	// the only evidence of a working channel is a marker somewhere in an
	// append-only log, which proves something happened once, not that this
	// session is live.
	InitializedAt string `json:"initialized_at,omitempty"`
	SchemaNote    string `json:"note,omitempty"`
}

var safeName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// runtimeDir is per-uid so a shared /tmp cannot cross-wire two users.
func runtimeDir() string {
	if v := os.Getenv("AMP_BRIDGE_DIR"); v != "" {
		return v
	}
	return fmt.Sprintf("/tmp/amp-bridge-%d", os.Getuid())
}

// ensureRuntimeDir creates the per-uid runtime directory, refusing a symlink.
//
// Order matters. /tmp is world-writable, so another local user can pre-create
// our directory name as a symlink pointing anywhere. MkdirAll and Chmod both
// follow symlinks, so checking afterwards is too late: by then we have already
// chmod'ed 0700 onto a directory of the attacker's choosing. Inspect first,
// mutate second, and use Mkdir rather than MkdirAll — mkdir(2) does not follow a
// symlink in the final path component, so a dangling link fails with EEXIST
// instead of creating the target.
func ensureRuntimeDir() (string, error) {
	dir := runtimeDir()

	switch _, err := ownRuntimeDir(); {
	case errors.Is(err, os.ErrNotExist):
		if mkErr := os.Mkdir(dir, 0o700); mkErr != nil {
			return "", fmt.Errorf("create runtime dir %s: %w", dir, mkErr)
		}
		return dir, nil
	case err != nil:
		return "", err
	}

	// Tighten rather than refuse: this is our own directory, it may predate a
	// version that set the mode, and chmod succeeds precisely because we own it.
	// The read-only side does refuse — see trustedRuntimeDir.
	//
	// #nosec G302 -- 0700 is the tightest mode a directory can have and still
	// be traversable; the socket and registry inside it are 0600.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
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
	name = capSocketName(dir, name)
	sock = filepath.Join(dir, name+".sock")
	if v := os.Getenv("AMP_BRIDGE_SOCKET"); v != "" {
		sock = v // explicit override wins, single-session mode
	}

	entry = registryEntry{
		Name:        name,
		BridgePID:   os.Getpid(),
		Socket:      sock,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		Version:     serverVersion,
		Fingerprint: buildFingerprint(),
		SchemaNote:  "amp-bridge registry; discovered via `amp-bridge --list`",
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

// trustedRuntimeDir returns the runtime directory only if it is one we can
// trust: a real directory, owned by us, that nobody else can write to.
//
// Both the server and the *client* must check this. /tmp is world-writable, so
// another local user can pre-create our directory name — as a symlink, or as a
// perfectly ordinary directory of their own holding a live socket and a matching
// registry entry. Either way `--ask` then delivers the user's prompt straight
// into it. That is exfiltration, not merely denial of service.
//
// The symlink refusal alone was not enough, and neither is the mode: a directory
// owned by someone else, mode 0700, passes a permissions check and is still
// theirs. Ownership is the load-bearing half.
func trustedRuntimeDir() (string, error) {
	dir, err := ownRuntimeDir()
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	// Refused, not tightened. A directory that is world-writable *now* may
	// already hold a socket and a registry entry someone else planted, and
	// chmod would only stop the next one. The server tightens it on its next
	// start; until then, reading it is not safe.
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf(
			"runtime dir %s is mode %04o — group/other access must be off (chmod 700 %s)",
			dir, perm, dir)
	}
	return dir, nil
}

// ownRuntimeDir checks the properties no amount of chmod can repair: that the
// path is a real directory and that it is ours. Ownership is the load-bearing
// half — a directory owned by another uid passes any permissions check while
// still being theirs.
func ownRuntimeDir() (string, error) {
	dir := runtimeDir()
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("runtime dir %s is a symlink — refusing to use it", dir)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("runtime dir %s exists but is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if uid := os.Getuid(); int(st.Uid) != uid {
			return "", fmt.Errorf(
				"runtime dir %s is owned by uid %d, not %d — refusing to use it",
				dir, st.Uid, uid)
		}
	}
	return dir, nil
}

// buildFingerprint is a content hash of the running executable, computed once.
//
// It exists so a registry entry can say which *build* is serving, not merely
// which path was used to start it. Reinstalling over a running bridge leaves the
// path identical and the running code stale, which is the single most common
// self-inflicted failure here and the one path comparison cannot see.
var buildFingerprint = sync.OnceValue(func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	// #nosec G304 -- our own executable.
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
})

// capSocketName keeps dir/name.sock inside the platform's ~104-byte sockaddr_un
// limit. Overflowing it fails at bind with an opaque error that names neither
// the length nor the culprit.
func capSocketName(dir, name string) string {
	const pathLimit = 100
	budget := max(pathLimit-len(dir)-len("/")-len(".sock"), 8)
	if len(name) > budget {
		name = strings.TrimRight(name[:budget], "-")
	}
	return name
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
// It refuses an untrustworthy runtime directory rather than reading whatever it
// finds there — see trustedRuntimeDir.
func listBridges() ([]registryEntry, error) {
	dir, err := trustedRuntimeDir()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // nothing has run yet; not an error
		}
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
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
	return out, nil
}
