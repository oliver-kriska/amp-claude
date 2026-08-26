package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readMCP(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("invalid JSON written: %v", err)
	}
	return doc
}

func serverCommand(t *testing.T, doc map[string]any, name string) string {
	t.Helper()
	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("no mcpServers object: %v", doc)
	}
	entry, ok := servers[name].(map[string]any)
	if !ok {
		return ""
	}
	cmd, _ := entry["command"].(string)
	return cmd
}

func TestInitWritesTheRunningBinaryPath(t *testing.T) {
	dir := shortTempDir(t)
	if code := cmdInit(dir); code != 0 {
		t.Fatalf("cmdInit = %d, want 0", code)
	}

	// The path must be resolved, not hardcoded: a checked-in .mcp.json with
	// someone else's home directory in it is the classic silent failure.
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	if got := serverCommand(t, readMCP(t, dir), serverName); got != exe {
		t.Errorf("command = %q, want this binary at %q", got, exe)
	}
}

func TestInitPreservesOtherServers(t *testing.T) {
	dir := shortTempDir(t)
	existing := `{"mcpServers":{"other":{"type":"stdio","command":"/usr/bin/other"}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(existing), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if code := cmdInit(dir); code != 0 {
		t.Fatalf("cmdInit = %d, want 0", code)
	}
	doc := readMCP(t, dir)
	if got := serverCommand(t, doc, "other"); got != "/usr/bin/other" {
		t.Errorf("clobbered an unrelated MCP server: %q", got)
	}
	if serverCommand(t, doc, serverName) == "" {
		t.Error("did not add the bridge")
	}
}

func TestInitRefusesToClobberInvalidJSON(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if code := cmdInit(dir); code == 0 {
		t.Error("init must refuse rather than overwrite a file it cannot parse")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{not json" {
		t.Error("the unparseable file was overwritten")
	}
}

func TestDoctorExitCode(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", shortTempDir(t))

	// A broken MCP config is a FAIL, and doctor must exit non-zero so it can be
	// used as a gate rather than only read by a human.
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"),
		[]byte(`{"mcpServers":{"amp-bridge":{"command":"/nope"}}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code := cmdDoctor(testConfig(), dir, false); code != 1 {
		t.Errorf("cmdDoctor = %d, want 1 when a check fails", code)
	}
}

func TestCapSocketName(t *testing.T) {
	t.Parallel()
	// Overflowing sockaddr_un fails at bind with an error naming neither the
	// length nor the culprit, so the name is capped before it gets there.
	long := strings.Repeat("a", 200)
	got := capSocketName("/tmp/amp-bridge-501", long)
	full := "/tmp/amp-bridge-501/" + got + ".sock"
	if len(full) > 104 {
		t.Errorf("socket path is %d bytes: %s", len(full), full)
	}
	if short := capSocketName("/tmp/amp-bridge-501", "amp-claude-b1"); short != "amp-claude-b1" {
		t.Errorf("a short name must be left alone, got %q", short)
	}
}

func TestTrustedRuntimeDirRefusesASymlink(t *testing.T) {
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

	// Discovery must refuse too, not just publication: following a planted
	// symlink would post the user's prompt into someone else's socket.
	if _, err := trustedRuntimeDir(); err == nil {
		t.Error("trustedRuntimeDir must refuse a symlink")
	}
	if _, err := listBridges(); err == nil {
		t.Error("listBridges must surface the refusal rather than reporting no sessions")
	}
}

// script writes an executable stand-in for a bridge binary.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "amp-bridge")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func mcpConfigWith(t *testing.T, entry string) string {
	t.Helper()
	dir := shortTempDir(t)
	body := `{"mcpServers":{"amp-bridge":` + entry + `}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

func TestCheckMCPConfigShape(t *testing.T) {
	// checkMCPConfig falls back to ~/.claude.json when a project has no config,
	// so the developer's own user-scope registration would otherwise leak in and
	// turn the "missing" cases green.
	t.Setenv("HOME", shortTempDir(t))
	good := script(t, "exit 0")

	tests := []struct {
		name   string
		entry  string
		want   checkStatus
		detail string
	}{
		{"missing file", "", statusWarn, "no .mcp.json"},
		{
			// Parses, names the right binary, and cannot serve a channel.
			"wrong transport type",
			`{"type":"http","command":"` + good + `"}`,
			statusFail, `must be "stdio"`,
		},
		{
			// The nastiest shape: it starts, prints, and exits, so Claude reports
			// only that the server went away.
			"args make it exit instead of serve",
			`{"type":"stdio","command":"` + good + `","args":["--list"]}`,
			statusFail, "exit instead of serve",
		},
		{"no command", `{"type":"stdio"}`, statusFail, "no command"},
		{
			"command does not exist",
			`{"type":"stdio","command":"/nonexistent/amp-bridge"}`,
			statusFail, "does not exist",
		},
		{
			"command is a directory",
			`{"type":"stdio","command":"/tmp"}`,
			statusFail, "not a regular file",
		},
		{"healthy", `{"type":"stdio","command":"` + good + `"}`, statusWarn, "but this is"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortTempDir(t)
			if tc.entry != "" {
				dir = mcpConfigWith(t, tc.entry)
			}
			got := checkMCPConfig(dir)
			if got.status != tc.want {
				t.Errorf("status = %s, want %s (%s / %s)",
					got.status.symbol(), tc.want.symbol(), got.detail, got.fix)
			}
			if !strings.Contains(got.detail, tc.detail) {
				t.Errorf("detail %q should mention %q", got.detail, tc.detail)
			}
		})
	}
}

func TestCheckMCPConfigRejectsANonExecutable(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "amp-bridge")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := checkMCPConfig(mcpConfigWith(t, `{"type":"stdio","command":"`+path+`"}`))
	if got.status != statusFail || !strings.Contains(got.detail, "not executable") {
		t.Errorf("want FAIL/not executable, got %s %q", got.status.symbol(), got.detail)
	}
}

func TestCheckMCPTargetRunsTheBinary(t *testing.T) {
	// Not parallel: the subtest below adjusts the shared probe timeout.

	// A regular, executable file that the OS kills at exec is indistinguishable
	// from a healthy one until you run it — that is the invalidated-signature
	// case, and claiming to diagnose it without executing would be a false green.
	killed := script(t, "kill -9 $$")
	got := checkMCPTarget(killed, killed)
	if got.status != statusFail {
		t.Errorf("a binary killed by a signal must FAIL, got %s %q", got.status.symbol(), got.detail)
	}
	if !strings.Contains(got.fix, "code signature") {
		t.Errorf("the fix should name the signature case, got %q", got.fix)
	}

	broken := script(t, "echo 'cannot start' >&2; exit 3")
	if got := checkMCPTarget(broken, broken); got.status != statusFail {
		t.Errorf("a binary that exits non-zero must FAIL, got %s", got.status.symbol())
	}

	fine := script(t, "exit 0")
	if got := checkMCPTarget(fine, fine); got.status != statusOK {
		t.Errorf("a healthy binary should pass, got %s %q", got.status.symbol(), got.detail)
	}

	// Last, because it shortens the shared probe timeout: a binary that never
	// returns must be bounded rather than hanging doctor. Shortened so the test
	// need not wait the real bound out; what is under test is that the bound
	// holds at all, and that a grandchild holding the output pipe cannot
	// extend it past it.
	restore := mcpProbeTimeout
	mcpProbeTimeout = 2 * time.Second
	defer func() { mcpProbeTimeout = restore }()

	hangs := script(t, "sleep 30")
	start := time.Now()
	if got := checkMCPTarget(hangs, hangs); got.status != statusFail {
		t.Errorf("a hanging binary must FAIL, got %s", got.status.symbol())
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("waited %s for a `sleep 30` binary; the timeout did not bound it", elapsed)
	}
}

func TestDoctorStrictPromotesWarnings(t *testing.T) {
	dir := shortTempDir(t) // no .mcp.json -> warn
	t.Setenv("AMP_BRIDGE_DIR", shortTempDir(t))

	if code := cmdDoctor(testConfig(), dir, false); code != 0 {
		t.Errorf("warnings must not fail by default (pre-flight is the happy path), got %d", code)
	}
	if code := cmdDoctor(testConfig(), dir, true); code != 1 {
		t.Errorf("--strict must turn a warning into a gate failure, got %d", code)
	}
}

func TestDoctorChecksTimeoutOrdering(t *testing.T) {
	t.Parallel()

	healthy := testConfig()
	healthy.ampDisabled = false
	healthy.replyWait = 3 * time.Minute
	healthy.ampTimeout = 2 * time.Minute
	if got := checkTimeoutOrdering(healthy); got.status != statusOK {
		t.Errorf("default ordering should be healthy, got %s: %s", got.status.symbol(), got.detail)
	}

	for _, tc := range []struct {
		name       string
		ampTimeout time.Duration
		want       string
	}{
		{"exactly the recovery margin", 140 * time.Second, "need >30s"},
		{"Amp can outlive Claude", 4 * time.Minute, "outlive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := healthy
			cfg.ampTimeout = tc.ampTimeout
			got := checkTimeoutOrdering(cfg)
			if got.status != statusWarn {
				t.Fatalf("status = %s, want warn: %s", got.status.symbol(), got.detail)
			}
			if !strings.Contains(got.detail, tc.want) || !strings.Contains(got.fix, "AMP_BRIDGE_AMP_TIMEOUT") {
				t.Errorf("warning is not actionable: %q / %q", got.detail, got.fix)
			}
		})
	}

	disabled := healthy
	disabled.ampDisabled = true
	disabled.ampTimeout = 10 * time.Minute
	if got := checkTimeoutOrdering(disabled); got.status != statusOK {
		t.Errorf("outbound-disabled bridge should not warn, got %s", got.status.symbol())
	}
}

func TestMCPServerAlreadyExists(t *testing.T) {
	t.Parallel()
	if !mcpServerAlreadyExists([]byte("MCP server amp-bridge already exists in user config")) {
		t.Error("Claude's duplicate-registration diagnostic should allow skill refresh")
	}
	if mcpServerAlreadyExists([]byte("permission denied")) {
		t.Error("an unrelated registration failure must remain fatal")
	}
}

func TestTrustedRuntimeDirRefusesAWorldWritableDir(t *testing.T) {
	dir := shortTempDir(t)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Setenv("AMP_BRIDGE_DIR", dir)

	// A directory anyone can write to can be filled with a registry entry and a
	// socket of someone else's choosing, and --ask would then deliver the user's
	// prompt into it. Refusing to read it is the only safe answer.
	if _, err := trustedRuntimeDir(); err == nil {
		t.Fatal("a mode-0777 runtime dir must be refused")
	}
	if _, err := listBridges(); err == nil {
		t.Error("discovery must refuse it too, not just publication")
	}
	if got := checkRuntimeDir(); got.status != statusFail {
		t.Errorf("doctor must FAIL on it, not warn: %s %q", got.status.symbol(), got.detail)
	}
}

func TestTrustedRuntimeDirAcceptsAPrivateDir(t *testing.T) {
	dir := shortTempDir(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Setenv("AMP_BRIDGE_DIR", dir)

	// The ownership half of the check cannot be exercised without a second uid,
	// so this only pins the mode half plus the happy path.
	if _, err := trustedRuntimeDir(); err != nil {
		t.Errorf("our own 0700 dir must be accepted: %v", err)
	}
}

func TestInitRefusesToWriteThroughASymlink(t *testing.T) {
	dir := shortTempDir(t)
	victim := filepath.Join(shortTempDir(t), "victim.json")
	if err := os.WriteFile(victim, []byte(`{"keep":"me"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, ".mcp.json")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// An untrusted repo can ship .mcp.json as a link to any writable JSON file.
	if code := cmdInit(dir); code == 0 {
		t.Error("init must refuse a symlinked .mcp.json")
	}
	data, _ := os.ReadFile(victim)
	if string(data) != `{"keep":"me"}` {
		t.Errorf("init wrote through the link and clobbered %s: %s", victim, data)
	}
}

func TestInitRefusesShapesItCannotMerge(t *testing.T) {
	for _, body := range []string{
		`null`, // valid JSON, not an object — used to panic on the map write
		`[]`,
		`{"mcpServers":"not-an-object"}`,
		`{"mcpServers":[]}`,
	} {
		t.Run(body, func(t *testing.T) {
			dir := shortTempDir(t)
			path := filepath.Join(dir, ".mcp.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if code := cmdInit(dir); code == 0 {
				t.Errorf("init must refuse %s rather than replacing it", body)
			}
			data, _ := os.ReadFile(path)
			if string(data) != body {
				t.Errorf("the file was rewritten: %s", data)
			}
		})
	}
}

func TestInitLeavesAReasonableModeAlone(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code := cmdInit(dir); code != 0 {
		t.Fatalf("cmdInit = %d", code)
	}

	// 0644 is the normal mode for a git-tracked file; silently tightening it is
	// a side effect nobody asked for. Only group/world *write* is a real risk,
	// because that is what lets someone else change the command Claude runs.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode changed to %04o", fi.Mode().Perm())
	}
}

func TestInitTightensAWorldWritableConfig(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o666); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if code := cmdInit(dir); code != 0 {
		t.Fatalf("cmdInit = %d", code)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm()&0o022 != 0 {
		t.Errorf("left the config writable by others: %04o", fi.Mode().Perm())
	}
}

func TestLiveSessionsNoticesAStaleBuild(t *testing.T) {
	dir := shortTempDir(t)
	runtime := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", runtime)

	sock := filepath.Join(runtime, "s.sock")
	ln, err := bindSocket(sock)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer func() { _ = ln.Close() }()

	entry := registryEntry{
		Name: "stale-session", Socket: sock, CWD: dir,
		Fingerprint:   "0000000000000000", // not this build
		InitializedAt: "2026-08-19T10:00:00Z",
	}
	if _, err := entry.publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// This is the failure doctor used to walk straight past: same path, same
	// name, different inode. Comparing paths cannot see it.
	got := checkLiveSessions(dir)
	if got.status != statusWarn || !strings.Contains(got.detail, "running build") {
		t.Errorf("want a stale-build warning, got %s %q", got.status.symbol(), got.detail)
	}
	if !strings.Contains(got.fix, "restart") {
		t.Errorf("fix should say to restart the session, got %q", got.fix)
	}
}

func TestLiveSessionsSeparatesOtherProjects(t *testing.T) {
	mine := shortTempDir(t)
	runtime := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", runtime)

	sock := filepath.Join(runtime, "o.sock")
	ln, err := bindSocket(sock)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer func() { _ = ln.Close() }()

	entry := registryEntry{
		Name: "elsewhere", Socket: sock, CWD: shortTempDir(t),
		Fingerprint: buildFingerprint(), InitializedAt: "2026-08-19T10:00:00Z",
	}
	if _, err := entry.publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// A healthy bridge in someone else's project is not evidence about this one.
	got := checkLiveSessions(mine)
	if got.status != statusWarn || !strings.Contains(got.detail, "elsewhere") {
		t.Errorf("want a warning naming the other project, got %s %q",
			got.status.symbol(), got.detail)
	}
}

func TestLiveSessionsNoticesAnIncompleteHandshake(t *testing.T) {
	dir := shortTempDir(t)
	runtime := shortTempDir(t)
	t.Setenv("AMP_BRIDGE_DIR", runtime)

	sock := filepath.Join(runtime, "h.sock")
	ln, err := bindSocket(sock)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Running, discoverable, and never asked to serve a channel — the shape of a
	// session started without the channel flag.
	entry := registryEntry{
		Name: "no-handshake", Socket: sock, CWD: dir, Fingerprint: buildFingerprint(),
	}
	if _, err := entry.publish(); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := checkLiveSessions(dir)
	if got.status != statusWarn || !strings.Contains(got.detail, "handshake") {
		t.Errorf("want a handshake warning, got %s %q", got.status.symbol(), got.detail)
	}
}

func TestCheckMCPConfigAcceptsAUserScopeEntry(t *testing.T) {
	home := shortTempDir(t)
	t.Setenv("HOME", home)
	good := script(t, "exit 0")

	body := `{"mcpServers":{"amp-bridge":{"type":"stdio","command":"` + good + `"}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// `claude mcp add --scope user` registers the bridge for every project, so a
	// project with no .mcp.json is correctly configured. Warning at someone who
	// installed it deliberately is the cry-wolf failure this command must avoid.
	got := checkMCPConfig(shortTempDir(t))
	if got.status == statusWarn && strings.Contains(got.detail, "no .mcp.json") {
		t.Errorf("user-scope registration should satisfy the check, got %q", got.detail)
	}
	if !strings.Contains(got.detail, "user config") {
		t.Errorf("detail should say where the registration came from, got %q", got.detail)
	}
}

func TestCheckMCPConfigWarnsWithNoRegistrationAnywhere(t *testing.T) {
	t.Setenv("HOME", shortTempDir(t))
	got := checkMCPConfig(shortTempDir(t))
	if got.status != statusWarn {
		t.Errorf("want warn, got %s", got.status.symbol())
	}
	// Both routes, because which one is right depends on whether the user wants
	// this in one project or all of them.
	for _, want := range []string{"init", "--scope user"} {
		if !strings.Contains(got.fix, want) {
			t.Errorf("fix should mention %q, got %q", want, got.fix)
		}
	}
}

func TestParseSubcommandFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args    []string
		mode    runMode
		dir     string
		strict  bool
		global  bool
		wantErr bool
	}{
		{args: []string{"doctor"}, mode: modeDoctor, dir: "."},
		{args: []string{"doctor", "--strict"}, mode: modeDoctor, dir: ".", strict: true},
		{args: []string{"doctor", "/p", "--strict"}, mode: modeDoctor, dir: "/p", strict: true},
		{args: []string{"init"}, mode: modeInit, dir: "."},
		{args: []string{"init", "--global"}, mode: modeInit, dir: ".", global: true},
		{args: []string{"init", "/p"}, mode: modeInit, dir: "/p"},
		// A flag that belongs to the other subcommand is a typo, and a typo that
		// silently does nothing is the failure mode this binary keeps designing
		// against.
		{args: []string{"init", "--strict"}, wantErr: true},
		{args: []string{"doctor", "--global"}, wantErr: true},
		{args: []string{"doctor", "--nope"}, wantErr: true},
		{args: []string{"init", "/a", "/b"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			got, err := parseArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if got.mode != tc.mode || got.dir != tc.dir ||
				got.strict != tc.strict || got.global != tc.global {
				t.Errorf("got mode=%v dir=%q strict=%v global=%v",
					got.mode, got.dir, got.strict, got.global)
			}
		})
	}
}

func TestEmbeddedSkillIsPresent(t *testing.T) {
	t.Parallel()
	// A `go install` user has no repository, so the skill has to travel inside
	// the binary. An empty embed would make `init --global` silently install
	// nothing.
	if len(skillDoc) < 500 {
		t.Errorf("embedded skill is %d bytes; it should be the real document", len(skillDoc))
	}
	if !strings.Contains(skillDoc, "request_id") {
		t.Error("embedded skill does not look like the amp-bridge skill")
	}
	if !strings.Contains(skillDoc, "Do not omit `--thread`") {
		t.Error("embedded skill does not teach Amp to identify its source thread")
	}
}
