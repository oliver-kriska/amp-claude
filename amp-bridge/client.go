package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Client mode: the same binary, used from the Amp side to talk to a running
// bridge over its Unix socket.

func cmdList(verbose bool) int {
	bridges, err := listBridges()
	if err != nil {
		fmt.Fprintf(os.Stderr, "amp-bridge: %v\n", err)
		return 2
	}
	if len(bridges) == 0 {
		fmt.Println("no live amp-bridge sessions")
		return 1
	}
	for _, e := range bridges {
		fmt.Printf("%-28s  claude_pid=%-7d  cwd=%s\n", e.Name, e.ClaudePID, e.CWD)
		if verbose {
			fmt.Printf("  bridge_pid=%d  session_id=%s  version=%s  build=%s\n",
				e.BridgePID, valueOrDash(e.SessionID), valueOrDash(e.Version), valueOrDash(e.Fingerprint))
			fmt.Printf("  handshake=%s  started=%s  socket=%s\n",
				valueOrDash(e.InitializedAt), valueOrDash(e.StartedAt), e.Socket)
		}
	}
	return 0
}

func valueOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func pickBridge(want string) (registryEntry, error) {
	bridges, err := listBridges()
	if err != nil {
		return registryEntry{}, err
	}
	if len(bridges) == 0 {
		return registryEntry{}, errors.New(
			"no live amp-bridge sessions.\n" +
				"Is a Claude Code session running with the channel loaded?",
		)
	}
	if want != "" {
		for _, e := range bridges {
			if e.Name == want {
				return e, nil
			}
		}
		return registryEntry{}, fmt.Errorf("no live bridge named %q; live: %s",
			want, strings.Join(bridgeNames(bridges), ", "))
	}
	if len(bridges) > 1 {
		return registryEntry{}, fmt.Errorf(
			"%d live bridges — pass --session <name> to choose: %s",
			len(bridges), strings.Join(bridgeNames(bridges), ", "),
		)
	}
	return bridges[0], nil
}

func bridgeNames(bridges []registryEntry) []string {
	names := make([]string, 0, len(bridges))
	for _, e := range bridges {
		names = append(names, e.Name)
	}
	return names
}

func cmdAsk(cfg config, session, threadID, text string) int {
	target, err := pickBridge(session)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "unix", target.Socket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot reach %s at %s: %v\n", target.Name, target.Socket, err)
		return 2
	}
	defer func() { _ = conn.Close() }()

	if err := json.NewEncoder(conn).Encode(ampRequest{Text: text, ThreadID: threadID}); err != nil {
		fmt.Fprintf(os.Stderr, "send failed: %v\n", err)
		return 2
	}
	// Slightly past the server's own deadline, so a server-side timeout is
	// reported as such rather than as a client read failure.
	if err := conn.SetReadDeadline(time.Now().Add(cfg.replyWait + 10*time.Second)); err != nil {
		fmt.Fprintf(os.Stderr, "set deadline: %v\n", err)
		return 2
	}

	var resp ampResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "no reply: %v\n", err)
		return 2
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "bridge error: %s\n", resp.Error)
		return 1
	}
	fmt.Println(resp.Reply)
	return 0
}
