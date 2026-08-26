package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// `amp-bridge init` — write the project's .mcp.json pointing at THIS binary.
//
// Hand-editing that file is the step people get wrong, and the failure is
// invisible: a path that no longer exists, or one that points at a stale build,
// produces a session where the channel simply never delivers. Resolving the path
// from os.Executable also keeps the file portable — it is generated per machine
// rather than shipped with someone's home directory baked into it.
//
// This command rewrites a file inside a directory it did not create, so it is
// deliberately conservative: it refuses anything it does not understand instead
// of replacing it. An untrusted repository can ship a .mcp.json, and the naive
// version of this followed a symlink there and overwrote whatever it pointed at.

type mcpServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

func cmdInit(dir string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "amp-bridge: cannot resolve own path: %v\n", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	path := filepath.Join(dir, ".mcp.json")
	doc, previous, existed, err := loadMCPConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "amp-bridge: %v\n", err)
		return 1
	}

	servers, _ := doc["mcpServers"].(map[string]any)
	servers[serverName] = mcpServer{
		Type:    "stdio",
		Command: exe,
		Args:    []string{},
		Env:     map[string]string{},
	}
	doc["mcpServers"] = servers

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "amp-bridge: %v\n", err)
		return 1
	}
	if err := writeFileAtomic(path, append(out, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "amp-bridge: cannot write %s: %v\n", path, err)
		return 1
	}

	switch {
	case !existed:
		fmt.Printf("added %s to %s\n", serverName, path)
	case sameCommand(previous, exe):
		fmt.Printf("%s already pointed at %s\n", path, exe)
	default:
		fmt.Printf("updated %s to point at %s\n", path, exe)
	}

	fmt.Println()
	fmt.Println("Next: start a Claude session with the channel loaded —")
	fmt.Println("  claude --dangerously-load-development-channels server:" + serverName)
	fmt.Println()
	fmt.Println("Then check everything agrees:")
	fmt.Println("  " + serverName + " doctor " + dir)
	return 0
}

// loadMCPConfig reads an existing .mcp.json for merging, refusing anything whose
// shape it cannot safely preserve. It always returns a document with an
// "mcpServers" object ready to write into.
func loadMCPConfig(path string) (doc map[string]any, previous any, existed bool, err error) {
	// Lstat, not Stat: writing through a symlink would rewrite the file it
	// points at, anywhere on disk, at the choosing of whoever shipped the repo.
	// Refusing is clearer than replacing the link, which would silently break a
	// deliberate layout.
	if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(path)
		return nil, nil, false, fmt.Errorf(
			"%s is a symlink to %s — refusing to write through it; "+
				"remove the link if you want a real file here", path, target,
		)
	}

	doc = map[string]any{}
	data, rerr := os.ReadFile(path) // #nosec G304 -- operator-supplied project dir
	switch {
	case errors.Is(rerr, os.ErrNotExist):
		doc["mcpServers"] = map[string]any{}
		return doc, nil, false, nil
	case rerr != nil:
		return nil, nil, false, rerr
	}

	// A file holding `null` or `[]` is valid JSON and not a config. Unmarshalling
	// `null` into the map leaves it nil, and the write that followed used to
	// panic rather than explain.
	if uerr := json.Unmarshal(data, &doc); uerr != nil {
		return nil, nil, false, fmt.Errorf("%s exists but is not valid JSON: %w", path, uerr)
	}
	if doc == nil {
		return nil, nil, false, fmt.Errorf(
			"%s contains JSON null, not an object — refusing to overwrite it", path,
		)
	}

	raw, present := doc["mcpServers"]
	servers, ok := raw.(map[string]any)
	if present && !ok {
		return nil, nil, false, fmt.Errorf(
			"%s has an \"mcpServers\" key that is not an object (%T) — "+
				"refusing to replace it", path, raw,
		)
	}
	if servers == nil {
		servers = map[string]any{}
	}
	doc["mcpServers"] = servers

	previous, existed = servers[serverName]
	return doc, previous, existed, nil
}

// writeFileAtomic writes via a same-directory temp file and renames over the
// target, so an interrupted run cannot leave a truncated config behind. New
// files are 0600; an existing file is tightened only when it is group- or
// world-writable, since that is what would let someone else change the command
// Claude executes. Its mode is otherwise the project's business, not ours.
func writeFileAtomic(path string, data []byte) error {
	perm := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		if p := fi.Mode().Perm(); p&0o022 == 0 {
			perm = p
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".mcp.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func sameCommand(previous any, exe string) bool {
	m, ok := previous.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := m["command"].(string)
	return cmd == exe
}
