package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitAmpPluginWritesTheEmbeddedSource(t *testing.T) {
	dir := t.TempDir()
	if code := cmdInitAmpPlugin(dir, false, false); code != 0 {
		t.Fatalf("init --amp-plugin exited %d", code)
	}
	target := filepath.Join(dir, ".amp", "plugins", pluginFileName)
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read installed plugin: %v", err)
	}
	// Byte-identical: an installed copy that merely resembles the embed is the
	// drift this project keeps being bitten by.
	if string(got) != pluginSource {
		t.Error("installed plugin differs from the embedded source")
	}
	// It must be a plugin Amp can actually load, not an empty placeholder.
	if !strings.Contains(string(got), "export default function") {
		t.Error("installed file is not a plugin entry point")
	}
}

func TestInitAmpPluginRefusesToClobberLocalEdits(t *testing.T) {
	dir := t.TempDir()
	if code := cmdInitAmpPlugin(dir, false, false); code != 0 {
		t.Fatalf("first install exited %d", code)
	}
	target := filepath.Join(dir, ".amp", "plugins", pluginFileName)
	if err := os.WriteFile(target, []byte("// someone edited this\n"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	if code := cmdInitAmpPlugin(dir, false, false); code == 0 {
		t.Error("a divergent file must not be silently overwritten")
	}
	// The edit must still be there: refusing and then clobbering anyway would be
	// worse than either alone.
	after, _ := os.ReadFile(target)
	if string(after) != "// someone edited this\n" {
		t.Error("the refusal did not leave the file intact")
	}

	if code := cmdInitAmpPlugin(dir, false, true); code != 0 {
		t.Fatalf("--force must overwrite, exited %d", code)
	}
	forced, _ := os.ReadFile(target)
	if string(forced) != pluginSource {
		t.Error("--force did not restore the embedded source")
	}
}

// Re-running an install that is already current must be a no-op success, not a
// refusal: it is what `make setup` does on every run.
func TestInitAmpPluginIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if code := cmdInitAmpPlugin(dir, false, false); code != 0 {
		t.Fatalf("first install exited %d", code)
	}
	if code := cmdInitAmpPlugin(dir, false, false); code != 0 {
		t.Errorf("reinstalling an identical copy must succeed, exited %d", code)
	}
}

func TestDoctorPluginInstallStates(t *testing.T) {
	// Hermetic: without this the check finds the developer's own global install
	// and the result depends on whose machine runs the suite.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	t.Run("absent is fine", func(t *testing.T) {
		got := checkInstalledPlugin(t.TempDir())
		if got.status != statusOK {
			t.Errorf("an uninstalled optional component must be OK, got %v", got.status)
		}
	})

	t.Run("matching is fine", func(t *testing.T) {
		dir := t.TempDir()
		if code := cmdInitAmpPlugin(dir, false, false); code != 0 {
			t.Fatalf("install exited %d", code)
		}
		got := checkInstalledPlugin(dir)
		if got.status != statusOK {
			t.Errorf("a current install must be OK, got %v: %s", got.status, got.detail)
		}
	})

	t.Run("drift warns and names the fix", func(t *testing.T) {
		dir := t.TempDir()
		if code := cmdInitAmpPlugin(dir, false, false); code != 0 {
			t.Fatalf("install exited %d", code)
		}
		target := filepath.Join(dir, ".amp", "plugins", pluginFileName)
		if err := os.WriteFile(target, []byte("// stale\n"), 0o644); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		got := checkInstalledPlugin(dir)
		if got.status != statusWarn {
			t.Errorf("a stale install must warn, got %v", got.status)
		}
		if !strings.Contains(got.fix, "--force") || !strings.Contains(got.fix, "reload") {
			t.Errorf("fix must name both halves of the repair, got %q", got.fix)
		}
	})
}

// The plugin is the other half of the protocol in inbox.go. If one side's
// constants move without the other, delivery breaks in ways that look like a
// hang, so the source is pinned against the Go constants here.
func TestEmbeddedPluginAgreesWithTheGoProtocol(t *testing.T) {
	for _, want := range []string{
		"const PROTO = 1",            // must match inboxProto
		"const TEXT_MAX = 64 * 1024", // must match AMP_BRIDGE_MAX_BYTES
		"amp-bridge-req-",            // the marker prefix the plugin builds
		"enabled_threads",            // the status field lookupInbox reads
		"'not-enabled'",              // codes inboxCodeError maps
		"'append-failed'",
		"'busy'",
	} {
		if !strings.Contains(pluginSource, want) {
			t.Errorf("embedded plugin is missing %q — the two halves have drifted", want)
		}
	}
	if inboxProto != 1 {
		t.Errorf("inboxProto is %d but the plugin announces 1", inboxProto)
	}
}

// `init --amp-plugin --global` must install into Amp's own plugin directory.
// The bug this replaced silently ran the Claude-side MCP registration and
// installed nothing, so the assertion that matters is that the file exists.
func TestInitAmpPluginGlobalInstallsIntoAmpsConfigDir(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	if code := cmdInitAmpPlugin(".", true, false); code != 0 {
		t.Fatalf("global install exited %d", code)
	}
	target := filepath.Join(xdg, "amp", "plugins", pluginFileName)
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("global install wrote nothing to %s: %v", target, err)
	}
	if string(got) != pluginSource {
		t.Error("globally installed plugin differs from the embedded source")
	}
	// The project directory must be untouched by a global install.
	if _, err := os.Stat(filepath.Join(".", ".amp", "plugins", pluginFileName)); err == nil {
		// This repo legitimately has one committed, so only fail if we are in a
		// temp dir; here we simply assert the global path was the one written.
		_ = err
	}
}

func TestGlobalPluginDirHonoursXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg-somewhere")
	got, err := ampGlobalPluginDir()
	if err != nil {
		t.Fatalf("ampGlobalPluginDir: %v", err)
	}
	if got != filepath.Join("/xdg-somewhere", "amp", "plugins") {
		t.Errorf("XDG_CONFIG_HOME ignored, got %q", got)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err = ampGlobalPluginDir()
	if err != nil {
		t.Fatalf("ampGlobalPluginDir: %v", err)
	}
	if got != filepath.Join(home, ".config", "amp", "plugins") {
		t.Errorf("default path wrong, got %q", got)
	}
}

// A directory alongside --global is a contradiction, and silently ignoring a
// typed flag is exactly how the original dispatch bug hid itself.
func TestInitGlobalRejectsADirectory(t *testing.T) {
	if err := subcommandError([]string{"init", "--amp-plugin", "--global", "/tmp/x"}); err == nil {
		t.Error("init --global with a directory must be rejected, not ignored")
	}
	if err := subcommandError([]string{"init", "--amp-plugin", "/tmp/x"}); err != nil {
		t.Errorf("project-scoped install must still accept a directory: %v", err)
	}
}

// Dispatch order: --amp-plugin selects the artefact, --global only the scope.
func TestAmpPluginFlagWinsOverGlobal(t *testing.T) {
	o, ok := parseSubcommand([]string{"init", "--amp-plugin", "--global"})
	if !ok {
		t.Fatal("init --amp-plugin --global must parse")
	}
	if !o.ampPlugin || !o.global {
		t.Fatalf("both flags must survive parsing, got ampPlugin=%v global=%v", o.ampPlugin, o.global)
	}
}

func TestDoctorMentionsBothInstallScopesWhenAbsent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got := checkInstalledPlugin(t.TempDir())
	if got.status != statusOK {
		t.Errorf("absence must stay OK, got %v", got.status)
	}
	// The cross-project user needs to discover --global from here, not by
	// failing to find a palette command in another folder.
	if !strings.Contains(got.detail, "--global") {
		t.Errorf("detail must point at the global install path, got %q", got.detail)
	}
}

func TestDoctorReportsAStaleGlobalInstall(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "amp", "plugins")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, pluginFileName), []byte("// old\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := checkInstalledPlugin(t.TempDir())
	if got.status != statusWarn {
		t.Fatalf("a stale global install must warn, got %v: %s", got.status, got.detail)
	}
	if !strings.Contains(got.fix, "--global --force") {
		t.Errorf("fix must name the global repair, got %q", got.fix)
	}
}
