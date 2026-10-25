package mcp

// Tool definitions. Every tools/list result is sent to the model on every turn
// by most hosts, so descriptions say what a model needs to choose and call the
// tool, and no more. Every RPC method is reachable: the common ones as tools,
// the rest through browser_call.

import "sync"

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	method      string         // RPC method the tool maps onto ("" = handled in callTool)
	action      bool           // changes the page: the reply carries the snapshot delta
}

type prop struct {
	name, typ, desc string
}

func schema(required []string, props ...prop) map[string]any {
	ps := map[string]any{}
	for _, p := range props {
		m := map[string]any{"type": p.typ}
		if p.desc != "" {
			m["description"] = p.desc
		}
		if p.typ == "array" {
			m["items"] = map[string]any{"type": "string"}
		}
		ps[p.name] = m
	}
	s := map[string]any{"type": "object", "properties": ps}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func req(names ...string) []string { return names }

var (
	pTab      = prop{"tab", "string", ""}
	pSel      = prop{"selector", "string", ""}
	pSince    = prop{"since", "number", "cursor from the previous reply"}
	pTimeout  = prop{"timeoutMs", "number", ""}
	pMaxChars = prop{"maxChars", "number", ""}
	pOffset   = prop{"offset", "number", ""}
	pContext  = prop{"context", "string", ""}
)

// toolList builds the tool table on first use: built at package init it was 800 allocations in the start of
// every sweef command.
var toolList = sync.OnceValue(func() []toolDef {
	return []toolDef{
		{Name: "browser_navigate", method: "Page.navigate",
			Description: "Open a URL in the active tab (absolute, or a path on the current site) and wait for load. Returns title and final URL.",
			InputSchema: schema(req("url"), prop{"url", "string", ""}, pTab)},
		{Name: "browser_snapshot", method: "Page.ariaSnapshot",
			Description: "Read the page as a YAML accessibility tree: headings, text, lists, tables and every control with role, name and state. Controls carry [ref=eN]: pass the ref (e12) as the selector of any tool; CSS and text=/role=/label=/placeholder= locators work too. delta=true: only lines changed since your last snapshot; interactiveOnly=true: controls and headings only.",
			InputSchema: schema(nil, pSel, prop{"interactiveOnly", "boolean", ""}, prop{"delta", "boolean", ""},
				prop{"maxChars", "number", "default 20000"}, prop{"maxTextLen", "number", "per text node, default 200"}, prop{"maxDepth", "number", ""}, pTab)},
		{Name: "browser_click", method: "Input.click", action: true,
			Description: "Click an element (links navigate, submit buttons submit).",
			InputSchema: schema(req("selector"), pSel, pTab)},
		{Name: "browser_fill", method: "Input.fill", action: true,
			Description: "Set the value of an input, textarea or select at once.",
			InputSchema: schema(req("selector", "value"), pSel, prop{"value", "string", ""}, pTab)},
		{Name: "browser_type", method: "Input.type", action: true,
			Description: "Type text key by key (for fields that react to each keystroke). Without a selector it types into the focused element, e.g. the field browser_click just clicked.",
			InputSchema: schema(req("text"), pSel, prop{"text", "string", ""}, prop{"delayMs", "number", ""}, pTab)},
		{Name: "browser_press", method: "Input.press", action: true,
			Description: "Press a key or combination on an element (Enter submits its form; Control+a, Tab, ArrowDown).",
			InputSchema: schema(req("selector"), pSel, prop{"key", "string", "default Enter"}, pTab)},
		{Name: "browser_select_option", method: "Input.selectOption", action: true,
			Description: "Select an option of a <select> by value or visible label.",
			InputSchema: schema(req("selector", "value"), pSel, prop{"value", "string", ""}, pTab)},
		{Name: "browser_check", method: "Input.check", action: true,
			Description: "Check a checkbox or radio.", InputSchema: schema(req("selector"), pSel, pTab)},
		{Name: "browser_uncheck", method: "Input.uncheck", action: true,
			Description: "Uncheck a checkbox.", InputSchema: schema(req("selector"), pSel, pTab)},
		{Name: "browser_hover", method: "Input.hover", action: true,
			Description: "Hover an element (menus and tooltips react).", InputSchema: schema(req("selector"), pSel, pTab)},
		{Name: "browser_drag", method: "Input.drag", action: true,
			Description: "Drag one element onto another (HTML5 drag and drop).",
			InputSchema: schema(req("source", "target"), prop{"source", "string", ""}, prop{"target", "string", ""}, pTab)},
		{Name: "browser_scroll", method: "Input.scroll", action: true,
			Description: "Scroll an element into view (selector), or the page by x/y pixels.",
			InputSchema: schema(nil, pSel, prop{"x", "number", ""}, prop{"y", "number", ""}, pTab)},
		{Name: "browser_mouse", method: "Input.mouse", action: true,
			Description: "Raw mouse at x,y (screenshot coordinates): action move|down|up|click.",
			InputSchema: schema(req("action", "x", "y"), prop{"action", "string", ""}, prop{"x", "number", ""}, prop{"y", "number", ""}, pTab)},
		{Name: "browser_keyboard", method: "Input.keyboard", action: true,
			Description: "Keyboard on the focused element: text types characters; key presses a key or combination (Enter, Control+a).",
			InputSchema: schema(nil, prop{"text", "string", ""}, prop{"key", "string", ""}, prop{"delayMs", "number", ""}, pTab)},
		{Name: "browser_evaluate", method: "Runtime.evaluate",
			Description: "Run JavaScript in the page (ES2023, full DOM, fetch, timers); expressions or statements, promises are awaited. Returns the JSON value; DOM nodes come back with a ref usable as selector.",
			InputSchema: schema(req("expression"), prop{"expression", "string", ""}, pMaxChars, pOffset, pTab)},
		{Name: "browser_back", method: "Page.goBack", action: true, Description: "History back.", InputSchema: schema(nil, pTab)},
		{Name: "browser_forward", method: "Page.goForward", action: true, Description: "History forward.", InputSchema: schema(nil, pTab)},
		{Name: "browser_reload", method: "Page.reload", action: true, Description: "Reload the page.", InputSchema: schema(nil, pTab)},
		{Name: "browser_screenshot", method: "Page.screenshot",
			Description: "Capture the viewport (or fullPage) as a PNG image. Coordinates match browser_mouse. text=true adds a text render of the page.",
			InputSchema: schema(nil, prop{"fullPage", "boolean", ""}, prop{"width", "number", ""}, prop{"height", "number", ""},
				prop{"deviceScaleFactor", "number", ""}, prop{"text", "boolean", ""}, pTab)},
		{Name: "browser_get_text", method: "Page.textContent",
			Description: "Visible text of an element (default: the whole page).",
			InputSchema: schema(nil, pSel, pMaxChars, pOffset, pTab)},
		{Name: "browser_get_html", method: "Page.content",
			Description: "The page's current HTML (serialised DOM).", InputSchema: schema(nil, pMaxChars, pOffset, pTab)},
		{Name: "browser_get_attribute", method: "Locator.getAttribute",
			Description: "Value of one attribute of an element; without name, all its attributes.",
			InputSchema: schema(req("selector"), pSel, prop{"name", "string", ""}, pTab)},
		{Name: "browser_locator_count", method: "Locator.count",
			Description: "Count elements matching a selector.", InputSchema: schema(req("selector"), pSel, pTab)},
		{Name: "browser_wait_for_selector", method: "Page.waitForSelector",
			Description: "Wait until a selector is visible (or state attached|detached|hidden). The page runs meanwhile.",
			InputSchema: schema(req("selector"), pSel, prop{"state", "string", ""}, pTimeout, pTab)},
		{Name: "browser_wait_for_function", method: "Page.waitForFunction",
			Description: "Wait until a JS expression is truthy.",
			InputSchema: schema(req("expression"), prop{"expression", "string", ""}, pTimeout, pTab)},
		{Name: "browser_wait_for_load_state", method: "Page.waitForLoadState",
			Description: "Wait for load|domcontentloaded|networkidle.",
			InputSchema: schema(nil, prop{"state", "string", ""}, pTimeout, pTab)},
		{Name: "browser_wait_for_url", method: "Page.waitForURL",
			Description: "Wait until the URL matches a glob (** any, * one segment) or substring.",
			InputSchema: schema(req("pattern"), prop{"pattern", "string", ""}, pTimeout, pTab)},
		{Name: "browser_wait", method: "Page.waitForTimeout",
			Description: "Let the page run for ms milliseconds (max 60000).",
			InputSchema: schema(req("ms"), prop{"ms", "number", ""}, pTab)},
		{Name: "browser_console_log", method: "Log.getConsole",
			Description: "Console messages since a cursor, one per line.", InputSchema: schema(nil, pSince, pMaxChars, pOffset, pTab)},
		{Name: "browser_network_log", method: "Network.getLogs",
			Description: "Network requests since a cursor, one line each (method, status, URL); verbose=true for headers.",
			InputSchema: schema(nil, pSince, prop{"verbose", "boolean", ""}, pMaxChars, pOffset, pTab)},
		{Name: "browser_events_poll", method: "Events.poll",
			Description: "Navigation, DOM mutation, console and network events since a cursor.",
			InputSchema: schema(nil, pSince, pMaxChars, pOffset, pTab)},
		{Name: "browser_tab_new", method: "Browser.newTab",
			Description: "Open a new tab (optionally at a URL); it becomes the active tab.", InputSchema: schema(nil, prop{"url", "string", ""})},
		{Name: "browser_tab_list", method: "Browser.listTabs", Description: "Open tabs: id, URL, title.", InputSchema: schema(nil)},
		{Name: "browser_tab_close", method: "Browser.closeTab", Description: "Close a tab.", InputSchema: schema(req("tab"), pTab)},
		{Name: "browser_dialog_respond", method: "Page.armDialog",
			Description: "How to answer the next alert/confirm/prompt: action accept|dismiss, promptText for prompt().",
			InputSchema: schema(req("action"), prop{"action", "string", ""}, prop{"promptText", "string", ""}, pTab)},
		{Name: "browser_route_mock", method: "Network.route",
			Description: "Intercept requests matching a URL glob or /regex/ (optionally method, bodyContains). action fulfill (status, body or json, contentType, headers), abort, or continue (url, newMethod, newBody, headers); times limits uses.",
			InputSchema: schema(req("pattern"), prop{"pattern", "string", ""}, prop{"action", "string", "fulfill|abort|continue"},
				prop{"method", "string", ""}, prop{"bodyContains", "string", ""}, prop{"status", "number", ""}, prop{"body", "string", ""},
				prop{"json", "object", "response body as JSON"}, prop{"contentType", "string", ""}, prop{"headers", "object", ""},
				prop{"url", "string", ""}, prop{"newMethod", "string", ""}, prop{"newBody", "string", ""}, prop{"times", "number", ""}, pTab)},
		{Name: "browser_unroute", method: "Network.unroute",
			Description: "Remove interception rules for a pattern (empty: all).", InputSchema: schema(nil, prop{"pattern", "string", ""}, pTab)},
		{Name: "browser_pdf", method: "Page.pdf",
			Description: "Save the page as PDF (to path, or a temp file) with format A4|Letter|Legal|A3|A5, landscape, margin pt, scale; returns the path.",
			InputSchema: schema(nil, prop{"path", "string", ""}, prop{"format", "string", ""}, prop{"landscape", "boolean", ""},
				prop{"margin", "number", ""}, prop{"scale", "number", ""}, prop{"displayHeaderFooter", "boolean", ""}, pTab)},
		{Name: "browser_downloads", method: "Page.downloads",
			Description: "Files the page downloaded, with saved paths.", InputSchema: schema(nil, pTab)},
		{Name: "browser_download", method: "Page.download",
			Description: "Download a URL (relative to the page) with the page's cookies.",
			InputSchema: schema(req("url"), prop{"url", "string", ""}, prop{"filename", "string", ""}, pTab)},
		{Name: "browser_expose_binding", method: "Runtime.addBinding",
			Description: "Expose window[name]() to page code; calls are recorded (browser_binding_calls) and return reply (JSON).",
			InputSchema: schema(req("name"), prop{"name", "string", ""}, prop{"reply", "string", ""}, pTab)},
		{Name: "browser_binding_calls", method: "Runtime.bindingCalls",
			Description: "Calls page code made to exposed bindings since a cursor.", InputSchema: schema(nil, pSince, pTab)},
		{Name: "browser_add_init_script", method: "Page.addInitScript",
			Description: "JS that runs before page scripts on every navigation.", InputSchema: schema(req("source"), prop{"source", "string", ""}, pTab)},
		{Name: "browser_har_export", method: "Network.har",
			Description: "Captured traffic as HAR 1.2 JSON.", InputSchema: schema(nil, pMaxChars, pOffset, pTab)},
		{Name: "browser_storage_state", method: "BrowserContext.storageState",
			Description: "Cookies and localStorage as Playwright storage_state JSON (path saves it).",
			InputSchema: schema(nil, prop{"path", "string", ""}, pContext)},
		{Name: "browser_set_storage_state", method: "BrowserContext.loadStorageState",
			Description: "Load storage_state JSON (state inline, or path) into a context.",
			InputSchema: schema(nil, prop{"state", "string", ""}, prop{"path", "string", ""}, pContext)},
		{Name: "browser_new_context", method: "Browser.newContext",
			Description: "Create an isolated context (own cookies and storage): userAgent, viewportWidth/Height, offline, colorScheme, proxy.",
			InputSchema: schema(nil, prop{"name", "string", ""}, prop{"userAgent", "string", ""}, prop{"viewportWidth", "number", ""},
				prop{"viewportHeight", "number", ""}, prop{"offline", "boolean", ""}, prop{"colorScheme", "string", ""}, prop{"proxy", "string", ""})},
		{Name: "browser_list_contexts", method: "Browser.listContexts", Description: "List contexts.", InputSchema: schema(nil)},
		{Name: "browser_close_context", method: "Browser.closeContext",
			Description: "Close a context and its tabs.", InputSchema: schema(req("context"), pContext)},
		{Name: "browser_emulate",
			Description: "Emulate on the tab. kind: viewport (value 1280x720 or 390x844@3), userAgent, geolocation (lat,lon), permissions (a,b), offline (true|false), colorScheme (dark|light), headers (JSON object).",
			InputSchema: schema(req("kind", "value"), prop{"kind", "string", ""}, prop{"value", "string", ""}, pTab)},
		{Name: "browser_request", method: "Fetch.request",
			Description: "HTTP request with the context's cookies, no page needed: method, url, headers, body -> status, headers, body.",
			InputSchema: schema(req("url"), prop{"method", "string", ""}, prop{"url", "string", ""}, prop{"headers", "object", ""},
				prop{"body", "string", ""}, pContext, pMaxChars, pOffset)},
		{Name: "browser_stats", method: "Browser.stats", Description: "Engine memory, goroutines, tabs.", InputSchema: schema(nil)},
		{Name: "browser_batch", method: "Batch.run", action: false,
			Description: "Run many steps in ONE call and get one observation back; use it whenever you know the next several actions. steps: [{cmd, args}] with cmd open, click, dblclick, fill, type, press, hover, focus, check, uncheck, select, scroll, get (text|html|value|attr|title|url|count), is (visible|enabled|checked), wait (selector|ms|text t|url glob|fn js|load), eval, back, forward, reload, sleep, snapshot, expect (text t|url s|title s|visible sel|hidden sel|count sel n|value sel v), or {method, params} for any RPC method. Stops at the first failure unless stopOnError=false or the step is optional. observe: interactive (default), full, delta, text, none.",
			InputSchema: map[string]any{"type": "object", "required": []string{"steps"}, "properties": map[string]any{
				"steps": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
					"cmd": map[string]any{"type": "string"}, "args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"method": map[string]any{"type": "string"}, "params": map[string]any{"type": "object"}, "optional": map[string]any{"type": "boolean"}}}},
				"stopOnError": map[string]any{"type": "boolean"},
				"observe":     map[string]any{"type": "string", "enum": []string{"none", "interactive", "full", "delta", "text"}},
				"tab":         map[string]any{"type": "string"}}}},
		{Name: "browser_call",
			Description: "Call any Sweef RPC method directly (Page.frames, Page.popups, BrowserContext.addCookies, Emulation.*, Locator.*, DOM.*, Network.setExtraHTTPHeaders, ...); params is the method's parameter object.",
			InputSchema: schema(req("method"), prop{"method", "string", ""}, prop{"params", "object", ""}, pMaxChars, pOffset)},
	}
})

var toolByName = sync.OnceValue(func() map[string]*toolDef {
	tools := toolList()
	m := map[string]*toolDef{}
	for i := range tools {
		m[tools[i].Name] = &tools[i]
	}
	m["browser_aria_snapshot"] = m["browser_snapshot"] // the pre-0.2 name; not listed, still answered
	return m
})
