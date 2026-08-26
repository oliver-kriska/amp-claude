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
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

type runMode int

const (
	modeServe runMode = iota
	modeList
	modeAsk
	modeHelp
	modeVersion
	modeDoctor
	modeInit
)

type options struct {
	mode    runMode
	session string
	thread  string
	text    string
	dir     string
	strict  bool
	verbose bool
	global  bool
	// ampPlugin installs the Amp-side half; force overwrites a divergent copy.
	ampPlugin bool
	force     bool
}

const usage = `amp-bridge — bridge an Amp thread to a Claude Code session

  amp-bridge                            run as an MCP stdio server (Claude spawns this)
  amp-bridge init [dir]                 write .mcp.json pointing at this binary
  amp-bridge init --global              register for every project, plus the skill
  amp-bridge init --amp-plugin [dir]    install the Amp plugin into one project, so
                                        Claude can reach a thread that is open
  amp-bridge init --amp-plugin --global install it for every Amp session
                                        (~/.config/amp/plugins), still off until
                                        enabled per thread
  amp-bridge doctor [dir] [--strict]    diagnose why the channel is not working
  amp-bridge --list [--verbose]         list live bridges; include build and handshake details
  amp-bridge --version                  print the version and build fingerprint
  amp-bridge [--session N] [--thread T] --ask "text"

Environment:
  AMP_BRIDGE_MAX_INFLIGHT   inbound and async caps           (default 8 each)
  AMP_BRIDGE_MAX_BYTES      max bytes per message            (default 65536)
  AMP_BRIDGE_TIMEOUT        how long a caller waits          (default 3m0s)
  AMP_BRIDGE_AMP_TIMEOUT    how long ask_amp/send_amp waits  (default 2m0s)
  AMP_BRIDGE_LOG            log file (default ~/.local/state/amp-bridge/amp-bridge.log)
  AMP_BRIDGE_LOG_BODIES     1 to log frame bodies (contains conversation text)
  AMP_BRIDGE_DIR            runtime/registry directory
  AMP_BRIDGE_SOCKET         explicit socket path
  AMP_BRIDGE_DISABLE_OUTBOUND  1 to disable Claude→Amp tools
  AMP_BIN                   Amp CLI to invoke                (default "amp")`

// parseArgs is strict: an unrecognised flag is an error rather than a silent
// no-op, because a typo'd flag otherwise looks like the feature simply failing.
func parseArgs(args []string) (options, error) {
	o := options{mode: modeServe, dir: "."}

	// Subcommands come first and take an optional directory.
	if sub, ok := parseSubcommand(args); ok {
		return sub, subcommandError(args)
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--list":
			o.mode = modeList
		case "--verbose":
			o.verbose = true
		case "--help", "-h":
			o.mode = modeHelp
			return o, nil
		case "--version":
			o.mode = modeVersion
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
			if o.verbose {
				return o, errors.New("--verbose is only valid with --list")
			}
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
	if o.verbose && o.mode != modeList {
		return o, errors.New("--verbose is only valid with --list")
	}
	return o, nil
}

// parseSubcommand recognises the verb form (`init`, `doctor`), which takes an
// optional directory rather than flags.
func parseSubcommand(args []string) (options, bool) {
	if len(args) == 0 {
		return options{}, false
	}
	o := options{dir: "."}
	switch args[0] {
	case "init":
		o.mode = modeInit
	case "doctor":
		o.mode = modeDoctor
	default:
		return options{}, false
	}
	for _, a := range args[1:] {
		switch a {
		case "--strict":
			o.strict = true
		case "--global":
			o.global = true
		case "--amp-plugin":
			o.ampPlugin = true
		case "--force":
			o.force = true
		default:
			o.dir = a
		}
	}
	return o, true
}

// subcommandError rejects a shape it does not understand rather than silently
// using the last directory it saw: a typo'd flag that looks like a no-op is the
// failure mode this whole binary keeps designing against.
func subcommandError(args []string) error {
	dirs := 0
	for _, a := range args[1:] {
		switch {
		case a == "--strict" && args[0] == "doctor":
			continue
		case a == "--global" && args[0] == "init":
			continue
		case a == "--amp-plugin" && args[0] == "init":
			continue
		case a == "--force" && args[0] == "init":
			continue
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("%s does not take %s", args[0], a)
		default:
			dirs++
		}
	}
	if dirs > 1 {
		return fmt.Errorf("%s takes at most one directory", args[0])
	}
	// A directory with --global is a contradiction. Ignoring it is how a user
	// ends up believing they installed something into a path that was never touched.
	if dirs > 0 && args[0] == "init" && slices.Contains(args[1:], "--global") {
		return errors.New("init --global installs for every project, so it takes no directory")
	}
	return nil
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
	case modeVersion:
		// Releases stamp this via -ldflags; a source build reports "dev". The
		// build fingerprint is what `doctor` compares sessions against, so
		// printing both means one command answers "which release" and "which
		// binary" together.
		fmt.Printf("amp-bridge %s (build %s)\n", serverVersion, buildFingerprint())
		return 0
	case modeList:
		return cmdList(opts.verbose)
	case modeAsk:
		if opts.thread != "" && !canonicalThreadIDRE.MatchString(opts.thread) {
			fmt.Fprintf(
				os.Stderr,
				"amp-bridge: --thread needs a complete Amp thread id like T-01a01877-2274-734d-8306-7c37b33f2a7f; got %q\n",
				opts.thread,
			)
			return 2
		}
		return cmdAsk(cfg, opts.session, opts.thread, opts.text)
	case modeInit:
		// --amp-plugin names the artefact, --global names the scope, so the
		// artefact is selected first. Checking global first meant that
		// `init --amp-plugin --global` silently registered the Claude MCP server
		// and installed no plugin at all — a typed flag being dropped without a
		// word, which is the exact failure class this binary keeps designing out.
		if opts.ampPlugin {
			return cmdInitAmpPlugin(opts.dir, opts.global, opts.force)
		}
		if opts.global {
			return cmdInitGlobal()
		}
		return cmdInit(opts.dir)
	case modeDoctor:
		return cmdDoctor(cfg, opts.dir, opts.strict)
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
	// Fatal. Without a registration no client can discover us: `--list` shows
	// nothing and `--ask` cannot find a target, so the bridge is running,
	// healthy and unreachable — the exact silent failure this codebase keeps
	// having to design against.
	regPath, err := entry.publish()
	if err != nil {
		b.logf("REGISTRY_PUBLISH_FAILED %v", err)
		fmt.Fprintf(os.Stderr, "amp-bridge: cannot publish registry entry: %v\n", err)
		return 1
	}
	b.regMu.Lock()
	b.reg, b.regPath = entry, regPath
	b.regMu.Unlock()

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

	code := 0
	select {
	case <-done:
	case <-b.fatalSignal():
		fmt.Fprintln(os.Stderr,
			"amp-bridge: socket supervision exhausted its restart budget; exiting")
		code = 1
	}

	b.drainToolWork()
	return code
}

// drainToolWork cancels in-flight tool calls and waits briefly for them.
//
// Tool calls run off the read loop, so an ask_amp or send_amp subprocess may
// still be mid-turn when Claude closes stdin. Without this, `amp threads
// continue` outlives the session as an orphan.
func (b *bridge) drainToolWork() {
	b.beginShutdown()

	drained := make(chan struct{})
	go func() {
		b.toolWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		b.logf("SHUTDOWN_TIMEOUT in-flight tool work did not unwind in time")
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
