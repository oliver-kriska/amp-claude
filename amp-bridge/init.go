package main

import (
	"encoding/json"
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

	// Preserve any other servers already configured for this project.
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil { // #nosec G304 -- operator-supplied dir
		if err := json.Unmarshal(data, &doc); err != nil {
			fmt.Fprintf(os.Stderr, "amp-bridge: %s exists but is not valid JSON: %v\n", path, err)
			return 1
		}
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	previous, existed := servers[serverName]

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
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
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
	fmt.Println("  " + serverName + " doctor")
	return 0
}

func sameCommand(previous any, exe string) bool {
	m, ok := previous.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := m["command"].(string)
	return cmd == exe
}
