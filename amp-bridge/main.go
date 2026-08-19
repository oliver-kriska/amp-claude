// amp-bridge — a Claude Code Channel server that bridges an Amp thread to a
// Claude Code session.
//
// Data flow:
//
//	Amp ──unix socket──▶ amp-bridge ──notifications/claude/channel──▶ Claude
//	Amp ◀──unix socket── amp-bridge ◀────── reply tool call ───────── Claude
//
// Usage:
//
//	amp-bridge                             run as an MCP stdio server (Claude spawns this)
//	amp-bridge --list                      list live bridges, one per Claude session
//	amp-bridge --ask "text"                send to the only live bridge
//	amp-bridge --session N --ask "text"    send to a named session
//	amp-bridge --thread T --ask "text"     let Claude reply into Amp thread T
//
// Three things in this server look like bugs and are load-bearing; each is
// documented where it lives. See mcp.go (server/discover, the experimental
// capability key) and channel.go (`meta`, not `_meta`).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

type runMode int

const (
	modeServe runMode = iota
	modeList
	modeAsk
	modeHelp
)

type options struct {
	mode    runMode
	session string
	thread  string
	text    string
}

const usage = `amp-bridge — bridge an Amp thread to a Claude Code session

  amp-bridge                            run as an MCP stdio server (Claude spawns this)
  amp-bridge --list                     list live bridges
  amp-bridge [--session N] [--thread T] --ask "text"

Environment:
  AMP_BRIDGE_MAX_INFLIGHT   unanswered events allowed        (default 8)
  AMP_BRIDGE_MAX_BYTES      max bytes per message            (default 65536)
  AMP_BRIDGE_TIMEOUT        how long a caller waits          (default 3m0s)
  AMP_BRIDGE_AMP_TIMEOUT    how long ask_amp waits           (default 5m0s)
  AMP_BRIDGE_LOG            log file (default ~/.local/state/amp-bridge/amp-bridge.log)
  AMP_BRIDGE_LOG_BODIES     1 to log frame bodies (contains conversation text)
  AMP_BRIDGE_DIR            runtime/registry directory
  AMP_BRIDGE_SOCKET         explicit socket path
  AMP_BRIDGE_DISABLE_OUTBOUND  1 to disable the ask_amp tool
  AMP_BIN                   Amp CLI to invoke                (default "amp")`

// parseArgs is strict: an unrecognised flag is an error rather than a silent
// no-op, because a typo'd flag otherwise looks like the feature simply failing.
func parseArgs(args []string) (options, error) {
	o := options{mode: modeServe}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--list":
			o.mode = modeList
		case "--help", "-h":
			o.mode = modeHelp
			return o, nil
		case "--session", "--thread":
			if i+1 >= len(args) {
				return o, fmt.Errorf("%s needs a value", args[i])
			}
			if args[i] == "--session" {
				o.session = args[i+1]
			} else {
				o.thread = args[i+1]
			}
			i++
		case "--ask":
			// Everything after --ask is the message, so it need not be quoted.
			o.mode = modeAsk
			o.text = strings.TrimSpace(strings.Join(args[i+1:], " "))
			if o.text == "" {
				return o, errors.New("--ask needs a message")
			}
			return o, nil
		default:
			return o, fmt.Errorf("unknown argument %q (try --help)", args[i])
		}
	}
	return o, nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "amp-bridge: %v\n", err)
		return 2
	}
	cfg := loadConfig()

	switch opts.mode {
	case modeHelp:
		fmt.Println(usage)
		return 0
	case modeList:
		return cmdList()
	case modeAsk:
		return cmdAsk(cfg, opts.session, opts.thread, opts.text)
	case modeServe:
		return runServer(cfg)
	default:
		return 2
	}
}

// defaultLogPath resolves where the server logs. It deliberately does NOT sit
// beside the binary: once installed to ~/.local/bin that would drop a log file
// into a directory meant to hold executables.
func defaultLogPath() string {
	if v := os.Getenv("AMP_BRIDGE_LOG"); v != "" {
		return v
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "amp-bridge.log" // last resort: the working directory
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, serverName, serverName+".log")
}

// openLog opens the append-only log at 0600: frames can carry conversation
// content when AMP_BRIDGE_LOG_BODIES=1.
func openLog(path string) (*os.File, error) {
	// 0700: the directory may hold conversation text when bodies are logged.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	// #nosec G304 -- the log path is operator-configured (AMP_BRIDGE_LOG) and
	// otherwise derived from the user's own state directory.
	lf, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// Tighten a pre-existing looser log rather than inheriting its mode.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lf.Close()
		return nil, err
	}
	return lf, nil
}

func runServer(cfg config) int {
	logPath := defaultLogPath()
	lf, err := openLog(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "amp-bridge: cannot open log %s: %v\n", logPath, err)
		return 1
	}
	defer func() { _ = lf.Close() }()

	b := newBridge(cfg, os.Stdout, lf)

	name, sock, entry := resolveIdentity()
	if _, err := ensureRuntimeDir(); err != nil {
		b.logf("RUNTIME_DIR_FAILED %v", err)
		fmt.Fprintf(os.Stderr, "amp-bridge: %v\n", err)
		return 1
	}
	ln, err := bindSocket(sock)
	if err != nil {
		b.logf("BIND_FAILED %v", err)
		fmt.Fprintf(os.Stderr, "amp-bridge: %v\n", err)
		return 1
	}
	regPath, err := entry.publish()
	if err != nil {
		// Not fatal: the bridge still works, it is just undiscoverable.
		b.logf("REGISTRY_PUBLISH_FAILED %v", err)
	}

	b.logf("=== %s %s started name=%s pid=%d claude_pid=%d socket=%s bodies=%v ===",
		serverName, serverVersion, name, os.Getpid(), entry.ClaudePID, sock, cfg.logBodies)

	b.setListener(ln)

	cleanup := sync.OnceFunc(func() {
		// Mark the shutdown first, so the supervisor reads the listener closing
		// as intent rather than as a fault to restart.
		b.beginShutdown()
		if l := b.listener(); l != nil {
			_ = l.Close()
		}
		_ = os.Remove(sock)
		if regPath != "" {
			_ = os.Remove(regPath)
		}
	})
	defer cleanup()
	go b.superviseSocket(sock)

	// Without this, SIGTERM skips the deferred cleanup and leaves a stale socket
	// and registry entry behind. `--list` sweeps them eventually, but the window
	// is confusing while it lasts.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		s := <-sigs
		b.logf("=== signal %v — cleaning up and exiting ===", s)
		cleanup()
		os.Exit(0)
	}()

	// Two ways to finish: Claude closes stdin (normal), or socket supervision
	// gives up (permanent fault — exit non-zero so it is not mistaken for one).
	done := make(chan struct{})
	go func() {
		b.serveStdio(os.Stdin)
		close(done)
	}()

	select {
	case <-done:
		return 0
	case <-b.fatalSignal():
		fmt.Fprintln(os.Stderr,
			"amp-bridge: socket supervision exhausted its restart budget; exiting")
		return 1
	}
}

// serveStdio reads newline-delimited JSON-RPC until the transport closes.
func (b *bridge) serveStdio(in io.Reader) {
	rd := bufio.NewReaderSize(in, 1<<20)
	for {
		line, rerr := rd.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			var msg rpc
			if jerr := json.Unmarshal([]byte(s), &msg); jerr != nil {
				b.logf("PARSE_ERROR %v", jerr)
			} else {
				b.logFrame(">>RECV", msg.Method, msg.ID, []byte(s))
				b.handle(msg)
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				b.logf("STDIN_ERROR %v", rerr)
			}
			break
		}
	}
	b.logf("=== stdin closed, exiting ===")
}
