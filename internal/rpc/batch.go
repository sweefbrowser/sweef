package rpc

// batch.go — run many steps in one round trip.
//
// Agents pay per tool call: a model round trip is seconds, a Sweef command
// is milliseconds. Batch.run lets an agent send a whole plan ("fill these
// three fields, tick the box, submit, wait for the dashboard, then show me
// what changed") as one call, and get back per-step results plus a single
// observation of the final page.
//
//	{"steps": [
//	   {"cmd": "fill",   "args": ["#email", "ada@example.com"]},
//	   {"cmd": "click",  "args": ["text=Sign in"]},
//	   {"cmd": "expect", "args": ["url", "/dashboard"]},
//	   {"method": "Runtime.evaluate", "params": {"expression": "document.title"}}
//	 ],
//	 "stopOnError": true,
//	 "observe": "delta"}            // none | interactive | full | delta | text
//
// Steps use the same verbs as the CLI (open, click, fill, type, press,
// hover, focus, check, uncheck, select, scroll, get, is, wait, eval, back,
// forward, reload, snapshot, expect, sleep) or a raw RPC method.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type batchStep struct {
	Cmd    string         `json:"cmd,omitempty"`
	Args   []string       `json:"args,omitempty"`
	Method string         `json:"method,omitempty"`
	Params map[string]any `json:"params,omitempty"`
	// Optional: continue past a failure of this step even when stopOnError.
	Optional bool `json:"optional,omitempty"`
}

type stepResult struct {
	Step   int    `json:"step"`
	Cmd    string `json:"cmd"`
	OK     bool   `json:"ok"`
	Ms     int64  `json:"ms"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// lastSnap remembers the previous snapshot per tab for delta observations.
var (
	lastSnapMu sync.Mutex
	lastSnap   = map[string]string{}
)

func (s *Server) batch(p Params) (any, error) {
	raw, _ := json.Marshal(p["steps"])
	var steps []batchStep
	if err := json.Unmarshal(raw, &steps); err != nil || len(steps) == 0 {
		return nil, fmt.Errorf("batch: steps must be a non-empty array of {cmd,args} or {method,params}")
	}
	if len(steps) > 500 {
		return nil, fmt.Errorf("batch: at most 500 steps per call")
	}
	stopOnError := true
	if v, ok := p["stopOnError"].(bool); ok {
		stopOnError = v
	}
	tab := str(p, "tab")
	start := time.Now()
	results := make([]stepResult, 0, len(steps))
	failed := 0
	for i, st := range steps {
		t0 := time.Now()
		// an input step settles fully only when it ends the batch: its observation follows at once
		if t, terr := s.tab(Params{"tab": tab}); terr == nil {
			t.SetFullInputSettle(i == len(steps)-1)
			defer t.SetFullInputSettle(false)
		}
		name := st.Cmd
		var res any
		var err error
		if st.Method != "" {
			name = st.Method
			params := st.Params
			if params == nil {
				params = map[string]any{}
			}
			if _, has := params["tab"]; !has && tab != "" {
				params["tab"] = tab
			}
			res, err = s.dispatch(st.Method, params)
		} else {
			res, err = s.runVerb(tab, st.Cmd, st.Args)
		}
		r := stepResult{Step: i + 1, Cmd: strings.TrimSpace(name + " " + strings.Join(st.Args, " ")), OK: err == nil, Ms: time.Since(t0).Milliseconds(), Result: res}
		if err != nil {
			r.Error = err.Error()
			r.Result = nil
			failed++
		}
		results = append(results, r)
		if err != nil && stopOnError && !st.Optional {
			break
		}
	}
	out := map[string]any{
		"ok":      failed == 0,
		"steps":   results,
		"ran":     len(results),
		"total":   len(steps),
		"failed":  failed,
		"totalMs": time.Since(start).Milliseconds(),
	}
	if obs := str(p, "observe"); obs != "" && obs != "none" {
		if o, err := s.observe(tab, obs); err == nil {
			out["observation"] = o
		} else {
			out["observationError"] = err.Error()
		}
	}
	if t, err := s.tab(Params{"tab": tab}); err == nil {
		out["url"], out["title"] = t.URL(), t.Title()
	}
	return out, nil
}

// observe returns the final page view requested by a batch.
func (s *Server) observe(tab, mode string) (string, error) {
	t, err := s.tab(Params{"tab": tab})
	if err != nil {
		return "", err
	}
	switch mode {
	case "text":
		return t.Text("body")
	case "interactive", "full", "delta":
		o := snapshotOptions(Params{"interactiveOnly": mode == "interactive"})
		y, err := t.AriaSnapshotV2(o)
		if err != nil {
			return "", err
		}
		if mode == "delta" {
			return deltaFor(t.ID, y), nil
		}
		rememberSnap(t.ID, y)
		return y, nil
	}
	return "", fmt.Errorf("observe must be none, interactive, full, delta or text")
}

func rememberSnap(tabID, y string) {
	lastSnapMu.Lock()
	lastSnap[tabID] = y
	lastSnapMu.Unlock()
}

// deltaFor diffs against the previous snapshot of the tab and remembers y.
func deltaFor(tabID, y string) string {
	lastSnapMu.Lock()
	prev, ok := lastSnap[tabID]
	lastSnap[tabID] = y
	lastSnapMu.Unlock()
	if !ok {
		return y
	}
	if prev == y {
		return "(no changes since last snapshot)"
	}
	return LineDiff(prev, y)
}

func normRef(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "@e") {
		return s[1:]
	}
	return s
}

// runVerb executes one CLI-style verb against the server.
func (s *Server) runVerb(tab, verb string, args []string) (any, error) {
	need := func(n int, usage string) error {
		if len(args) < n {
			return fmt.Errorf("usage: %s", usage)
		}
		return nil
	}
	call := func(method string, params map[string]any) (any, error) {
		if tab != "" {
			params["tab"] = tab
		}
		return s.dispatch(method, params)
	}
	switch verb {
	case "open", "goto", "navigate":
		if err := need(1, "open <url>"); err != nil {
			return nil, err
		}
		u := args[0]
		if !strings.Contains(u, "://") && !strings.HasPrefix(u, "about:") {
			u = "https://" + u
		}
		return call("Page.navigate", map[string]any{"url": u})
	case "click", "dblclick", "hover", "focus", "check", "uncheck", "tap":
		if err := need(1, verb+" <selector>"); err != nil {
			return nil, err
		}
		m := map[string]string{"click": "Input.click", "dblclick": "Input.click", "hover": "Input.hover", "focus": "Input.focus",
			"check": "Input.check", "uncheck": "Input.uncheck", "tap": "Input.tap"}[verb]
		return call(m, map[string]any{"selector": normRef(args[0])})
	case "fill":
		if err := need(2, "fill <selector> <text>"); err != nil {
			return nil, err
		}
		return call("Input.fill", map[string]any{"selector": normRef(args[0]), "value": strings.Join(args[1:], " ")})
	case "type":
		if len(args) == 1 { // "type <text>": into the focused element, after a click
			return call("Input.keyboard", map[string]any{"text": args[0]})
		}
		if err := need(2, "type [selector] <text>"); err != nil {
			return nil, err
		}
		return call("Input.type", map[string]any{"selector": normRef(args[0]), "text": strings.Join(args[1:], " ")})
	case "press", "key":
		if err := need(1, "press <key> [selector]"); err != nil {
			return nil, err
		}
		if len(args) > 1 {
			return call("Input.press", map[string]any{"selector": normRef(args[1]), "key": args[0]})
		}
		return call("Input.keyboard", map[string]any{"key": args[0]})
	case "select":
		if err := need(2, "select <selector> <value>"); err != nil {
			return nil, err
		}
		return call("Input.selectOption", map[string]any{"selector": normRef(args[0]), "value": strings.Join(args[1:], " ")})
	case "scroll":
		dy := 600
		if len(args) > 1 {
			dy, _ = strconv.Atoi(args[1])
		}
		if len(args) > 0 && args[0] == "up" {
			dy = -dy
		}
		return call("Input.scroll", map[string]any{"x": 0, "y": dy})
	case "eval":
		if err := need(1, "eval <js>"); err != nil {
			return nil, err
		}
		r, err := call("Runtime.evaluate", map[string]any{"expression": strings.Join(args, " ")})
		if m, ok := r.(map[string]any); ok {
			return m["result"], err
		}
		return r, err
	case "back":
		return call("Page.goBack", map[string]any{})
	case "forward":
		return call("Page.goForward", map[string]any{})
	case "reload":
		return call("Page.reload", map[string]any{})
	case "sleep":
		ms := 100
		if len(args) > 0 {
			ms, _ = strconv.Atoi(args[0])
		}
		if ms > 30000 {
			ms = 30000
		}
		if _, err := call("Page.waitForTimeout", map[string]any{"ms": ms}); err != nil { // the page runs meanwhile
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		return map[string]any{"slept": ms}, nil
	case "snapshot":
		mode := "full"
		for _, a := range args {
			switch a {
			case "-i":
				mode = "interactive"
			case "--delta":
				mode = "delta"
			}
		}
		return s.observe(tab, mode)
	case "get":
		if err := need(1, "get text|html|value|attr|title|url|count [selector] [name]"); err != nil {
			return nil, err
		}
		sel := "body"
		if len(args) > 1 {
			sel = normRef(args[1])
		}
		switch args[0] {
		case "title":
			return call("Page.title", map[string]any{})
		case "url":
			return call("Page.url", map[string]any{})
		case "text":
			return call("Locator.textContent", map[string]any{"selector": sel})
		case "html":
			return call("Locator.innerHTML", map[string]any{"selector": sel})
		case "value":
			return call("Locator.getAttribute", map[string]any{"selector": sel, "name": "value"})
		case "attr":
			if err := need(3, "get attr <selector> <name>"); err != nil {
				return nil, err
			}
			return call("Locator.getAttribute", map[string]any{"selector": sel, "name": args[2]})
		case "count":
			return call("Locator.count", map[string]any{"selector": sel})
		}
		return nil, fmt.Errorf("unknown get %q", args[0])
	case "is":
		if err := need(2, "is visible|enabled|checked <selector>"); err != nil {
			return nil, err
		}
		k := args[0]
		return call("Locator.is"+strings.ToUpper(k[:1])+k[1:], map[string]any{"selector": normRef(args[1])})
	case "wait":
		if err := need(1, "wait <selector|ms> | wait text <t> | wait url <glob> | wait fn <js> | wait load <state>"); err != nil {
			return nil, err
		}
		switch args[0] {
		case "text", "--text":
			return call("Page.waitForSelector", map[string]any{"selector": "text=" + strings.Join(args[1:], " "), "state": "visible", "timeoutMs": 10000})
		case "url", "--url":
			return call("Page.waitForURL", map[string]any{"pattern": strings.Join(args[1:], " "), "timeoutMs": 10000})
		case "fn", "--fn":
			return call("Page.waitForFunction", map[string]any{"expression": strings.Join(args[1:], " "), "timeoutMs": 10000})
		case "load", "--load":
			st := "load"
			if len(args) > 1 {
				st = args[1]
			}
			return call("Page.waitForLoadState", map[string]any{"state": st, "timeoutMs": 30000})
		}
		if ms, err := strconv.Atoi(args[0]); err == nil {
			return s.runVerb(tab, "sleep", []string{strconv.Itoa(ms)})
		}
		return call("Page.waitForSelector", map[string]any{"selector": normRef(args[0]), "state": "visible", "timeoutMs": 10000})
	case "expect":
		return s.expect(tab, args)
	case "download":
		if err := need(1, "download <url> [filename]"); err != nil {
			return nil, err
		}
		p := map[string]any{"url": args[0]}
		if len(args) > 1 {
			p["filename"] = args[1]
		}
		return call("Page.download", p)
	case "pdf":
		if err := need(1, "pdf <path>"); err != nil {
			return nil, err
		}
		return call("Page.pdf", map[string]any{"path": args[0]})
	}
	return nil, fmt.Errorf("unknown batch verb %q", verb)
}

// expect asserts something about the page, auto-waiting up to 5 s:
//
//	expect text <t>            text appears somewhere visible
//	expect url <substring>     URL contains
//	expect title <substring>   title contains
//	expect visible <sel>       / hidden <sel>
//	expect count <sel> <n>     exactly n matches
//	expect value <sel> <v>     input value equals
func (s *Server) expect(tab string, args []string) (any, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("usage: expect text|url|title|visible|hidden|count|value ...")
	}
	t, err := s.tab(Params{"tab": tab})
	if err != nil {
		return nil, err
	}
	kind, rest := args[0], args[1:]
	want := strings.Join(rest, " ")
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for {
		ok := false
		switch kind {
		case "text":
			n, _ := t.Locator("text=" + want).Count()
			ok, last = n > 0, fmt.Sprintf("%d matches", n)
		case "url":
			last = t.URL()
			ok = strings.Contains(last, want)
		case "title":
			last = t.Title()
			ok = strings.Contains(last, want)
		case "visible", "hidden":
			v, _ := t.Locator(normRef(want)).IsVisible()
			ok, last = v == (kind == "visible"), fmt.Sprintf("visible=%v", v)
		case "count":
			if len(rest) < 2 {
				return nil, fmt.Errorf("usage: expect count <selector> <n>")
			}
			n, _ := t.Locator(normRef(rest[0])).Count()
			wantN, _ := strconv.Atoi(rest[1])
			ok, last = n == wantN, strconv.Itoa(n)
		case "value":
			if len(rest) < 2 {
				return nil, fmt.Errorf("usage: expect value <selector> <value>")
			}
			v, _, _ := t.Locator(normRef(rest[0])).GetAttribute("value")
			ok, last = v == strings.Join(rest[1:], " "), v
		default:
			return nil, fmt.Errorf("unknown expectation %q", kind)
		}
		if ok {
			return map[string]any{"passed": true}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("expect %s %q failed: got %s", kind, want, last)
		}
		t.WakeJS() // an expectation is waiting for content the page's scripts may still add
		t.Settle(50 * time.Millisecond)
	}
}

// LineDiff is a small LCS line diff (+ added, - removed); above ~16M cells
// it falls back to a multiset difference.
func LineDiff(a, b string) string {
	al, bl := strings.Split(strings.TrimRight(a, "\n"), "\n"), strings.Split(strings.TrimRight(b, "\n"), "\n")
	var sb strings.Builder
	if len(al)*len(bl) > 16_000_000 {
		seen := map[string]int{}
		for _, l := range al {
			seen[l]++
		}
		for _, l := range bl {
			if seen[l] > 0 {
				seen[l]--
			} else {
				sb.WriteString("+ " + l + "\n")
			}
		}
		for l, n := range seen {
			for ; n > 0; n-- {
				sb.WriteString("- " + l + "\n")
			}
		}
		return sb.String()
	}
	n, m := len(al), len(bl)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			sb.WriteString("- " + al[i] + "\n")
			i++
		default:
			sb.WriteString("+ " + bl[j] + "\n")
			j++
		}
	}
	for ; i < n; i++ {
		sb.WriteString("- " + al[i] + "\n")
	}
	for ; j < m; j++ {
		sb.WriteString("+ " + bl[j] + "\n")
	}
	return sb.String()
}
