package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// `amp-bridge init --amp-plugin` — install the Amp-side half.
//
// The Go binary alone cannot reach a thread somebody has open, because Amp
// allows one executor per thread. This plugin runs inside that Amp session and
// gives it an inbox. The two halves are installed separately on purpose: an
// Amp-hosted plugin cannot ship or provision a native Go binary, and pretending
// otherwise would produce an install that half-works.
//
// The file is embedded rather than copied from the repository so that a user who
// installed with `go install` — no checkout, no Makefile — can still install it.

// The Amp plugin source. Kept byte-identical to the copy committed under
// .amp/plugins/ by `make plugin`; `make check` fails if the two drift, which is
// this project's signature failure mode (an installed artefact quietly older
// than the build that reads it).
//
//go:embed plugin/amp-bridge-inbox.ts
var pluginSource string

const pluginFileName = "amp-bridge-inbox.ts"

// pluginDigest is the content hash doctor compares an installed copy against.
func pluginDigest() string {
	sum := sha256.Sum256([]byte(pluginSource))
	return hex.EncodeToString(sum[:])[:16]
}

func fileDigest(path string) (string, bool) {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied install path
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16], true
}

// ampGlobalPluginDir is where Amp loads personal plugins from, for every
// project. Honouring XDG_CONFIG_HOME rather than hardcoding ~/.config: users who
// set it expect tools to respect it, and silently writing elsewhere would leave
// a plugin that never loads and no clue why.
func ampGlobalPluginDir() (string, error) {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "amp", "plugins"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "amp", "plugins"), nil
}

// cmdInitAmpPlugin installs the Amp-side half, either into one project or into
// Amp's personal plugin directory.
//
// Global install changes only WHERE the file is loaded from, never what it does
// when loaded: the plugin still binds nothing, writes nothing and creates no
// directory until someone enables a specific thread. That is what makes loading
// it into every Amp session an acceptable default rather than an ambient
// listener.
func cmdInitAmpPlugin(dir string, global, force bool) int {
	var plugins string
	if global {
		g, err := ampGlobalPluginDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "amp-bridge: %v\n", err)
			return 1
		}
		plugins = g
	} else {
		root, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "amp-bridge: %v\n", err)
			return 1
		}
		plugins = filepath.Join(root, ".amp", "plugins")
	}
	target := filepath.Join(plugins, pluginFileName)

	// Refuse to clobber a divergent file. Someone may have edited it, and
	// silently replacing local changes is not an install, it is data loss.
	if have, ok := fileDigest(target); ok && have != pluginDigest() && !force {
		fmt.Fprintf(os.Stderr,
			"amp-bridge: %s already exists and differs from this build\n"+
				"  installed: %s\n  this build: %s\n"+
				"re-run with --force to overwrite it\n",
			target, have, pluginDigest())
		return 1
	}

	// 0750 on the directory: nothing here needs to be world-readable, and this
	// is code an Amp session will execute. The file's mode is left to
	// writeFileAtomic, which creates at 0600 and otherwise preserves whatever is
	// already there — so a copy checked out from git keeps its 0644 instead of
	// being silently retightened under the user every time they reinstall.
	if err := os.MkdirAll(plugins, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "amp-bridge: %v\n", err)
		return 1
	}
	if err := writeFileAtomic(target, []byte(pluginSource)); err != nil {
		fmt.Fprintf(os.Stderr, "amp-bridge: %v\n", err)
		return 1
	}

	fmt.Printf("installed %s\n", target)
	if global {
		fmt.Println("every Amp session on this machine will now load it (and do nothing until enabled)")
	}
	fmt.Println()
	fmt.Println("In the Amp session you want Claude to reach:")
	fmt.Println("  1. reload plugins — the `load_plugin` tool, or Ctrl+O → `plugins: reload`")
	fmt.Println("  2. send a message if the session is new (a fresh `amp` has no thread yet)")
	fmt.Println("  3. Ctrl+O → 'amp-bridge: Enable Claude inbox for this thread'")
	fmt.Println()
	fmt.Println("For an Amp-managed/background target, use 'Enable Claude inbox for another thread'")
	fmt.Println("from a local Amp thread and paste the target URL or id.")
	fmt.Println()
	fmt.Println("That one enable survives plugin reloads and local Amp process restarts until disabled.")
	fmt.Println("Claude can append requests whenever that local thread is open.")
	fmt.Println("Amp must already be active or the user must start the queued turn.")
	return 0
}
