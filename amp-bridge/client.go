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
			fmt.Printf("  handshake=%s  started=%s  timeout=%s  max_timeout=%s  socket=%s\n",
				valueOrDash(e.InitializedAt), valueOrDash(e.StartedAt),
				valueOrDash(e.ReplyTimeout), valueOrDash(e.MaxReplyTimeout), e.Socket)
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

func cmdAsk(cfg config, session, threadID, text string, requestedWait time.Duration) int {
	target, err := pickBridge(session)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if requestedWait > 0 && !supportsRequestedTimeout(target) {
		fmt.Fprintf(os.Stderr,
			"session %s predates per-request timeouts; restart its Claude session before using --timeout\n",
			target.Name,
		)
		return 2
	}
	wait := advertisedWait(target, cfg.replyWait, requestedWait)
	resp, err := exchange(target, ampRequest{
		Text:          text,
		ThreadID:      threadID,
		TimeoutMillis: requestedWait.Milliseconds(),
	}, wait)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "bridge error: %s\n", resp.Error)
		if resp.RequestID != "" {
			fmt.Fprintf(os.Stderr,
				"request_id=%s; check later with: amp-bridge --session %s --result %s\n",
				resp.RequestID, target.Name, resp.RequestID,
			)
		}
		return 1
	}
	fmt.Println(resp.Reply)
	return 0
}

func cmdResult(cfg config, session, requestID string) int {
	target, err := pickBridge(session)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if !supportsRequestedTimeout(target) {
		fmt.Fprintf(os.Stderr,
			"session %s predates retained result lookup; restart its Claude session before using --result\n",
			target.Name,
		)
		return 2
	}
	resp, err := exchange(target, ampRequest{ResultID: requestID}, cfg.replyWait)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "bridge error: %s\n", resp.Error)
		if resp.ExpiresAt != "" {
			fmt.Fprintf(os.Stderr, "retained_until=%s\n", resp.ExpiresAt)
		}
		return 1
	}
	fmt.Println(resp.Reply)
	return 0
}

func advertisedWait(target registryEntry, fallback, requested time.Duration) time.Duration {
	wait := requested
	if wait == 0 {
		wait = fallback
		if advertised, err := time.ParseDuration(target.ReplyTimeout); err == nil && advertised > 0 {
			wait = advertised
		} else {
			// Legacy servers do not advertise their fixed 3-minute deadline. A
			// shorter client-side AMP_BRIDGE_TIMEOUT must not abandon the socket
			// first and lose the request id needed to diagnose the timeout.
			wait = max(wait, 3*time.Minute)
		}
	}
	if maximum, err := time.ParseDuration(target.MaxReplyTimeout); err == nil && maximum > 0 && wait > maximum {
		wait = maximum
	}
	return wait
}

func supportsRequestedTimeout(target registryEntry) bool {
	defaultWait, defaultErr := time.ParseDuration(target.ReplyTimeout)
	maxWait, maxErr := time.ParseDuration(target.MaxReplyTimeout)
	return defaultErr == nil && defaultWait > 0 && maxErr == nil && maxWait >= defaultWait
}

func exchange(target registryEntry, req ampRequest, wait time.Duration) (ampResponse, error) {
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "unix", target.Socket)
	if err != nil {
		return ampResponse{}, fmt.Errorf("cannot reach %s at %s: %w", target.Name, target.Socket, err)
	}
	defer func() { _ = conn.Close() }()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return ampResponse{}, fmt.Errorf("send failed: %w", err)
	}
	// Slightly past the server's own deadline, so a server-side timeout is
	// reported as such rather than as a client read failure.
	if err := conn.SetReadDeadline(time.Now().Add(wait + 10*time.Second)); err != nil {
		return ampResponse{}, fmt.Errorf("set deadline: %w", err)
	}

	var resp ampResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return ampResponse{}, fmt.Errorf("no reply: %w", err)
	}
	return resp, nil
}
