package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests set environment variables, so none of them call t.Parallel.

// shortTempDir keeps paths well inside the ~103-byte Unix socket limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ampb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeSessionFile writes the Claude session self-description the bridge reads
// to learn who spawned it, at the path it will actually look.
func fakeSessionFile(t *testing.T, si sessionInfo) {
	t.Helper()
	home := shortTempDir(t)
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.Marshal(si)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.json", os.Getppid()))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestResolveIdentityFromSessionFile(t *testing.T) {
	runtime := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", runtime)
	fakeSessionFile(t, sessionInfo{
		SessionID: "b959f5dd-fee6-43f6",
		CWD:       "/Users/x/Projects/amp_claude",
		Name:      "amp/claude b1",
	})

	name, sock, entry := resolveIdentity()

	// The name becomes a filename and an address, so it is sanitised.
	if name != "amp-claude-b1" {
		t.Errorf("name = %q, want the sanitised amp-claude-b1", name)
	}
	if want := filepath.Join(runtime, "amp-claude-b1.sock"); sock != want {
		t.Errorf("sock = %q, want %q", sock, want)
	}
	if entry.SessionID != "b959f5dd-fee6-43f6" || entry.CWD != "/Users/x/Projects/amp_claude" {
		t.Errorf("session details not carried into the registry entry: %+v", entry)
	}
	if entry.ClaudePID != os.Getppid() {
		t.Errorf("ClaudePID = %d, want our ppid %d", entry.ClaudePID, os.Getppid())
	}
	if entry.BridgePID != os.Getpid() {
		t.Errorf("BridgePID = %d, want %d", entry.BridgePID, os.Getpid())
	}
}

func TestResolveIdentityFallbacks(t *testing.T) {
	tests := []struct {
		name string
		si   sessionInfo
		want string
	}{
		{"named session wins", sessionInfo{Name: "alpha", SessionID: "sid12345678"}, "alpha"},
		{"session id when unnamed", sessionInfo{SessionID: "sid12345678"}, "cc-sid12345"},
		{"ppid when the file says nothing", sessionInfo{}, fmt.Sprintf("cc-ppid%d", os.Getppid())},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AMP_BRIDGE_DIR", shortTempDir(t))
			fakeSessionFile(t, tc.si)
			if name, _, _ := resolveIdentity(); name != tc.want {
				t.Errorf("name = %q, want %q", name, tc.want)
			}
		})
	}
}

func TestResolveIdentityWithNoSessionFile(t *testing.T) {
	t.Setenv("AMP_BRIDGE_DIR", shortTempDir(t))
	t.Setenv("HOME", shortTempDir(t)) // no .claude/sessions at all

	name, _, entry := resolveIdentity()
	if want := fmt.Sprintf("cc-ppid%d", os.Getppid()); name != want {
		t.Errorf("name = %q, want %q — a missing session file must degrade, not fail", name, want)
	}
	if entry.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", entry.SessionID)
	}
}

func TestSocketOverrideWins(t *testing.T) {
	t.Setenv("AMP_BRIDGE_DIR", shortTempDir(t))
	t.Setenv("HOME", shortTempDir(t))
	t.Setenv("AMP_BRIDGE_SOCKET", "/tmp/explicit.sock")

	if _, sock, _ := resolveIdentity(); sock != "/tmp/explicit.sock" {
		t.Errorf("sock = %q, want the explicit override", sock)
	}
}

func TestShortID(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"b959f5dd-fee6-43f6", "b959f5dd"},
		{"short", "short"},
		{"", ""},
		{"----", "----"}, // hyphens are inside the allowed class
	}
	for _, tc := range tests {
		if got := shortID(tc.in); got != tc.want {
			t.Errorf("shortID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEnsureRuntimeDir(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "run")
	t.Setenv("AMP_BRIDGE_DIR", dir)

	got, err := ensureRuntimeDir()
	if err != nil {
		t.Fatalf("ensureRuntimeDir: %v", err)
	}
	if got != dir {
		t.Errorf("dir = %q, want %q", got, dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The socket lives here and carries conversation traffic; group and other
	// must not be able to reach it.
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("runtime dir mode = %o, want 700", perm)
	}
}

func TestEnsureRuntimeDirTightensLoosePermissions(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "loose")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Setenv("AMP_BRIDGE_DIR", dir)

	if _, err := ensureRuntimeDir(); err != nil {
		t.Fatalf("ensureRuntimeDir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %o, want 700 — a pre-existing loose dir must be tightened", perm)
	}
}

func TestEnsureRuntimeDirRefusesASymlink(t *testing.T) {
	base := shortTempDir(t)
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("AMP_BRIDGE_DIR", link)

	if _, err := ensureRuntimeDir(); err == nil {
		t.Error("a symlinked runtime dir must be refused — it is how another user redirects our socket")
	}
}

func TestBindSocket(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "b.sock")

	ln, err := bindSocket(sock)
	if err != nil {
		t.Fatalf("bindSocket: %v", err)
	}
	defer func() { _ = ln.Close() }()

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
}

func TestBindSocketRefusesToHijackALiveBridge(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "b.sock")

	first, err := bindSocket(sock)
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	defer func() { _ = first.Close() }()

	// Stealing the path would silently cut the running bridge off from Amp.
	if _, err := bindSocket(sock); err == nil {
		t.Fatal("binding over a live socket must fail")
	} else if !strings.Contains(err.Error(), "refusing to hijack") {
		t.Errorf("error should explain the refusal: %v", err)
	}
}

func TestBindSocketClearsAStaleSocket(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "b.sock")

	dead, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := dead.Close(); err != nil { // leaves the file behind
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Skip("this platform removes the socket file on close; nothing stale to clear")
	}

	ln, err := bindSocket(sock)
	if err != nil {
		t.Fatalf("a stale socket must be cleared and reused, got %v", err)
	}
	_ = ln.Close()
}

func TestPublishAndListBridges(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", dir)

	sock := filepath.Join(dir, "live.sock")
	ln, err := bindSocket(sock)
	if err != nil {
		t.Fatalf("bindSocket: %v", err)
	}
	defer func() { _ = ln.Close() }()

	live := registryEntry{Name: "live", Socket: sock, ClaudePID: 42, CWD: "/tmp"}
	path, err := live.publish()
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("registry file mode = %o, want 600", perm)
	}

	// A registration whose socket is gone is a leftover from a crashed bridge.
	stale := registryEntry{Name: "stale", Socket: filepath.Join(dir, "gone.sock")}
	stalePath, err := stale.publish()
	if err != nil {
		t.Fatalf("publish stale: %v", err)
	}

	got := listBridges()
	if len(got) != 1 || got[0].Name != "live" {
		t.Fatalf("listBridges = %+v, want only the live entry", got)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Error("listBridges must sweep the stale registration, not just skip it")
	}
}

func TestListBridgesIgnoresJunkFiles(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", dir)

	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := listBridges(); len(got) != 0 {
		t.Errorf("listBridges = %+v, want none", got)
	}
}

func TestEnsureRuntimeDirDoesNotTouchASymlinkTarget(t *testing.T) {
	base := shortTempDir(t)
	victim := filepath.Join(base, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(victim, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("AMP_BRIDGE_DIR", link)

	if _, err := ensureRuntimeDir(); err == nil {
		t.Fatal("a symlinked runtime dir must be refused")
	}

	// Refusing is not enough. /tmp is world-writable, so another local user can
	// pre-create our directory name as a symlink to a directory of theirs
	// choosing. Chmod follows symlinks, so checking after mutating would already
	// have re-permissioned someone else's directory.
	fi, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("stat victim: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("symlink target mode changed to %o — the check ran after the side effect", perm)
	}
}

func TestEnsureRuntimeDirRefusesADanglingSymlink(t *testing.T) {
	base := shortTempDir(t)
	target := filepath.Join(base, "does-not-exist")
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("AMP_BRIDGE_DIR", link)

	// mkdir(2) does not follow a symlink in the final component, so this must
	// fail rather than quietly creating an attacker-chosen directory.
	if _, err := ensureRuntimeDir(); err == nil {
		t.Fatal("a dangling symlink must be refused, not followed")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the symlink target was created — mkdir followed the link")
	}
}

func TestEnsureRuntimeDirRefusesANonDirectory(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "afile")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("AMP_BRIDGE_DIR", path)

	if _, err := ensureRuntimeDir(); err == nil {
		t.Error("a plain file in place of the runtime dir must be refused")
	}
}
