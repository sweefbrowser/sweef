package net

import (
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Snapshot v2 — the Playwright-format accessibility tree agents read.
//
// Design (see docs: "Sweef Snapshot v2 Spec"):
//   - every element is classified as prune / emit / descend;
//   - refs are small sequential ids from the Doc id table and appear only on
//     nodes an agent can act on, which is where most of the token saving is;
//   - content (headings, paragraphs, list items, cells, bare text) is kept so
//     an agent can read a page, not just its controls.

// SnapshotOptions tune the size and scope of one snapshot.
type SnapshotOptions struct {
	MaxChars        int    // hard cap on output bytes (0 = 20000, about 5k tokens)
	Selector        string // CSS selector or ref scoping the walk (default body)
	InteractiveOnly bool   // keep only ref-bearing nodes + their heading context
	MaxTextLen      int    // characters kept per text/paragraph node (0 = 200)
	MaxDepth        int    // 0 = unlimited
	// Frame, when set, supplies the snapshot lines of the document inside an <iframe>
	// element (already indented relative to the iframe line, refs qualified with the
	// frame prefix it returns), so one snapshot covers the page and its frames.
	Frame func(n *html.Node) (ref string, lines []string)
}

func (o *SnapshotOptions) defaults() {
	if o.MaxChars <= 0 {
		o.MaxChars = 20000
	}
	if o.MaxTextLen <= 0 {
		o.MaxTextLen = 200
	}
}

// snapNode is one line of the tree before serialisation.
type snapNode struct {
	role        string // "" for a bare text node
	name        string
	text        string // bare text content when role == ""
	attrs       []string
	ref         int64 // 0 = no ref emitted
	href        string
	value       string
	hasValue    bool
	children    []*snapNode
	interactive bool
	heading     bool
	raw         []string // pre-rendered lines nested under this node (an iframe's document)
}

// ---- role computation ------------------------------------------------------

var implicitRoles = map[string]string{
	"button": "button", "select": "combobox", "textarea": "textbox",
	"h1": "heading", "h2": "heading", "h3": "heading", "h4": "heading", "h5": "heading", "h6": "heading",
	"nav": "navigation", "main": "main", "header": "banner", "footer": "contentinfo",
	"aside": "complementary", "form": "form", "article": "article", "dialog": "dialog",
	"ul": "list", "ol": "list", "menu": "list", "li": "listitem",
	"table": "table", "thead": "rowgroup", "tbody": "rowgroup", "tfoot": "rowgroup",
	"tr": "row", "th": "columnheader", "td": "cell", "caption": "caption",
	"p": "paragraph", "blockquote": "blockquote", "code": "code", "pre": "code",
	"option": "option", "optgroup": "group", "progress": "progressbar", "meter": "meter",
	"hr": "separator", "details": "group", "summary": "button", "img": "img",
	"fieldset": "group", "legend": "legend", "search": "search", "output": "status",
	"dl": "list", "dt": "term", "dd": "definition", "figure": "figure", "time": "time",
	"strong": "strong", "em": "emphasis", "sup": "superscript", "sub": "subscript",
	"del": "deletion", "ins": "insertion", "mark": "mark",
}

var inputRoles = map[string]string{
	"": "textbox", "text": "textbox", "email": "textbox", "tel": "textbox", "url": "textbox",
	"password": "textbox", "number": "spinbutton", "search": "searchbox", "range": "slider",
	"checkbox": "checkbox", "radio": "radio", "submit": "button", "button": "button",
	"reset": "button", "image": "button", "file": "button", "color": "button",
	"date": "textbox", "datetime-local": "textbox", "month": "textbox", "week": "textbox", "time": "textbox",
}

var leafRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true, "checkbox": true, "radio": true,
	"combobox": true, "img": true, "option": true, "menuitem": true, "menuitemcheckbox": true,
	"menuitemradio": true, "slider": true, "spinbutton": true, "switch": true, "tab": true,
	"progressbar": true, "separator": true, "meter": true,
}

var interactiveRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true, "checkbox": true, "radio": true,
	"combobox": true, "listbox": true, "option": true, "menuitem": true, "menuitemcheckbox": true,
	"menuitemradio": true, "slider": true, "spinbutton": true, "switch": true, "tab": true,
	"treeitem": true, "menubar": false,
}

// nameFromContent lists roles whose accessible name comes from their text.
var nameFromContent = map[string]bool{
	"button": true, "link": true, "heading": true, "columnheader": true, "rowheader": true,
	"option": true, "menuitem": true, "menuitemcheckbox": true, "menuitemradio": true,
	"tab": true, "treeitem": true, "switch": true, "checkbox": true, "radio": true,
	"tooltip": true, "legend": true, "caption": true, "term": true,
}

// compactRoles are emitted as `- role: text` when their only content is text.
var compactRoles = map[string]bool{
	"paragraph": true, "listitem": true, "cell": true, "blockquote": true, "code": true,
	"definition": true, "status": true, "alert": true, "note": true, "time": true,
	"strong": true, "emphasis": true, "superscript": true, "subscript": true, "deletion": true,
	"insertion": true, "mark": true, "figure": true, "group": true, "region": true,
}

// explicitAriaRole returns a usable role="..." value.
func explicitAriaRole(n *html.Node) string {
	r := strings.TrimSpace(strings.ToLower(attr(n, "role")))
	if r == "" || r == "generic" || r == "none" || r == "presentation" {
		return ""
	}
	// role may list fallbacks: "switch checkbox"
	if i := strings.IndexByte(r, ' '); i > 0 {
		r = r[:i]
	}
	return r
}

// computeRole is the single source of truth for an element's role.
func computeRole(n *html.Node) string {
	if n.Type != html.ElementNode {
		return ""
	}
	if r := explicitAriaRole(n); r != "" {
		return r
	}
	switch n.Data {
	case "a", "area":
		if hasAttr(n, "href") {
			return "link"
		}
		return ""
	case "input":
		if r, ok := inputRoles[strings.ToLower(attr(n, "type"))]; ok {
			return r
		}
		return "textbox"
	case "section":
		if attr(n, "aria-label") != "" || attr(n, "aria-labelledby") != "" {
			return "region"
		}
		return ""
	case "select":
		if hasAttr(n, "multiple") || attr(n, "size") != "" && attr(n, "size") != "1" {
			return "listbox"
		}
		return "combobox"
	}
	return implicitRoles[n.Data]
}

// ---- accessible name -------------------------------------------------------

func rootOf(n *html.Node) *html.Node {
	for n.Parent != nil {
		n = n.Parent
	}
	return n
}

func findByID(root *html.Node, id string) *html.Node {
	var res *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if res != nil {
			return
		}
		if n.Type == html.ElementNode && attr(n, "id") == id {
			res = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return res
}

// labelForControl finds <label for=id> or a wrapping <label>.
func labelForControl(n *html.Node) string {
	if id := attr(n, "id"); id != "" {
		var lab *html.Node
		var walk func(*html.Node)
		walk = func(c *html.Node) {
			if lab != nil {
				return
			}
			if c.Type == html.ElementNode && c.Data == "label" && attr(c, "for") == id {
				lab = c
				return
			}
			for k := c.FirstChild; k != nil; k = k.NextSibling {
				walk(k)
			}
		}
		walk(rootOf(n))
		if lab != nil {
			return normWS(innerText(lab))
		}
	}
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "label" {
			return normWS(innerText(p))
		}
	}
	return ""
}

// accessibleName implements the accname subset agents need:
// aria-label > aria-labelledby > <label> > alt > placeholder > title >
// value (submit buttons) > content (for name-from-content roles).
func accessibleName(n *html.Node) string {
	return accessibleNameRole(n, computeRole(n))
}

func accessibleNameRole(n *html.Node, role string) string {
	if v := attr(n, "aria-label"); strings.TrimSpace(v) != "" {
		return normWS(v)
	}
	if ids := attr(n, "aria-labelledby"); ids != "" {
		root := rootOf(n)
		var parts []string
		for _, id := range strings.Fields(ids) {
			if ln := findByID(root, id); ln != nil {
				if v := attr(ln, "aria-label"); v != "" {
					parts = append(parts, v)
				} else {
					parts = append(parts, innerText(ln))
				}
			}
		}
		if s := normWS(strings.Join(parts, " ")); s != "" {
			return s
		}
	}
	switch n.Data {
	case "input", "textarea", "select", "meter", "progress", "output":
		if lab := labelForControl(n); lab != "" {
			return lab
		}
	}
	if n.Data == "img" || n.Data == "area" || (n.Data == "input" && attr(n, "type") == "image") {
		if v := attr(n, "alt"); v != "" {
			return normWS(v)
		}
	}
	if n.Data == "input" {
		switch strings.ToLower(attr(n, "type")) {
		case "submit", "button", "reset":
			if v := attr(n, "value"); v != "" {
				return normWS(v)
			}
			if attr(n, "type") == "submit" {
				return "Submit"
			}
		}
	}
	if v := attr(n, "placeholder"); v != "" {
		return normWS(v)
	}
	if nameFromContent[role] {
		if t := normWS(innerTextWithAlt(n)); t != "" {
			return t
		}
	}
	if v := attr(n, "title"); v != "" {
		return normWS(v)
	}
	if n.Data == "img" {
		return ""
	}
	return ""
}

// innerTextWithAlt is innerText that also includes img alt / aria-label of
// descendants, as accname's name-from-content does.
func innerTextWithAlt(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.ElementNode {
			switch x.Data {
			case "script", "style", "noscript", "template":
				return
			}
			if attr(x, "aria-hidden") == "true" {
				return
			}
			if v := attr(x, "aria-label"); v != "" && x != n {
				sb.WriteString(v)
				sb.WriteByte(' ')
				return
			}
			if x.Data == "img" {
				sb.WriteString(attr(x, "alt"))
				sb.WriteByte(' ')
				return
			}
		}
		if x.Type == html.TextNode {
			sb.WriteString(x.Data)
			sb.WriteByte(' ')
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func inputValue(n *html.Node) string {
	switch n.Data {
	case "textarea":
		return normWS(innerText(n))
	case "input":
		switch strings.ToLower(attr(n, "type")) {
		case "password":
			if attr(n, "value") != "" {
				return "•••"
			}
			return ""
		case "checkbox", "radio", "submit", "button", "reset", "image", "file", "hidden":
			return ""
		}
		return attr(n, "value")
	case "select":
		for _, o := range optionsOf(n) {
			if hasAttr(o, "selected") {
				return normWS(innerText(o))
			}
		}
		if os := optionsOf(n); len(os) > 0 && !hasAttr(n, "multiple") {
			return normWS(innerText(os[0]))
		}
	}
	return ""
}

func optionsOf(sel *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				if c.Data == "option" {
					out = append(out, c)
				} else if c.Data == "optgroup" {
					walk(c)
				}
			}
		}
	}
	walk(sel)
	return out
}

// ---- pruning ---------------------------------------------------------------

// styleHidden reports whether the cascade (inline style plus stylesheets
// installed with SetStylesheets) hides n: display:none or visibility
// hidden/collapse. visibility inherits, so a hidden ancestor hides
// descendants that do not set visibility:visible themselves; the snapshot
// walk stops at the first hidden element anyway. opacity:0 does not hide: it
// does not remove an element from the accessibility tree (Chromium,
// Playwright's snapshot), and content that animates in from opacity 0 (GSAP,
// Framer Motion, AOS) vanished from the snapshot until the animation ran.
func (d *Doc) styleHidden(n *html.Node) bool {
	d.cssMu.Lock()
	cs := d.styleLocked(n)
	d.cssMu.Unlock()
	if cs.display == "none" {
		return true
	}
	if cs.visibility == "hidden" || cs.visibility == "collapse" {
		return true
	}
	return false
}

// pruned reports whether n and its whole subtree are excluded from the tree.
func (d *Doc) pruned(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.Data {
	case "script", "style", "noscript", "template", "head", "title", "meta", "link", "base", "svg", "canvas", "iframe", "object", "embed":
		if n.Data == "svg" || n.Data == "canvas" || n.Data == "iframe" {
			// keep an svg/canvas with an accessible name as an img line; handled by caller
			return false
		}
		return true
	}
	if attr(n, "aria-hidden") == "true" {
		return true
	}
	if n.Data == "input" && strings.EqualFold(attr(n, "type"), "hidden") {
		return true
	}
	// The hidden attribute is a UA rule `[hidden]{display:none}`: an author
	// rule for display beats it (e.g. `.show{display:block}`), but only once
	// stylesheets are installed; without any, it prunes unconditionally.
	if hasAttr(n, "hidden") {
		d.cssMu.Lock()
		hasSheets := d.css != nil && d.css.hasSheets
		d.cssMu.Unlock()
		if !hasSheets {
			return true
		}
	}
	return d.styleHidden(n)
}

func isInteractiveElement(n *html.Node, role string) bool {
	if interactiveRoles[role] {
		return true
	}
	if hasAttr(n, "tabindex") && attr(n, "tabindex") != "-1" {
		return true
	}
	if hasAttr(n, "onclick") || hasAttr(n, "contenteditable") {
		return true
	}
	switch n.Data {
	case "summary", "details", "label":
		return true
	}
	return false
}

// ---- tree building ---------------------------------------------------------

func (d *Doc) buildSnap(n *html.Node, o *SnapshotOptions, depth int) []*snapNode {
	if n.Type == html.TextNode {
		t := normWS(n.Data)
		if t == "" {
			return nil
		}
		return []*snapNode{{text: truncate(t, o.MaxTextLen)}}
	}
	if n.Type != html.ElementNode && n.Type != html.DocumentNode {
		return nil
	}
	if d.pruned(n) {
		return nil
	}
	role := computeRole(n)
	if n.Data == "svg" || n.Data == "canvas" || n.Data == "iframe" {
		name := accessibleNameRole(n, "img")
		if name == "" {
			if n.Data == "iframe" {
				name = attr(n, "src")
			} else {
				return nil
			}
		}
		if n.Data == "iframe" {
			sn := &snapNode{role: "iframe", name: name}
			if o.Frame != nil {
				if ref, lines := o.Frame(n); len(lines) > 0 {
					sn.raw = lines
					if ref != "" {
						sn.attrs = append(sn.attrs, "frame="+ref)
					}
				}
			}
			return []*snapNode{sn}
		}
		return []*snapNode{{role: "img", name: name}}
	}

	if role == "" {
		// transparent: descend
		return d.buildChildren(n, o, depth)
	}
	if o.MaxDepth > 0 && depth > o.MaxDepth {
		return nil
	}

	sn := &snapNode{role: role}
	sn.name = accessibleNameRole(n, role)
	sn.interactive = isInteractiveElement(n, role)
	sn.heading = role == "heading"
	if sn.interactive || sn.heading {
		sn.ref = d.refOf(n)
	}

	// attributes
	if role == "heading" {
		lvl := attr(n, "aria-level")
		if lvl == "" && len(n.Data) == 2 && n.Data[0] == 'h' {
			lvl = n.Data[1:]
		}
		if lvl != "" {
			sn.attrs = append(sn.attrs, "level="+lvl)
		}
	}
	switch role {
	case "checkbox", "radio", "switch", "menuitemcheckbox", "menuitemradio":
		if hasAttr(n, "checked") || attr(n, "aria-checked") == "true" {
			sn.attrs = append(sn.attrs, "checked")
		} else if attr(n, "aria-checked") == "mixed" {
			sn.attrs = append(sn.attrs, "checked=mixed")
		}
	}
	if hasAttr(n, "disabled") || attr(n, "aria-disabled") == "true" {
		sn.attrs = append(sn.attrs, "disabled")
	}
	if v := attr(n, "aria-expanded"); v == "true" {
		sn.attrs = append(sn.attrs, "expanded")
	} else if n.Data == "details" && hasAttr(n, "open") {
		sn.attrs = append(sn.attrs, "expanded")
	}
	if hasAttr(n, "selected") || attr(n, "aria-selected") == "true" {
		sn.attrs = append(sn.attrs, "selected")
	}
	if attr(n, "aria-pressed") == "true" {
		sn.attrs = append(sn.attrs, "pressed")
	}
	if hasAttr(n, "required") || attr(n, "aria-required") == "true" {
		sn.attrs = append(sn.attrs, "required")
	}
	if hasAttr(n, "readonly") || attr(n, "aria-readonly") == "true" {
		sn.attrs = append(sn.attrs, "readonly")
	}
	if n.Data == "input" && strings.EqualFold(attr(n, "type"), "password") {
		sn.attrs = append(sn.attrs, "type=password")
	}
	if attr(n, "aria-current") != "" && attr(n, "aria-current") != "false" {
		sn.attrs = append(sn.attrs, "current")
	}
	if role == "link" {
		sn.href = d.shortHref(attr(n, "href"))
	}
	switch n.Data {
	case "input", "textarea", "select":
		if v := inputValue(n); v != "" {
			sn.value, sn.hasValue = truncate(v, o.MaxTextLen), true
		}
	}

	if leafRoles[role] {
		if n.Data == "select" {
			for _, opt := range optionsOf(n) {
				on := &snapNode{role: "option", name: normWS(innerText(opt)), interactive: true, ref: d.refOf(opt)}
				if hasAttr(opt, "selected") {
					on.attrs = append(on.attrs, "selected")
				}
				if hasAttr(opt, "disabled") {
					on.attrs = append(on.attrs, "disabled")
				}
				sn.children = append(sn.children, on)
			}
		}
		return []*snapNode{sn}
	}

	kids := d.buildChildren(n, o, depth+1)
	textOnly := true
	for _, k := range kids {
		if k.role != "" {
			textOnly = false
			break
		}
	}
	switch {
	case nameFromContent[role] && sn.name != "" && textOnly:
		// name already carries the content
	case textOnly && compactRoles[role] && sn.name == "" && len(kids) > 0:
		var parts []string
		for _, k := range kids {
			parts = append(parts, k.text)
		}
		sn.value, sn.hasValue = truncate(strings.Join(parts, " "), o.MaxTextLen), true
	case textOnly && len(kids) == 1 && sn.name == "" && !landmarkRole(role):
		sn.value, sn.hasValue = kids[0].text, true
	default:
		sn.children = kids
	}
	// drop empty, nameless, non-interactive wrappers (e.g. an empty <ul>)
	if sn.name == "" && !sn.hasValue && len(sn.children) == 0 && !sn.interactive && role != "separator" {
		return nil
	}
	return []*snapNode{sn}
}

func landmarkRole(r string) bool {
	switch r {
	case "navigation", "main", "banner", "contentinfo", "complementary", "form", "region", "search", "article", "dialog", "list", "table", "row", "rowgroup", "group":
		return true
	}
	return false
}

func (d *Doc) buildChildren(n *html.Node, o *SnapshotOptions, depth int) []*snapNode {
	var out []*snapNode
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, d.buildSnap(c, o, depth)...)
	}
	return mergeText(out)
}

// mergeText joins adjacent bare text nodes into one line.
func mergeText(in []*snapNode) []*snapNode {
	if len(in) < 2 {
		return in
	}
	out := in[:0:0]
	for _, k := range in {
		if k.role == "" && len(out) > 0 && out[len(out)-1].role == "" {
			out[len(out)-1] = &snapNode{text: out[len(out)-1].text + " " + k.text}
			continue
		}
		out = append(out, k)
	}
	return out
}

// filterInteractive keeps ref-bearing nodes and the ancestors/headings that
// give them context.
func filterInteractive(nodes []*snapNode) []*snapNode {
	var out []*snapNode
	for _, n := range nodes {
		kids := filterInteractive(n.children)
		if n.ref != 0 || len(kids) > 0 || len(n.raw) > 0 {
			c := *n
			c.children = kids
			out = append(out, &c)
		}
	}
	return out
}

func countLines(nodes []*snapNode) int {
	n := 0
	for _, k := range nodes {
		n++
		n += countLines(k.children) + len(k.raw)
	}
	return n
}

// ---- serialisation ---------------------------------------------------------

func yamlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// yamlScalar quotes only when YAML would otherwise mis-parse the text.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	needs := strings.Contains(s, ": ") || strings.HasSuffix(s, ":") || strings.Contains(s, " #") ||
		strings.ContainsAny(s[:1], "-?:,[]{}#&*!|>'\"%@`") || strings.ContainsAny(s, "\n\"") ||
		s[0] == ' ' || s[len(s)-1] == ' '
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "~":
		needs = true
	}
	if needs {
		return yamlQuote(s)
	}
	return s
}

type snapWriter struct {
	b        strings.Builder
	max      int
	stopped  bool
	written  int
	skipped  int
	lines    int
	maxLines int
}

func (w *snapWriter) line(indent int, s string) bool {
	if w.stopped {
		w.skipped++
		return false
	}
	if w.b.Len()+len(s)+2*indent+1 > w.max {
		w.stopped = true
		w.skipped++
		return false
	}
	for i := 0; i < indent; i++ {
		w.b.WriteString("  ")
	}
	w.b.WriteString(s)
	w.b.WriteByte('\n')
	w.lines++
	return true
}

func (w *snapWriter) node(n *snapNode, indent int) {
	if n.role == "" {
		w.line(indent, "- text: "+yamlScalar(n.text))
		return
	}
	var sb strings.Builder
	sb.WriteString("- ")
	sb.WriteString(n.role)
	if n.name != "" {
		sb.WriteString(" ")
		sb.WriteString(yamlQuote(n.name))
	}
	attrs := append([]string(nil), n.attrs...)
	if n.ref != 0 {
		attrs = append(attrs, fmt.Sprintf("ref=e%d", n.ref))
	}
	if len(attrs) > 0 {
		sb.WriteString(" [" + strings.Join(attrs, " ") + "]")
	}
	hasBlock := n.href != "" || len(n.children) > 0 || len(n.raw) > 0
	switch {
	case n.hasValue && !hasBlock:
		sb.WriteString(": " + yamlScalar(n.value))
		w.line(indent, sb.String())
		return
	case hasBlock:
		sb.WriteString(":")
		if !w.line(indent, sb.String()) {
			w.skipped += countLines(n.children)
			return
		}
		if n.href != "" {
			w.line(indent+1, "- /url: "+yamlScalar(n.href))
		}
		if n.hasValue {
			w.line(indent+1, "- text: "+yamlScalar(n.value))
		}
		for _, c := range n.children {
			w.node(c, indent+1)
		}
		for _, ln := range n.raw {
			w.line(indent+1, ln)
		}
	default:
		w.line(indent, sb.String())
	}
}

// AriaSnapshotYAML renders the whole document with default options.
func (d *Doc) AriaSnapshotYAML() (string, error) {
	return d.AriaSnapshotV2(SnapshotOptions{})
}

// AriaSnapshotV2 renders the page as a Playwright-style accessibility tree in
// YAML. Every actionable node carries [ref=eN]; pass "eN" back to any action
// as its selector.
func (d *Doc) AriaSnapshotV2(o SnapshotOptions) (string, error) {
	o.defaults()
	root := d.Root
	if o.Selector != "" {
		infos, err := d.ResolveEngine(o.Selector)
		if err != nil {
			return "", err
		}
		if n := d.find(infos[0].BackendID); n != nil {
			root = n
		}
	}
	var nodes []*snapNode
	if root == d.Root {
		nodes = d.buildChildren(root, &o, 0)
	} else {
		nodes = d.buildSnap(root, &o, 0)
	}
	if o.InteractiveOnly {
		nodes = filterInteractive(nodes)
	}
	write := func(max int) *snapWriter {
		w := &snapWriter{max: max}
		for _, n := range nodes {
			w.node(n, 0)
		}
		return w
	}
	w := write(o.MaxChars)
	if w.stopped {
		// the footer (what was cut and an outline of the whole page) counts against the cap
		outline := headingOutline(nodes)
		if outline != "" {
			outline = "- outline (whole page): " + outline + "\n"
		}
		notice := "- …truncated: %d more nodes; pass selector= to scope or interactiveOnly=true\n"
		w = write(o.MaxChars - len(outline) - len(notice) - 8)
		w.b.WriteString(fmt.Sprintf(notice, w.skipped))
		w.b.WriteString(outline)
	}
	out := w.b.String()
	if strings.TrimSpace(out) == "" {
		if d.Root == nil {
			return "", fmt.Errorf("empty document")
		}
		// A page with no readable content at all: say so rather than erroring.
		if o.InteractiveOnly {
			return "- text: (no interactive elements on this page)\n", nil
		}
		return "- text: (page has no visible content; it may require JavaScript or be blank)\n", nil
	}
	return out, nil
}

// SnapshotInfos flattens the v2 tree into Info records (Page.snapshot JSON).
func (d *Doc) SnapshotInfos() []*Info {
	o := SnapshotOptions{}
	o.defaults()
	nodes := d.buildChildren(d.Root, &o, 0)
	var out []*Info
	var walk func([]*snapNode)
	walk = func(ns []*snapNode) {
		for _, n := range ns {
			if n.role != "" {
				var hn *html.Node
				if n.ref != 0 {
					hn = d.nodeByRef(n.ref)
				}
				info := &Info{Role: n.role, Text: n.name, Href: n.href, Visible: true}
				if info.Text == "" && n.hasValue {
					info.Text = n.value
				}
				if hn != nil {
					info.NodeID, info.BackendID = d.idOf(hn), d.idOf(hn)
					info.Ref = n.ref
					info.Name = hn.Data
					info.ID = attr(hn, "id")
					info.Type = attr(hn, "type")
					info.Disabled = hasAttr(hn, "disabled")
					info.Checked = hasAttr(hn, "checked")
					info.Focusable = isFocusable(hn)
				}
				out = append(out, info)
			}
			walk(n.children)
		}
	}
	walk(nodes)
	return out
}

// shortHref is a link's URL as the snapshot shows it: resolved against the document, then written as a path
// when it stays on the document's origin ("/wiki/Ada_Lovelace" rather than
// "https://en.wikipedia.org/wiki/Ada_Lovelace"): link-heavy pages are mostly URLs, and the origin is in the
// observation's header. Other origins, and anything that does not parse, are left as they are.
func (d *Doc) shortHref(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(strings.ToLower(raw), "javascript:") {
		return raw
	}
	base, err := url.Parse(d.base)
	if err != nil || base.Host == "" {
		return raw
	}
	u, err := base.Parse(raw)
	if err != nil {
		return raw
	}
	if u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil {
		return u.String()
	}
	out := u.EscapedPath()
	if out == "" {
		out = "/"
	}
	if u.RawQuery != "" || u.ForceQuery {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return out
}

const (
	outlineMaxHeadings = 40
	outlineMaxName     = 60
)

// headingOutline lists the headings of the whole tree, including the part a
// truncated snapshot cut off, so the agent knows what is there to scope to.
func headingOutline(nodes []*snapNode) string {
	var parts []string
	var walk func(ns []*snapNode)
	walk = func(ns []*snapNode) {
		for _, n := range ns {
			if len(parts) >= outlineMaxHeadings {
				return
			}
			if n.role == "heading" && n.name != "" {
				parts = append(parts, truncate(n.name, outlineMaxName))
			}
			walk(n.children)
		}
	}
	walk(nodes)
	return strings.Join(parts, " | ")
}
