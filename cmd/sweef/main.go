// Command sweef is the single static binary: browser engine, MCP server,
// SDK socket server, CDP endpoint and CLI — all in one ~4-8 MB executable.
//
//	sweef mcp                     speak Model Context Protocol on stdio
//	sweef serve --addr 127.0.0.1:9222   TCP RPC for Node/Python SDKs
//	sweef fetch <url> [--text|--html]   one-shot page grab (agent-friendly)
//	sweef call <Method> ['{"json"}']    raw RPC over a running server
//	sweef snapshot <url>            aria tree of a URL, JSON to stdout
//	sweef eval <url> <expr>         run JS in a throwaway tab
//	sweef pushd [--addr ...]        self-hostable Web Push relay (docs/push.md)
//	sweef version / stats
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/sweefbrowser/sweef/internal/agent"
	"github.com/sweefbrowser/sweef/internal/cliclient"
	"os"
	"os/signal"
	"runtime/pprof"
	"strings"
	"syscall"

	"github.com/sweefbrowser/sweef/internal/engine"
	"github.com/sweefbrowser/sweef/internal/mcp"
	"github.com/sweefbrowser/sweef/internal/rpc"
)

var (
	version = "0.1.0"
	commit  = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := cliclient.LeadingGlobalFlags(os.Args[1:])

	if cliclient.IsSessionCommand(cmd, args) {
		cliclient.Run(cmd, args)
		return
	}
	switch cmd {
	case "mcp":
		cmdMCP(args)
	case "serve":
		cmdServe(args)
	case "fetch":
		cmdFetch(args)
	case "snapshot":
		cmdSnapshot(args)
	case "shot":
		cmdShot(args)
	case "eval":
		cmdEval(args)
	case "call":
		cmdCall(args)
	case "agent":
		os.Exit(agent.RunCLI(args, os.Stdin, os.Stdout))
	case "gui":
		cmdGUI(args)
	case "pushd":
		cmdPushd(args)
	case "version", "--version":
		fmt.Println("sweef", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sweef - the agent browser that costs nothing to run

session commands (a daemon keeps the page alive between calls; refs come from snapshot):
  sweef open <url>                       navigate (starts the session daemon on first use)
  sweef snapshot [-i] [-c] [-d n] [-s sel] [--delta]
  sweef click @e3 | fill @e5 <text> | type | press <key> | hover | focus | check | uncheck | select
  sweef get text|html|value|attr|title|url|count [sel] [name]
  sweef is visible|enabled|checked <sel>
  sweef wait <sel|ms> | --text t | --url glob | --fn js | --load state
  sweef eval <js> | console | network | back | forward | reload | tab | screenshot [path] [--full] [--no-wait]
  sweef find role button click --name "Sign in" | set viewport 390 844 | cookies | storage | state save f
  sweef batch "open <url>" "fill #q hi" "click @e2" [--observe interactive|full|delta|text|none] [--continue]
  sweef pdf [path] | downloads | download <url> | route <glob> --json v|--abort | unroute | bind | bindings | dialog accept
  sweef rpc <Method> [params-json]       any RPC method against the session (Page.frames, Emulation.*, ...)
  sweef close                            stop the session daemon
  global flags: --session <name> --json --timeout 30s

one-shot and server commands:
  sweef fetch <url> [--html]             print page text (default) or HTML
  sweef snapshot <url> [--aria]          accessibility tree of a URL
  sweef eval <url> <js-expression>       evaluate JS after load
  sweef shot <url> [--out page.png] [--wait 2s]   PNG render, taken once the page stopped changing (--wait 0 = at once)
  sweef mcp                              MCP server over stdio (for AI agents)
  sweef serve [--addr host:port|unix:path] [--idle-timeout 1h]
  sweef call <Method> [params-json]      raw RPC against a running server (--addr)
  sweef agent [-url u] [-json] "<task>"  run the built-in LLM agent on a task (needs ANTHROPIC_API_KEY)
  sweef gui [--headless --script f.json --out dir --size WxH --profile dir --incognito --stats] [url...]
  sweef version
`)
}

func withSignals(ctx context.Context) (context.Context, context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	c, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-c.Done():
		}
	}()
	return c, cancel
}

func cmdMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	addCommonFlags(fs)
	_ = fs.Parse(args)
	if path := os.Getenv("SWEEF_CPUPROFILE"); path != "" { // development aid: profile the daemon until it exits
		if f, err := os.Create(path); err == nil {
			if pprof.StartCPUProfile(f) == nil {
				defer pprof.StopCPUProfile()
			}
		}
	}
	o := engineOptsFrom(fs)
	o.WarmVMs = 2 // long-lived agent server: keep two JS VMs ready (engine vmPool)
	engine.SetProcessMemoryBudget(o.MaxRSSMiB)
	e := engine.New(o)
	defer e.Close()
	srv := mcp.New(e, os.Stdout)
	ctx, cancel := withSignals(context.Background())
	defer cancel()
	if err := srv.Serve(ctx, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		os.Exit(1)
	}
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9222", "listen address (host:port or unix:/path)")
	idle := fs.Duration("idle-timeout", 0, "exit after this long without a request (0 = never)")
	addCommonFlags(fs)
	_ = fs.Parse(args)
	if path := os.Getenv("SWEEF_CPUPROFILE"); path != "" { // development aid: profile the daemon until it exits
		if f, err := os.Create(path); err == nil {
			if pprof.StartCPUProfile(f) == nil {
				defer pprof.StopCPUProfile()
			}
		}
	}
	o := engineOptsFrom(fs)
	o.WarmVMs = 2 // long-lived agent server: keep two JS VMs ready (engine vmPool)
	engine.SetProcessMemoryBudget(o.MaxRSSMiB)
	e := engine.New(o)
	defer e.Close()
	srv := rpc.NewServer(e)
	srv.IdleTimeout = *idle
	srv.BackgroundPump = true // pages finish what they started between calls (rpc.Server.BackgroundPump)
	ctx, cancel := withSignals(context.Background())
	defer cancel()
	srv.OnShutdown = cancel
	fmt.Fprintf(os.Stderr, "sweef serving %s (SDK protocol SWF1)\n", *addr)
	if err := srv.ServeTCP(ctx, *addr); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func cmdFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	addCommonFlags(fs)
	html := fs.Bool("html", false, "print raw HTML instead of text")
	width := fs.Int("width", 0, "if >0, print structured render at this width")
	rest := parseSkipURL(fs, args)
	if len(rest) == 0 {
		usage()
		os.Exit(2)
	}
	e := engine.New(engineOptsFrom(fs))
	defer e.Close()
	t, err := e.NewTab(rest[0])
	if err != nil {
		fail(err)
	}
	switch {
	case *html:
		fmt.Println(t.HTML(true))
	case *width > 0:
		fmt.Println(t.Screenshot(*width))
	default:
		txt, err := t.Text("body")
		if err != nil {
			fail(err)
		}
		fmt.Println(txt)
	}
}

func cmdSnapshot(args []string) {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	addCommonFlags(fs)
	aria := fs.Bool("aria", false, "Playwright-style YAML accessibility tree with [ref=eN] handles")
	rest := parseSkipURL(fs, args)
	if len(rest) == 0 {
		usage()
		os.Exit(2)
	}
	e := engine.New(engineOptsFrom(fs))
	defer e.Close()
	t, err := e.NewTab(rest[0])
	if err != nil {
		fail(err)
	}
	if *aria {
		y, err := t.AriaSnapshotYAML()
		if err != nil {
			fail(err)
		}
		fmt.Print(y)
		return
	}
	nodes, err := t.Snapshot()
	if err != nil {
		fail(err)
	}
	b, _ := json.MarshalIndent(nodes, "", " ")
	fmt.Println(string(b))
}

// cmdShot writes a real PNG screenshot of a URL to -out (default <url-host>.png),
// with Playwright-like --width/--height/--dsf flags.
func cmdShot(args []string) {
	fs := flag.NewFlagSet("shot", flag.ExitOnError)
	addCommonFlags(fs)
	width := fs.Int("width", 0, "viewport width (0 = context default)")
	height := fs.Int("height", 0, "viewport height (0 = context default)")
	dsf := fs.Float64("dsf", 1, "device scale factor")
	out := fs.String("out", "", "output path (default page.png)")
	wait := fs.Duration("wait", engine.ShotStableTimeout, "longest wait for the page to stop changing before the shot, so script animations finish (0 = shoot at once)")
	rest := parseSkipURL(fs, args)
	if len(rest) == 0 {
		usage()
		os.Exit(2)
	}
	e := engine.New(engineOptsFrom(fs))
	defer e.Close()
	t, err := e.NewTab(rest[0])
	if err != nil {
		fail(err)
	}
	if !t.SettleVisual(*wait, 0) {
		fmt.Fprintf(os.Stderr, "note: the page was still changing after %v; shooting anyway\n", *wait)
	}
	pb := t.PNGScreenshot(*width, *height, *dsf)
	path := *out
	if path == "" {
		path = "page.png"
	}
	if err := os.WriteFile(path, pb, 0o644); err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", path, len(pb))
}

func cmdEval(args []string) {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	addCommonFlags(fs)
	rest := parseSkipURL(fs, args)
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sweef eval <url> <expression>")
		os.Exit(2)
	}
	e := engine.New(engineOptsFrom(fs))
	defer e.Close()
	t, err := e.NewTab(rest[0])
	if err != nil {
		fail(err)
	}
	res, err := t.Evaluate(strings.Join(rest[1:], " "))
	if err != nil {
		fail(err)
	}
	fmt.Println(string(res))
}

// cmdCall connects to a running `sweef serve` and issues one method.
func cmdCall(args []string) {
	fs := flag.NewFlagSet("call", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9222", "server address")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: sweef call <Method> [params-json]")
		os.Exit(2)
	}
	params := "{}"
	if len(rest) > 1 {
		params = rest[1]
	}
	if err := callRemote(*addr, rest[0], params); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
