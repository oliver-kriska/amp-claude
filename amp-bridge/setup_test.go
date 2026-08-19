package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestCheckMCPConfig(t *testing.T) {
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)

	tests := []struct {
		name    string
		content string
		want    checkStatus
		detail  string
	}{
		{"missing file", "", statusWarn, "no .mcp.json"},
		{"invalid json", "{nope", statusFail, "not valid JSON"},
		{"no bridge entry", `{"mcpServers":{}}`, statusFail, "no amp-bridge server"},
		{
			"points at nothing",
			`{"mcpServers":{"amp-bridge":{"command":"/nope/amp-bridge"}}}`,
			statusFail, "does not exist",
		},
		{
			// The drift that actually bites: built but not installed, so Claude
			// keeps launching yesterday's binary and nothing says so.
			"points at a different build",
			`{"mcpServers":{"amp-bridge":{"command":"/bin/sh"}}}`,
			statusWarn, "older binary",
		},
		{
			"agrees with this binary",
			`{"mcpServers":{"amp-bridge":{"command":"` + exe + `"}}}`,
			statusOK, "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortTempDir(t)
			if tc.content != "" {
				if err := os.WriteFile(filepath.Join(dir, ".mcp.json"),
					[]byte(tc.content), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			got := checkMCPConfig(dir)
			if got.status != tc.want {
				t.Errorf("status = %v, want %v (%s / %s)",
					got.status.symbol(), tc.want.symbol(), got.detail, got.fix)
			}
			if tc.detail != "" && !strings.Contains(got.detail+" "+got.fix, tc.detail) {
				t.Errorf("output %q / %q should mention %q", got.detail, got.fix, tc.detail)
			}
		})
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
	if code := cmdDoctor(testConfig(), dir); code != 1 {
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
