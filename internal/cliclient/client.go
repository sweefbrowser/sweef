package cliclient

// agentcli.go — the session CLI agents drive one command at a time:
//
//	sweef open https://example.com
//	sweef snapshot -i
//	sweef click @e3
//	sweef fill @e5 "hello"
//	sweef get text @e1
//
// The first command starts a daemon (`sweef serve` on a unix socket) that
// keeps the engine, tabs and cookies alive between calls; every later call
// is one framed JSON-RPC round trip, a few milliseconds end to end. Refs
// come from `snapshot` and are accepted as @e3, e3 or ref=e3.

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sweefbrowser/sweef/internal/batchfmt"
	"github.com/sweefbrowser/sweef/internal/protocol"
)

var sessionCommands = map[string]bool{
	"open": true, "goto": true, "navigate": true, "snapshot": true, "click": true, "dblclick": true, "fill": true,
	"type": true, "press": true, "key": true, "hover": true, "focus": true, "check": true, "uncheck": true,
	"select": true, "scroll": true, "scrollintoview": true, "get": true, "is": true, "wait": true, "eval": true,
	"console": true, "network": true, "back": true, "forward": true, "reload": true, "tab": true,
	"screenshot": true, "close": true, "quit": true, "exit": true, "stats": true, "set": true, "find": true,
	"storage": true, "cookies": true, "state": true, "drag": true, "dialog": true,
	"batch": true, "pdf": true, "downloads": true, "download": true, "route": true, "unroute": true,
	"bind": true, "bindings": true, "rpc": true,
}

// isSessionCommand decides between the one-shot commands (snapshot <url>,
// eval <url> <expr>) and the daemon-backed session verbs.
// IsSessionCommand reports whether cmd (with its arguments) is a session command: one the daemon serves.
func IsSessionCommand(cmd string, args []string) bool { return isSessionCommand(cmd, args) }

// Run runs a session command against the session daemon (starting it when needed) and exits non-zero on
// failure.
func Run(cmd string, args []string) { cmdSession(cmd, args) }

// DaemonExe is the executable started as the session daemon ("<exe> serve ..."): the running binary in
// sweef itself; the engine-free client (cmd/sweef-cli) points it at the sweef binary next to it.
var DaemonExe = os.Executable

// LeadingGlobalFlags lets the session CLI's global flags come before the verb
// (`sweef --json get url`, `sweef --session w open ...`): the verb is the first
// argument that is not one of them, and the flags move after it.
func LeadingGlobalFlags(argv []string) (string, []string) {
	var flags []string
	i := 0
	for i < len(argv) {
		a := argv[i]
		switch {
		case a == "--json" || strings.HasPrefix(a, "--session=") || strings.HasPrefix(a, "--timeout="):
			flags = append(flags, a)
			i++
		case (a == "--session" || a == "--timeout") && i+1 < len(argv):
			flags = append(flags, a, argv[i+1])
			i += 2
		default:
			return a, append(append([]string{}, argv[i+1:]...), flags...)
		}
	}
	return "help", nil
}

func isSessionCommand(cmd string, args []string) bool {
	if !sessionCommands[cmd] {
		return false
	}
	if (cmd == "snapshot" || cmd == "eval") && len(args) > 0 && looksLikeURL(args[0]) {
		return false
	}
	return true
}

func looksLikeURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "file://")
}

type cliSession struct {
	name    string
	sock    string
	jsonOut bool
	timeout time.Duration
}

func sessionDir() string {
	if d := os.Getenv("SWEEF_HOME"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "sweef")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("sweef-%d", os.Getuid()))
}

// splitGlobalFlags pulls --session/--json/--timeout out of args wherever
// they appear so `sweef click @e1 --json` and `sweef --json click @e1` both work.
func splitGlobalFlags(args []string) (*cliSession, []string) {
	s := &cliSession{name: os.Getenv("SWEEF_SESSION"), timeout: 30 * time.Second}
	if s.name == "" {
		s.name = "default"
	}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			s.jsonOut = true
		case a == "--session" && i+1 < len(args):
			s.name = args[i+1]
			i++
		case strings.HasPrefix(a, "--session="):
			s.name = strings.TrimPrefix(a, "--session=")
		case a == "--timeout" && i+1 < len(args):
			if d, err := time.ParseDuration(args[i+1]); err == nil {
				s.timeout = d
			} else if ms, err := strconv.Atoi(args[i+1]); err == nil {
				s.timeout = time.Duration(ms) * time.Millisecond
			}
			i++
		default:
			rest = append(rest, a)
		}
	}
	s.sock = filepath.Join(sessionDir(), s.name+".sock")
	return s, rest
}

// ---- daemon lifecycle --------------------------------------------------------

func (s *cliSession) dial() (net.Conn, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	return d.Dial("unix", s.sock)
}

// ensureDaemon connects to the session daemon, starting it when absent.
func (s *cliSession) ensureDaemon() error {
	if c, err := s.dial(); err == nil {
		c.Close()
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.sock), 0o700); err != nil {
		return err
	}
	os.Remove(s.sock)
	self, err := DaemonExe()
	if err != nil {
		return err
	}
	logf, _ := os.OpenFile(strings.TrimSuffix(s.sock, ".sock")+".log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	args := []string{"serve", "--addr", "unix:" + s.sock, "--idle-timeout", envOr("SWEEF_IDLE_TIMEOUT", "1h")}
	if ua := os.Getenv("SWEEF_USER_AGENT"); ua != "" {
		args = append(args, "--user-agent", ua)
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	go cmd.Wait() //nolint — detached
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := s.dial(); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not start; see %s.log", strings.TrimSuffix(s.sock, ".sock"))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// call performs one RPC against the session daemon.
func (s *cliSession) call(method string, params map[string]any) (json.RawMessage, error) {
	c, err := s.dial() // fast path: the daemon is usually running
	if err != nil {
		if err := s.ensureDaemon(); err != nil {
			return nil, err
		}
		if c, err = s.dial(); err != nil {
			return nil, err
		}
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(s.timeout + 5*time.Second))
	conn := protocol.NewConn(c)
	raw, _ := json.Marshal(params)
	if err := conn.Write(&protocol.Message{JSONRPC: "2.0", ID: 1, Method: method, Params: raw}); err != nil {
		return nil, err
	}
	for {
		m, _, err := conn.Read(make([]byte, 4096))
		if err != nil {
			return nil, err
		}
		if m.ID != 1 {
			continue
		}
		if m.Error != nil {
			return nil, fmt.Errorf("%s", m.Error.Message)
		}
		return m.Result, nil
	}
}

// ---- command dispatch ----------------------------------------------------------

var errBatchFailed = fmt.Errorf("batch failed")

func cmdSession(cmd string, rawArgs []string) {
	s, args := splitGlobalFlags(rawArgs)
	out, err := s.run(cmd, args)
	if err == errBatchFailed {
		os.Exit(1)
	}
	if err != nil {
		if s.jsonOut {
			b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
			fmt.Println(string(b))
		} else {
			fmt.Fprintln(os.Stderr, "✗", err)
		}
		os.Exit(1)
	}
	if out != "" {
		fmt.Print(out)
		if !strings.HasSuffix(out, "\n") {
			fmt.Println()
		}
	}
}

func normSel(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "@e") {
		return s[1:]
	}
	return s
}

func need(args []string, n int, usage string) error {
	if len(args) < n {
		return fmt.Errorf("usage: sweef %s", usage)
	}
	return nil
}

// field pulls a key out of a JSON object result.
func field(raw json.RawMessage, key string) any {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m[key]
}

func (s *cliSession) ok(raw json.RawMessage, human string) string {
	if s.jsonOut {
		if len(raw) == 0 {
			raw = json.RawMessage(`{"ok":true}`)
		}
		return string(raw)
	}
	return human
}

func (s *cliSession) run(cmd string, args []string) (string, error) {
	switch cmd {
	case "open", "goto", "navigate":
		if len(args) == 0 {
			r, err := s.call("Browser.newTab", map[string]any{"url": "about:blank"})
			return s.ok(r, "✓ browser ready"), err
		}
		u := args[0]
		// a bare host gets https://; data:, about:, blob: URLs and paths of the current page go as they are
		if !looksLikeURL(u) && !strings.HasPrefix(u, "about:") && !strings.HasPrefix(u, "data:") && !strings.HasPrefix(u, "blob:") &&
			!strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "?") {
			u = "https://" + u
		}
		r, err := s.call("Page.navigate", map[string]any{"url": u})
		if err != nil {
			return "", err
		}
		return s.ok(r, fmt.Sprintf("✓ %v\n  %v", field(r, "title"), field(r, "url"))), nil

	case "snapshot":
		return s.snapshot(args)

	case "batch":
		return s.batch(args)

	case "pdf":
		path := "page.pdf"
		p := map[string]any{}
		for i := 0; i < len(args); i++ {
			switch a := args[i]; {
			case a == "--landscape":
				p["landscape"] = true
			case a == "--format" && i+1 < len(args):
				p["format"] = args[i+1]
				i++
			case a == "--no-header":
				p["displayHeaderFooter"] = false
			default:
				path = a
			}
		}
		abs, _ := filepath.Abs(path)
		p["path"] = abs
		r, err := s.call("Page.pdf", p)
		if err != nil {
			return "", err
		}
		return s.ok(r, fmt.Sprintf("✓ %s (%v bytes)", path, field(r, "bytes"))), nil

	case "downloads":
		r, err := s.call("Page.downloads", nil)
		if err != nil {
			return "", err
		}
		if s.jsonOut {
			return string(r), nil
		}
		var out struct {
			Downloads []struct {
				URL, Path, Error string
				Size             int64
			} `json:"downloads"`
		}
		json.Unmarshal(r, &out)
		if len(out.Downloads) == 0 {
			return "(no downloads)", nil
		}
		var sb strings.Builder
		for _, d := range out.Downloads {
			sb.WriteString(fmt.Sprintf("%s  %d bytes  %s", d.Path, d.Size, d.URL))
			if d.Error != "" {
				sb.WriteString("  error: " + d.Error)
			}
			sb.WriteString("\n")
		}
		return sb.String(), nil

	case "download":
		if err := need(args, 1, "download <url> [filename]"); err != nil {
			return "", err
		}
		p := map[string]any{"url": args[0]}
		if len(args) > 1 {
			p["filename"] = args[1]
		}
		r, err := s.call("Page.download", p)
		if err != nil {
			return "", err
		}
		return s.ok(r, fmt.Sprintf("✓ %v (%v bytes)", field(r, "path"), field(r, "size"))), nil

	case "route":
		// sweef route "**/api/*" --json '{"ok":true}' | --body text --status 404 | --abort | --continue --url u --header k:v --method POST --times 1
		if err := need(args, 1, "route <pattern> [--json v | --body t] [--status n] [--type ct] [--abort | --continue] [--url u] [--header k:v] [--method M] [--times n]"); err != nil {
			return "", err
		}
		p := map[string]any{"pattern": args[0], "action": "fulfill"}
		hdr := map[string]any{}
		for i := 1; i < len(args); i++ {
			a := args[i]
			val := func() string {
				if i+1 < len(args) {
					i++
					return args[i]
				}
				return ""
			}
			switch a {
			case "--abort":
				p["action"] = "abort"
			case "--continue":
				p["action"] = "continue"
			case "--json":
				var v any
				if err := json.Unmarshal([]byte(val()), &v); err != nil {
					return "", fmt.Errorf("--json: %w", err)
				}
				p["json"] = v
			case "--body":
				p["body"] = val()
			case "--status":
				n, _ := strconv.Atoi(val())
				p["status"] = n
			case "--type":
				p["contentType"] = val()
			case "--url":
				p["url"] = val()
			case "--method":
				p["method"] = val()
			case "--new-method":
				p["newMethod"] = val()
			case "--new-body":
				p["newBody"] = val()
			case "--body-contains":
				p["bodyContains"] = val()
			case "--times":
				n, _ := strconv.Atoi(val())
				p["times"] = n
			case "--header":
				kv := strings.SplitN(val(), ":", 2)
				if len(kv) == 2 {
					hdr[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
				}
			}
		}
		if len(hdr) > 0 {
			p["headers"] = hdr
		}
		r, err := s.call("Network.route", p)
		return s.ok(r, fmt.Sprintf("✓ route %s → %v", args[0], p["action"])), err

	case "unroute":
		p := map[string]any{}
		if len(args) > 0 {
			p["pattern"] = args[0]
			r, err := s.call("Network.unroute", p)
			return s.ok(r, "✓ unrouted "+args[0]), err
		}
		r, err := s.call("Network.unrouteAll", p)
		return s.ok(r, "✓ all routes removed"), err

	case "bind":
		if err := need(args, 1, "bind <name> [reply-json]"); err != nil {
			return "", err
		}
		p := map[string]any{"name": args[0]}
		if len(args) > 1 {
			p["reply"] = strings.Join(args[1:], " ")
		}
		r, err := s.call("Runtime.addBinding", p)
		return s.ok(r, "✓ window."+args[0]+" exposed"), err

	case "bindings":
		r, err := s.call("Runtime.bindingCalls", map[string]any{"since": 0})
		if err != nil {
			return "", err
		}
		if s.jsonOut {
			return string(r), nil
		}
		var out struct {
			Calls []struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			} `json:"calls"`
		}
		json.Unmarshal(r, &out)
		if len(out.Calls) == 0 {
			return "(no binding calls)", nil
		}
		var sb strings.Builder
		for _, c := range out.Calls {
			sb.WriteString(c.Name + string(c.Args) + "\n")
		}
		return sb.String(), nil

	case "click", "dblclick", "hover", "focus", "check", "uncheck", "scrollintoview":
		if err := need(args, 1, cmd+" <selector>"); err != nil {
			return "", err
		}
		method := map[string]string{"click": "Input.click", "dblclick": "Input.click", "hover": "Input.hover", "focus": "Input.focus",
			"check": "Input.check", "uncheck": "Input.uncheck", "scrollintoview": "Input.scroll"}[cmd]
		p := map[string]any{"selector": normSel(args[0]), "timeoutMs": s.timeout.Milliseconds()}
		if cmd == "dblclick" {
			p["clickCount"] = 2
		}
		r, err := s.call(method, p)
		if err != nil {
			return "", err
		}
		if cmd == "click" {
			if t, err2 := s.call("Page.title", nil); err2 == nil {
				return s.ok(r, fmt.Sprintf("✓ clicked %s\n  %v", args[0], field(t, "title"))), nil
			}
		}
		return s.ok(r, "✓ "+cmd+" "+args[0]), nil

	case "fill", "type":
		if cmd == "type" && len(args) == 1 { // "sweef type <text>": into the focused element (after sweef click)
			r, err := s.call("Input.keyboard", map[string]any{"text": args[0]})
			return s.ok(r, "✓ typed"), err
		}
		if err := need(args, 2, cmd+" <selector> <text>"); err != nil {
			return "", err
		}
		text := strings.Join(args[1:], " ")
		method := "Input.fill"
		p := map[string]any{"selector": normSel(args[0]), "value": text}
		if cmd == "type" {
			method, p = "Input.type", map[string]any{"selector": normSel(args[0]), "text": text}
		}
		r, err := s.call(method, p)
		return s.ok(r, fmt.Sprintf("✓ %s %s", cmd, args[0])), err

	case "press", "key":
		if err := need(args, 1, "press <key> [selector]"); err != nil {
			return "", err
		}
		if len(args) > 1 {
			r, err := s.call("Input.press", map[string]any{"selector": normSel(args[1]), "key": args[0]})
			return s.ok(r, "✓ pressed "+args[0]), err
		}
		r, err := s.call("Input.keyboard", map[string]any{"key": args[0]})
		return s.ok(r, "✓ pressed "+args[0]), err

	case "select":
		if err := need(args, 2, "select <selector> <value>"); err != nil {
			return "", err
		}
		r, err := s.call("Input.selectOption", map[string]any{"selector": normSel(args[0]), "value": strings.Join(args[1:], " ")})
		return s.ok(r, "✓ selected "+args[1]), err

	case "scroll":
		dir, px := "down", 600
		if len(args) > 0 {
			dir = args[0]
		}
		if len(args) > 1 {
			px, _ = strconv.Atoi(args[1])
		}
		dx, dy := 0, px
		switch dir {
		case "up":
			dy = -px
		case "left":
			dx, dy = -px, 0
		case "right":
			dx, dy = px, 0
		}
		r, err := s.call("Input.scroll", map[string]any{"x": dx, "y": dy})
		return s.ok(r, fmt.Sprintf("✓ scrolled %s %d", dir, px)), err

	case "drag":
		if err := need(args, 2, "drag <source> <target>"); err != nil {
			return "", err
		}
		r, err := s.call("Input.drag", map[string]any{"source": normSel(args[0]), "target": normSel(args[1])})
		return s.ok(r, "✓ dragged"), err

	case "get":
		return s.get(args)

	case "is":
		if err := need(args, 2, "is visible|enabled|checked <selector>"); err != nil {
			return "", err
		}
		key := args[0]
		r, err := s.call("Locator.is"+strings.ToUpper(key[:1])+key[1:], map[string]any{"selector": normSel(args[1])})
		if err != nil {
			return "", err
		}
		return s.ok(r, fmt.Sprint(field(r, key))), nil

	case "wait":
		return s.wait(args)

	case "eval":
		if err := need(args, 1, "eval <javascript>"); err != nil {
			return "", err
		}
		src := strings.Join(args, " ")
		if src == "-" || src == "--stdin" {
			b, _ := readAll(os.Stdin)
			src = string(b)
		}
		r, err := s.call("Runtime.evaluate", map[string]any{"expression": src})
		if err != nil {
			return "", err
		}
		if s.jsonOut {
			return string(r), nil
		}
		res, _ := json.Marshal(field(r, "result"))
		return string(res), nil

	case "console":
		r, err := s.call("Log.getConsole", map[string]any{"since": 0})
		if err != nil {
			return "", err
		}
		if s.jsonOut {
			return string(r), nil
		}
		var sb strings.Builder
		if msgs, ok := field(r, "messages").([]any); ok {
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				data, _ := mm["data"].(map[string]any)
				if data == nil {
					data = mm
				}
				sb.WriteString(fmt.Sprintf("[%v] %v\n", data["level"], data["text"]))
			}
		}
		if sb.Len() == 0 {
			return "(no console output)", nil
		}
		return sb.String(), nil

	case "network":
		r, err := s.call("Network.getLogs", map[string]any{"since": 0})
		if err != nil {
			return "", err
		}
		if s.jsonOut {
			return string(r), nil
		}
		var sb strings.Builder
		if entries, ok := field(r, "entries").([]any); ok {
			for _, e := range entries {
				em, _ := e.(map[string]any)
				data, _ := em["data"].(map[string]any)
				if data == nil || em["kind"] == "Network.request" {
					continue // one line per answered request: the request event itself has no status yet
				}
				if st, ok := data["response.status"]; ok {
					sb.WriteString(fmt.Sprintf("%v %v %v %vms\n", data["request.method"], st, data["request.url"], data["time"]))
				}
			}
		}
		if sb.Len() == 0 {
			return "(no network activity)", nil
		}
		return sb.String(), nil

	case "back", "forward", "reload":
		m := map[string]string{"back": "Page.goBack", "forward": "Page.goForward", "reload": "Page.reload"}[cmd]
		r, err := s.call(m, nil)
		if err != nil {
			return "", err
		}
		t, _ := s.call("Page.url", nil)
		return s.ok(r, fmt.Sprintf("✓ %s\n  %v", cmd, field(t, "url"))), nil

	case "tab":
		if len(args) == 0 || args[0] == "list" {
			r, err := s.call("Browser.listTabs", nil)
			if err != nil {
				return "", err
			}
			if s.jsonOut {
				return string(r), nil
			}
			var tabs []map[string]any
			json.Unmarshal(r, &tabs)
			var sb strings.Builder
			for _, t := range tabs {
				sb.WriteString(fmt.Sprintf("%v  %v  %v\n", t["id"], t["title"], t["url"]))
			}
			return sb.String(), nil
		}
		switch args[0] {
		case "new":
			u := "about:blank"
			if len(args) > 1 {
				u = args[1]
			}
			r, err := s.call("Browser.newTab", map[string]any{"url": u})
			return s.ok(r, fmt.Sprintf("✓ %v", field(r, "tab"))), err
		case "close":
			p := map[string]any{}
			if len(args) > 1 {
				p["tab"] = args[1]
			}
			r, err := s.call("Browser.closeTab", p)
			return s.ok(r, "✓ tab closed"), err
		}
		return "", fmt.Errorf("usage: sweef tab [list|new <url>|close <id>]")

	case "screenshot":
		sp := map[string]any{}
		var rest []string
		for i := 0; i < len(args); i++ {
			switch a := args[i]; {
			case a == "--full":
				sp["fullPage"] = true
			case a == "--no-wait":
				sp["noWait"] = true
			case a == "--wait" && i+1 < len(args): // ms to wait for the page to stop changing (default 2000)
				i++
				ms, err := strconv.Atoi(args[i])
				if err != nil || ms < 0 {
					return "", fmt.Errorf("usage: sweef screenshot [path] [--full] [--no-wait] [--wait <ms>]")
				}
				sp["waitMs"] = ms
			default:
				rest = append(rest, a)
			}
		}
		args = rest
		r, err := s.call("Page.screenshot", sp)
		if err != nil {
			return "", err
		}
		path := "screenshot.png"
		if len(args) > 0 {
			path = args[0]
		}
		if b64, _ := field(r, "pngBase64").(string); b64 != "" {
			pb, _ := base64.StdEncoding.DecodeString(b64)
			if err := os.WriteFile(path, pb, 0o644); err != nil {
				return "", err
			}
		}
		return s.ok(json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)), "✓ "+path), nil

	case "stats":
		r, err := s.call("Browser.stats", nil)
		return string(r), err

	case "rpc": // any RPC method against the session: sweef rpc Page.frames '{"tab":"tab-1"}'
		if err := need(args, 1, "rpc <Method> [params-json]"); err != nil {
			return "", err
		}
		params := map[string]any{}
		if len(args) > 1 {
			if err := json.Unmarshal([]byte(args[1]), &params); err != nil {
				return "", fmt.Errorf("rpc: params must be a JSON object: %v", err)
			}
		}
		r, err := s.call(args[0], params)
		return string(r), err

	case "set":
		return s.set(args)

	case "storage", "cookies", "state", "dialog", "find":
		return s.misc(cmd, args)

	case "close", "quit", "exit":
		c, err := s.dial()
		if err != nil {
			return "✓ no session running", nil
		}
		c.Close()
		s.call("Browser.shutdown", nil)
		// wait for the daemon to be gone: one still shutting down when the next command starts a new daemon
		// removed that daemon's socket on its way out, and the command after got a fresh session ("no such tab")
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			c, err := s.dial()
			if err != nil {
				break
			}
			c.Close()
		}
		os.Remove(s.sock)
		return "✓ session closed", nil
	}
	return "", fmt.Errorf("unknown command %q", cmd)
}

func (s *cliSession) snapshot(args []string) (string, error) {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	interactive := fs.Bool("i", false, "interactive elements only")
	compact := fs.Bool("c", false, "compact (shorter text, no empty containers)")
	depth := fs.Int("d", 0, "depth limit")
	sel := fs.String("s", "", "scope to a CSS selector or ref")
	delta := fs.Bool("delta", false, "print only lines changed since the last snapshot in this session")
	full := fs.Bool("full", false, "with --delta: print everything and reset the baseline")
	maxChars := fs.Int("max", 0, "max output characters")
	// allow flags anywhere
	var flagArgs, rest []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else if len(flagArgs) > 0 && !strings.Contains(flagArgs[len(flagArgs)-1], "=") && snapFlagNeedsValue(flagArgs[len(flagArgs)-1]) && len(rest) == 0 {
			flagArgs = append(flagArgs, a)
		} else {
			rest = append(rest, a)
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return "", err
	}
	p := map[string]any{"interactiveOnly": *interactive, "maxDepth": *depth, "maxChars": *maxChars}
	if *sel != "" {
		p["selector"] = normSel(*sel)
	}
	if *compact {
		p["maxTextLen"] = 80
	}
	if *delta && !*full {
		p["delta"] = true
	}
	r, err := s.call("Page.ariaSnapshot", p)
	if err != nil {
		return "", err
	}
	yaml, _ := field(r, "yaml").(string)
	if s.jsonOut {
		b, _ := json.Marshal(map[string]any{"snapshot": yaml})
		return string(b), nil
	}
	return yaml, nil
}

func snapFlagNeedsValue(flag string) bool {
	switch strings.TrimLeft(flag, "-") {
	case "d", "s", "max":
		return true
	}
	return false
}

func (s *cliSession) get(args []string) (string, error) {
	if err := need(args, 1, "get text|html|value|attr|title|url|count|box [selector] [name]"); err != nil {
		return "", err
	}
	what := args[0]
	sel := "body"
	if len(args) > 1 {
		sel = normSel(args[1])
	}
	var r json.RawMessage
	var err error
	var key string
	switch what {
	case "title":
		r, err = s.call("Page.title", nil)
		key = "title"
	case "url":
		r, err = s.call("Page.url", nil)
		key = "url"
	case "text":
		r, err = s.call("Locator.textContent", map[string]any{"selector": sel})
		key = "text"
	case "html":
		r, err = s.call("Locator.innerHTML", map[string]any{"selector": sel})
		key = "html"
	case "value":
		r, err = s.call("Locator.getAttribute", map[string]any{"selector": sel, "name": "value"})
		key = "value"
	case "attr":
		if len(args) < 3 {
			return "", fmt.Errorf("usage: sweef get attr <selector> <name>")
		}
		r, err = s.call("Locator.getAttribute", map[string]any{"selector": sel, "name": args[2]})
		key = "value"
	case "count":
		r, err = s.call("Locator.count", map[string]any{"selector": sel})
		key = "count"
	case "box":
		r, err = s.call("Locator.boundingBox", map[string]any{"selector": sel})
	default:
		return "", fmt.Errorf("unknown get %q", what)
	}
	if err != nil {
		return "", err
	}
	if s.jsonOut || key == "" {
		return string(r), nil
	}
	v := field(r, key)
	if v == nil {
		return "", nil
	}
	return fmt.Sprint(v), nil
}

func (s *cliSession) wait(args []string) (string, error) {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	text := fs.String("text", "", "wait for text to appear")
	urlGlob := fs.String("url", "", "wait for URL glob")
	fn := fs.String("fn", "", "wait for JS expression to be truthy")
	load := fs.String("load", "", "load|domcontentloaded|networkidle")
	state := fs.String("state", "visible", "attached|detached|visible|hidden")
	var flagArgs, rest []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flagArgs = append(flagArgs, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
		} else {
			rest = append(rest, args[i])
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		return "", err
	}
	to := s.timeout.Milliseconds()
	switch {
	case *text != "":
		_, err := s.call("Page.waitForSelector", map[string]any{"selector": "text=" + *text, "state": "visible", "timeoutMs": to})
		return "✓ text visible", err
	case *urlGlob != "":
		_, err := s.call("Page.waitForURL", map[string]any{"pattern": *urlGlob, "timeoutMs": to})
		return "✓ url matched", err
	case *fn != "":
		_, err := s.call("Page.waitForFunction", map[string]any{"expression": *fn, "timeoutMs": to})
		return "✓ condition met", err
	case *load != "":
		_, err := s.call("Page.waitForLoadState", map[string]any{"state": *load, "timeoutMs": to})
		return "✓ " + *load, err
	case len(rest) > 0:
		if ms, err := strconv.Atoi(rest[0]); err == nil {
			// the daemon runs the page meanwhile; a sleep here left timers and animations frozen
			if _, err := s.call("Page.waitForTimeout", map[string]any{"ms": ms}); err != nil {
				return "", err
			}
			return fmt.Sprintf("✓ waited %dms", ms), nil
		}
		_, err := s.call("Page.waitForSelector", map[string]any{"selector": normSel(rest[0]), "state": *state, "timeoutMs": to})
		return "✓ " + rest[0] + " " + *state, err
	}
	return "", fmt.Errorf("usage: sweef wait <selector|ms> | --text t | --url glob | --fn js | --load state")
}

func (s *cliSession) set(args []string) (string, error) {
	if err := need(args, 2, "set viewport <w> <h> | set offline on|off | set headers '<json>' | set media dark|light | set geo <lat> <lng> | set ua <string>"); err != nil {
		return "", err
	}
	switch args[0] {
	case "viewport":
		if len(args) < 3 {
			return "", fmt.Errorf("usage: sweef set viewport <w> <h>")
		}
		w, _ := strconv.Atoi(args[1])
		h, _ := strconv.Atoi(args[2])
		_, err := s.call("Emulation.setDeviceMetrics", map[string]any{"width": w, "height": h})
		return "✓ viewport", err
	case "offline":
		_, err := s.call("Emulation.setOffline", map[string]any{"offline": args[1] != "off"})
		return "✓ offline " + args[1], err
	case "headers":
		var h map[string]any
		if err := json.Unmarshal([]byte(strings.Join(args[1:], " ")), &h); err != nil {
			return "", err
		}
		_, err := s.call("Network.setExtraHTTPHeaders", map[string]any{"headers": h})
		return "✓ headers", err
	case "media":
		_, err := s.call("Emulation.setColorScheme", map[string]any{"colorScheme": args[1]})
		return "✓ media " + args[1], err
	case "geo":
		if len(args) < 3 {
			return "", fmt.Errorf("usage: sweef set geo <lat> <lng>")
		}
		lat, _ := strconv.ParseFloat(args[1], 64)
		lng, _ := strconv.ParseFloat(args[2], 64)
		_, err := s.call("Emulation.setGeolocation", map[string]any{"latitude": lat, "longitude": lng})
		return "✓ geo", err
	case "ua", "useragent", "user-agent":
		_, err := s.call("Emulation.setUserAgentOverride", map[string]any{"userAgent": strings.Join(args[1:], " ")})
		return "✓ user agent", err
	}
	return "", fmt.Errorf("unknown setting %q", args[0])
}

func (s *cliSession) misc(cmd string, args []string) (string, error) {
	switch cmd {
	case "cookies":
		if len(args) == 0 {
			r, err := s.call("BrowserContext.storageState", nil)
			if err != nil {
				return "", err
			}
			if s.jsonOut {
				return string(r), nil
			}
			var st struct {
				Cookies []map[string]any `json:"cookies"`
			}
			json.Unmarshal(r, &st)
			if len(st.Cookies) == 0 {
				return "(no cookies)", nil
			}
			var sb strings.Builder
			for _, c := range st.Cookies {
				sb.WriteString(fmt.Sprintf("%v=%v  domain=%v path=%v\n", c["name"], c["value"], c["domain"], c["path"]))
			}
			return sb.String(), nil
		}
		switch args[0] {
		case "clear":
			_, err := s.call("BrowserContext.clearCookies", nil)
			return "✓ cookies cleared", err
		case "set":
			if len(args) < 3 {
				return "", fmt.Errorf("usage: sweef cookies set <name> <value>")
			}
			u, _ := s.call("Page.url", nil)
			host := ""
			if us, ok := field(u, "url").(string); ok {
				if i := strings.Index(us, "://"); i >= 0 {
					host = strings.SplitN(us[i+3:], "/", 2)[0]
					host = strings.SplitN(host, ":", 2)[0]
				}
			}
			_, err := s.call("BrowserContext.addCookies", map[string]any{"cookies": []map[string]any{{"name": args[1], "value": args[2], "domain": host, "path": "/"}}})
			return "✓ cookie set", err
		}
	case "storage":
		kind := "local"
		if len(args) > 0 && (args[0] == "local" || args[0] == "session") {
			kind, args = args[0], args[1:]
		}
		store := "localStorage"
		if kind == "session" {
			store = "sessionStorage"
		}
		switch {
		case len(args) == 0:
			r, err := s.call("Runtime.evaluate", map[string]any{"expression": "Object.fromEntries(Object.keys(" + store + ").map(k => [k, " + store + ".getItem(k)]))"})
			if err != nil {
				return "", err
			}
			b, _ := json.MarshalIndent(field(r, "result"), "", "  ")
			return string(b), nil
		case args[0] == "set" && len(args) >= 3:
			k, _ := json.Marshal(args[1])
			v, _ := json.Marshal(strings.Join(args[2:], " "))
			_, err := s.call("Runtime.evaluate", map[string]any{"expression": store + ".setItem(" + string(k) + "," + string(v) + ")"})
			return "✓ set", err
		case args[0] == "clear":
			_, err := s.call("Runtime.evaluate", map[string]any{"expression": store + ".clear()"})
			return "✓ cleared", err
		default:
			k, _ := json.Marshal(args[0])
			r, err := s.call("Runtime.evaluate", map[string]any{"expression": store + ".getItem(" + string(k) + ")"})
			if err != nil {
				return "", err
			}
			return fmt.Sprint(field(r, "result")), nil
		}
	case "state":
		if len(args) >= 2 && args[0] == "save" {
			_, err := s.call("BrowserContext.storageState", map[string]any{"path": args[1]})
			return "✓ saved " + args[1], err
		}
		if len(args) >= 2 && args[0] == "load" {
			_, err := s.call("BrowserContext.loadStorageState", map[string]any{"path": args[1]})
			return "✓ loaded " + args[1], err
		}
		return "", fmt.Errorf("usage: sweef state save|load <path>")
	case "dialog":
		if len(args) == 0 {
			return "", fmt.Errorf("usage: sweef dialog accept [text] | dismiss")
		}
		p := map[string]any{"action": args[0]}
		if len(args) > 1 {
			p["promptText"] = strings.Join(args[1:], " ")
		}
		_, err := s.call("Page.armDialog", p)
		return "✓ dialog " + args[0], err
	case "find":
		// find role button click --name "Sign in"  |  find text "Learn more" click
		if len(args) < 3 {
			return "", fmt.Errorf("usage: sweef find role|text|label|placeholder|alt|testid <value> <action> [value] [--name n]")
		}
		engine, value, action := args[0], args[1], args[2]
		name := ""
		var extra []string
		for i := 3; i < len(args); i++ {
			if args[i] == "--name" && i+1 < len(args) {
				name = args[i+1]
				i++
			} else {
				extra = append(extra, args[i])
			}
		}
		sel := ""
		switch engine {
		case "role":
			sel = "role=" + value
			if name != "" {
				sel += `[name="` + name + `"]`
			}
		case "text", "label", "placeholder", "alt", "testid":
			sel = engine + "=" + value
		default:
			return "", fmt.Errorf("unknown find engine %q", engine)
		}
		switch action {
		case "click", "hover", "check", "uncheck", "focus":
			return s.run(action, []string{sel})
		case "fill", "type":
			return s.run(action, append([]string{sel}, extra...))
		case "text":
			return s.run("get", []string{"text", sel})
		}
		return "", fmt.Errorf("unknown find action %q", action)
	}
	return "", fmt.Errorf("usage: sweef %s ...", cmd)
}

// batch runs several commands in one daemon round trip:
//
//	sweef batch "fill #email ada@x.io" "click text=Sign in" "expect url /welcome" --observe delta
//	printf 'open example.com\nsnapshot -i\n' | sweef batch -
//
// Flags: --observe none|interactive|full|delta|text (default interactive),
// --continue (do not stop at the first failing step).
func (s *cliSession) batch(args []string) (string, error) {
	observe, stop := "interactive", true
	var lines []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--observe" && i+1 < len(args):
			observe = args[i+1]
			i++
		case strings.HasPrefix(a, "--observe="):
			observe = strings.TrimPrefix(a, "--observe=")
		case a == "--continue":
			stop = false
		case a == "-":
			b, _ := readAll(os.Stdin)
			for _, l := range strings.Split(string(b), "\n") {
				lines = append(lines, l)
			}
		default:
			lines = append(lines, strings.Split(a, " ;; ")...)
		}
	}
	var steps []map[string]any
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		toks := shellSplit(l)
		if len(toks) > 0 && toks[0] == "sweef" {
			toks = toks[1:]
		}
		if len(toks) == 0 {
			continue
		}
		steps = append(steps, map[string]any{"cmd": toks[0], "args": toks[1:]})
	}
	if len(steps) == 0 {
		return "", fmt.Errorf("usage: sweef batch \"cmd args\" \"cmd args\" ... | sweef batch - < steps.txt")
	}
	r, err := s.call("Batch.run", map[string]any{"steps": steps, "observe": observe, "stopOnError": stop})
	if err != nil {
		return "", err
	}
	if s.jsonOut {
		return string(r), nil
	}
	text, ok := batchfmt.Format(r, 120)
	if !ok {
		// Print the per-step report, then exit non-zero.
		fmt.Print(text)
		return "", errBatchFailed
	}
	return text, nil
}

// shellSplit tokenises a command line with "double" and 'single' quotes.
func shellSplit(s string) []string {
	var out []string
	var cur strings.Builder
	var q byte
	has := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else if c == '\\' && q == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			q = c
			has = true
		case c == ' ' || c == '\t':
			if cur.Len() > 0 || has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 || has {
		out = append(out, cur.String())
	}
	return out
}

func readAll(f *os.File) ([]byte, error) {
	var b []byte
	r := bufio.NewReader(f)
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return b, nil
}

// sessionUsage lists the session verbs for `sweef help`.
func sessionUsage() string {
	keys := make([]string, 0, len(sessionCommands))
	for k := range sessionCommands {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, " ")
}
