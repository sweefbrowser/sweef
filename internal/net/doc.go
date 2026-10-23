// Package net is Sweef's document + network layer: HTML5 parsing into a
// stable node tree, CSS selector matching, form modelling, HAR records and
// route interception. No cgo, no deps beyond golang.org/x/net/html.
package net

import (
	"fmt"
	"io"
	"net/http"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Doc owns one parsed HTML tree. Node identity is a small integer issued by a
// per-document id table (see ids.go) that survives mutation; see idOf/find.
type Doc struct {
	Root *html.Node
	base string

	mu        sync.RWMutex
	listeners []func(m Mutation)

	idMu sync.RWMutex
	tab  *idTable

	// css holds compiled stylesheet rules and the computed-style cache
	// (see css.go); nil until first use.
	cssMu sync.Mutex
	css   *cssState

	// mutGen counts tree mutations; layout results are cached per value.
	mutGen atomic.Uint64
	// treeGen counts child-list (structural) mutations only; attribute and style changes do not bump it,
	// so live collections that depend only on tree shape (children, childNodes, getElementsByTagName) can be cached across them.
	treeGen atomic.Uint64
	// contentGen counts the mutations that can change what the page says: all but style attribute writes
	// (an animation loop rewrites a transform or an opacity every frame), see ContentGen
	contentGen     atomic.Uint64
	paintGen       atomic.Uint64                 // repaint-only changes (video frames), see TouchPaint
	styleLog       atomic.Pointer[StyleWriteLog] // set while a caller records style writes (RecordStyleWrites)
	layoutMu       sync.Mutex
	layoutCache    *Layout
	layoutCacheW   int
	layoutCacheGen uint64
	viewportW      atomic.Int64
	layoutProvider func(width int) *Layout
	provisionalOK  func() bool // Layout.Provisional; nil: never reuse a provisional layout

	// option selectedness kept apart from script-written `selected` attributes (optsel.go)
	optOnce sync.Once
	optSel  *optSelState
}

// SetProvisionalOK sets which callers may be answered with a cached provisional layout (Layout.Provisional).
func (d *Doc) SetProvisionalOK(fn func() bool) {
	d.layoutMu.Lock()
	d.provisionalOK = fn
	d.layoutMu.Unlock()
}

// SetLayoutProvider replaces the built-in flow layout with fn (the render
// engine). fn must not call Layout on the same Doc. Clears the cache.
func (d *Doc) SetLayoutProvider(fn func(width int) *Layout) {
	d.layoutMu.Lock()
	d.layoutProvider = fn
	d.layoutCache = nil
	d.layoutMu.Unlock()
}

// GeometryOf returns precise element geometry when a layout provider is set.
func (d *Doc) GeometryOf(backendID int64) (Geometry, bool) {
	n := d.find(backendID)
	if n == nil {
		return Geometry{}, false
	}
	l := d.Layout(d.LayoutWidth())
	if l.Geo == nil {
		return Geometry{}, false
	}
	return l.Geo(n)
}

func (d *Doc) layoutGen() uint64 { return d.mutGen.Load() }

// Mutation describes a DOM change in Playwright-mutation-like terms. The engine
// subscribes so it can re-run init hooks / notify agents without reparsing.
type Mutation struct {
	Op        string `json:"op"` // set-attr | remove-attr | set-value | inner-html | check | select
	BackendID int64  `json:"backendNodeId"`
	Key       string `json:"key,omitempty"`
	Value     string `json:"value,omitempty"`
}

// SetMutationListener installs (or clears, with nil) a callback fired on every
// tree mutation performed through this layer.
func (d *Doc) SetMutationListener(fn func(Mutation)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if fn == nil {
		d.listeners = nil
		return
	}
	d.listeners = []func(Mutation){fn}
}

func (d *Doc) note(m Mutation) {
	if ((m.Op == "set-attr" || m.Op == "remove-attr") && (m.Key == "style" || m.Key == "data-sw-anim")) || m.Op == "event" {
		if l := d.styleLog.Load(); l != nil && m.Key == "style" {
			l.add(m.BackendID)
		}
		d.invalidateStylesOnly()
	} else {
		d.invalidateStyles()
	}
	d.mu.RLock()
	ls := append([]func(Mutation){}, d.listeners...)
	d.mu.RUnlock()
	for _, f := range ls {
		f(m)
	}
}

func ParseDocument(src, base string) (*Doc, error) {
	hn, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return nil, err
	}
	d := &Doc{Root: hn, base: base, tab: newIDTable()}
	liveDocs.Add(1)
	runtime.AddCleanup(d, func(struct{}) { liveDocs.Add(-1) }, struct{}{})
	return d, nil
}

// liveDocs counts parsed documents not yet garbage collected.
var liveDocs atomic.Int64

// LiveDocs is the number of parsed documents the garbage collector has not
// reclaimed yet (after a GC: the ones still referenced). A count that keeps
// growing across navigations is a leak: Browser.stats reports it.
func LiveDocs() int64 { return liveDocs.Load() }

// ---- helpers on html.Node -------------------------------------------------

func attr(n *html.Node, k string) string {
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, k string) bool {
	for _, a := range n.Attr {
		if a.Key == k {
			return true
		}
	}
	return false
}

// ---- text / title ----------------------------------------------------------

func (d *Doc) Title() string {
	var t *html.Node
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "title" && n.Namespace == "" {
			t = n
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(d.Root)
	if t == nil {
		return ""
	}
	var sb strings.Builder
	for c := t.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
	}
	return StripCollapseASCIIWhitespace(sb.String())
}

// StripCollapseASCIIWhitespace is the HTML "strip and collapse ASCII
// whitespace" (what document.title returns): runs of tab, LF, FF, CR and
// space become one space, leading and trailing ones go. Other whitespace
// (no-break space and friends) is kept.
func StripCollapseASCIIWhitespace(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	space := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\t', '\n', '\f', '\r', ' ':
			space = true
		default:
			if space && sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			space = false
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// innerText: whitespace-normalised visible text under a node.
func innerText(n *html.Node) string {
	var sb strings.Builder
	var skip = map[string]bool{"script": true, "style": true, "noscript": true}
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.ElementNode && skip[x.Data] {
			return
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
	return normWS(sb.String())
}

func normWS(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

func (d *Doc) Text(sel string) (string, error) {
	n, err := d.queryOne(sel)
	if err != nil {
		return "", err
	}
	return innerText(n), nil
}

func (d *Doc) Serialize(w io.Writer) error {
	return html.Render(w, d.Root)
}

// Selector matching lives in selector.go.

func (d *Doc) nodeToInfo(n *html.Node) *Info {
	role := implicitRole(n)
	txt := innerText(n)
	if txt == "" {
		txt = attr(n, "aria-label")
		if txt == "" {
			txt = attr(n, "placeholder")
		}
		if txt == "" {
			txt = attr(n, "alt")
		}
	}
	return &Info{
		NodeID:    d.idOf(n),
		BackendID: d.idOf(n),
		Name:      n.Data,
		ID:        attr(n, "id"),
		Type:      attr(n, "type"),
		Role:      role,
		Text:      txt,
		Href:      attr(n, "href"),
		Visible:   d.effectivelyVisible(n),
		Disabled:  hasAttr(n, "disabled"),
		Checked:   hasAttr(n, "checked"),
		Focusable: isFocusable(n),
	}
}

// effectivelyVisible walks up the ancestor chain checking the hidden
// attribute, aria-hidden, computed display:none/visibility:hidden (inline
// style plus any stylesheets installed with SetStylesheets) and
// input[type=hidden].
func (d *Doc) effectivelyVisible(n *html.Node) bool {
	for x := n; x != nil && x.Type == html.ElementNode; x = x.Parent {
		if d.pruned(x) {
			return false
		}
	}
	return true
}

func isFocusable(n *html.Node) bool {
	switch n.Data {
	case "a", "button", "input", "select", "textarea":
		return !hasAttr(n, "disabled")
	}
	return hasAttr(n, "tabindex")
}

func implicitRole(n *html.Node) string { return computeRole(n) }

// ---- attribute / state accessors (Playwright locator API surface) ---------

// GetAttribute returns the value of attr k on the first node matching sel.
// For input/textarea controls, "value" falls back to the live current value.
func (d *Doc) GetAttribute(backendID int64, k string) (string, bool, error) {
	n := d.find(backendID)
	if n == nil {
		return "", false, fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	if v, ok := liveValue(n, k); ok {
		return v, true, nil
	}
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val, true, nil
		}
	}
	return "", false, nil
}

func liveValue(n *html.Node, k string) (string, bool) {
	if k != "value" || n.Type != html.ElementNode {
		return "", false
	}
	switch n.Data {
	case "input":
		return currentVal(n), true
	case "textarea":
		if n.FirstChild != nil {
			return n.FirstChild.Data, true
		}
		return "", true
	}
	return "", false
}

func (d *Doc) SetAttribute(backendID int64, k, v string) error {
	n := d.find(backendID)
	if n == nil {
		return fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	if val, ok := liveValue(n, k); ok && (n.Data == "input" || n.Data == "textarea") {
		_ = val
		return d.SetValue(backendID, v)
	}
	setAttr(n, k, v)
	d.note(Mutation{Op: "set-attr", BackendID: backendID, Key: k, Value: v})
	return nil
}

func (d *Doc) RemoveAttribute(backendID int64, k string) error {
	n := d.find(backendID)
	if n == nil {
		return fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	removeAttr(n, k)
	d.note(Mutation{Op: "remove-attr", BackendID: backendID, Key: k})
	return nil
}

func (d *Doc) InnerHTML(backendID int64) (string, error) {
	n := d.find(backendID)
	if n == nil {
		return "", fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&sb, c); err != nil {
			return "", err
		}
	}
	return sb.String(), nil
}

func (d *Doc) OuterHTML(backendID int64) (string, error) {
	n := d.find(backendID)
	if n == nil {
		return "", fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	var sb strings.Builder
	if err := html.Render(&sb, n); err != nil {
		return "", err
	}
	return sb.String(), nil
}

func (d *Doc) SetInnerHTML(backendID int64, frag string) error {
	d.treeGen.Add(1)
	n := d.find(backendID)
	if n == nil {
		return fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	clearChildren(n)
	sub, err := html.ParseFragment(strings.NewReader(frag), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
	if err != nil {
		return err
	}
	for _, s := range sub {
		n.AppendChild(s)
	}
	d.invalidateStyles()
	return nil
}

func (d *Doc) TextByBackend(backendID int64) (string, error) {
	n := d.find(backendID)
	if n == nil {
		return "", fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	return innerText(n), nil
}

// InputTarget reports whether a backend node is a fillable form control and
// what kind (input/textarea/select).
func (d *Doc) InputTarget(backendID int64) (tag, typ string, isSelect bool, err error) {
	n := d.find(backendID)
	if n == nil {
		return "", "", false, fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	switch n.Data {
	case "input", "textarea":
		return n.Data, attr(n, "type"), false, nil
	case "select":
		return n.Data, "", true, nil
	}
	return n.Data, "", false, fmt.Errorf("element <%s> cannot receive a value", n.Data)
}

// SelectOptions lists option values under a <select>.
func (d *Doc) SelectOptions(backendID int64) ([]string, error) {
	n := d.find(backendID)
	if n == nil || n.Data != "select" {
		return nil, fmt.Errorf("not a select element")
	}
	var out []string
	for o := n.FirstChild; o != nil; o = o.NextSibling {
		if o.Type == html.ElementNode && o.Data == "option" {
			v := attr(o, "value")
			if v == "" {
				v = innerText(o)
			}
			// Playwright semantics: return only *selected* options; if none
			// carry an explicit selected attribute the browser default is the
			// first option.
			if hasAttr(o, "selected") {
				out = append(out, v)
			}
		}
	}
	if len(out) == 0 {
		for o := n.FirstChild; o != nil; o = o.NextSibling {
			if o.Type == html.ElementNode && o.Data == "option" {
				v := attr(o, "value")
				if v == "" {
					v = innerText(o)
				}
				out = append(out, v)
				break
			}
		}
	}
	return out, nil
}

// SetValueByLabel matches <option> by visible text if no exact value match.
func (d *Doc) SelectOption(backendID int64, want string) error {
	n := d.find(backendID)
	if n == nil || n.Data != "select" {
		return fmt.Errorf("not a select element")
	}
	opts := optionsOf(n) // includes options inside <optgroup>
	var pick *html.Node
	for _, o := range opts { // an exact value wins over a label
		if v, ok := attrVal(o, "value"); ok && v == want {
			pick = o
			break
		}
	}
	if pick == nil {
		w := normWS(want)
		for _, o := range opts {
			if normWS(innerText(o)) == w || attr(o, "label") == want {
				pick = o
				break
			}
		}
	}
	if pick == nil {
		return fmt.Errorf("no option matches %q", want)
	}
	d.markOptionsDirty(n)
	for _, c := range opts {
		if c != pick {
			removeAttr(c, "selected")
		}
	}
	setAttr(pick, "selected", "")
	d.invalidateStyles()
	return nil
}

// CheckState toggles checkbox/radio state in the tree.
func (d *Doc) SetChecked(backendID int64, checked bool) error {
	n := d.find(backendID)
	if n == nil {
		return fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	defer d.invalidateStyles()
	switch attr(n, "type") {
	case "checkbox":
		if checked {
			setAttr(n, "checked", "")
		} else {
			removeAttr(n, "checked")
		}
		return nil
	case "radio":
		if !checked {
			return fmt.Errorf("radios cannot be unchecked by clicking")
		}
		group := attr(n, "name")
		for x := n.Parent; x != nil; x = x.Parent {
			if x.Type == html.ElementNode && x.Data == "form" {
				var walk func(*html.Node)
				walk = func(m *html.Node) {
					if m.Type == html.ElementNode && m.Data == "input" && attr(m, "type") == "radio" && attr(m, "name") == group {
						removeAttr(m, "checked")
					}
					for c := m.FirstChild; c != nil; c = c.NextSibling {
						walk(c)
					}
				}
				walk(x)
				break
			}
		}
		setAttr(n, "checked", "")
		return nil
	}
	return fmt.Errorf("element is not checkable (type=%q)", attr(n, "type"))
}

// IsVisible approximates CSS visibility without layout: hidden attribute,
// aria-hidden, computed display:none/visibility:hidden (inline style and
// stylesheet rules) anywhere up the chain, and input[type=hidden] are
// invisible.
func (d *Doc) IsVisible(backendID int64) (bool, error) {
	n := d.find(backendID)
	if n == nil {
		return false, fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	if !d.Attached(n) {
		return false, nil
	}
	return d.effectivelyVisible(n), nil
}

// LayoutWidth is the viewport width used for boxes and hit-testing.
func (d *Doc) LayoutWidth() int {
	if w := int(d.viewportW.Load()); w > 0 {
		return w
	}
	return 1280
}

// SetLayoutWidth sets the viewport width (from emulation) for layout.
func (d *Doc) SetLayoutWidth(w int) { d.viewportW.Store(int64(w)) }

// BoundingBox returns the element's box from the flow layout (see layout.go).
func (d *Doc) BoundingBox(backendID int64) ([4]int, error) {
	n := d.find(backendID)
	if n == nil {
		return [4]int{}, fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	if b, ok := d.Layout(d.LayoutWidth()).Boxes[n]; ok {
		return b, nil
	}
	return [4]int{0, 0, 0, 0}, nil
}

var blockTags = map[string]bool{
	"html": true, "body": true, "div": true, "p": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"ul": true, "ol": true, "li": true, "table": true, "tr": true, "thead": true, "tbody": true, "section": true, "article": true,
	"header": true, "footer": true, "nav": true, "main": true, "aside": true, "form": true, "fieldset": true, "pre": true,
	"blockquote": true, "hr": true, "dl": true, "dt": true, "dd": true, "figure": true, "details": true, "summary": true,
	"address": true, "figcaption": true, "legend": true, "menu": true, "search": true, "dialog": true, "caption": true,
}

// Count returns how many nodes match a selector (waitForSelector count rule).
func (d *Doc) Count(sel string) int { return len(d.queryAll(sel)) }

// Predicate evaluates a waiting condition against the current tree.
func (d *Doc) Predicate(kind, sel string, backendID int64) (bool, error) {
	switch kind {
	case "attached":
		if backendID != 0 {
			return d.find(backendID) != nil, nil
		}
		return d.Count(sel) > 0, nil
	case "detached":
		if backendID != 0 {
			return d.find(backendID) == nil, nil
		}
		return d.Count(sel) == 0, nil
	case "visible":
		id := backendID
		if id == 0 {
			n, err := d.queryOne(sel)
			if err != nil {
				return false, nil
			}
			id = d.idOf(n)
		}
		return d.IsVisible(id)
	case "hidden":
		vis, err := d.Predicate("visible", sel, backendID)
		if err != nil {
			return false, nil // treat vanished node as hidden
		}
		return !vis, nil
	case "enabled":
		id := backendID
		if id == 0 {
			n, err := d.queryOne(sel)
			if err != nil {
				return false, err
			}
			id = d.idOf(n)
		}
		n := d.find(id)
		if n == nil {
			return false, fmt.Errorf("no node with backendNodeId %d", backendID)
		}
		return !hasAttr(n, "disabled"), nil
	default:
		return false, fmt.Errorf("unknown predicate %q", kind)
	}
}

func (d *Doc) QuerySelector(sel string) (*Info, error) {
	n, err := d.queryOne(sel)
	if err != nil {
		return nil, err
	}
	return d.nodeToInfo(n), nil
}

func (d *Doc) QuerySelectorAll(sel string) ([]*Info, error) {
	ns := d.queryAll(sel)
	out := make([]*Info, 0, len(ns))
	for _, n := range ns {
		out = append(out, d.nodeToInfo(n))
	}
	return out, nil
}

// AriaSnapshot returns the flattened accessibility tree (roles/names/refs)
// used by Page.snapshot. It never errors on an empty page.
func (d *Doc) AriaSnapshot() ([]*Info, error) {
	if d.Root == nil {
		return nil, fmt.Errorf("empty document")
	}
	return d.SnapshotInfos(), nil
}

// SetValue writes value into input/textarea/select option matching.
func (d *Doc) SetValue(backendID int64, value string) error {
	d.treeGen.Add(1)
	target := d.find(backendID)
	if target == nil {
		return fmt.Errorf("node %d vanished", backendID)
	}
	switch target.Data {
	case "input", "textarea":
		setAttr(target, "value", value)
		if target.Data == "textarea" {
			clearChildren(target)
			target.AppendChild(&html.Node{Type: html.TextNode, Data: value})
		}
		d.note(Mutation{Op: "set-value", BackendID: backendID, Value: value})
	case "select":
		d.markOptionsDirty(target)
		for c := target.FirstChild; c != nil; c = c.NextSibling {
			if c.Data == "option" {
				if normWS(innerText(c)) == value || attr(c, "value") == value {
					setAttr(c, "selected", "")
				} else {
					removeAttr(c, "selected")
				}
			}
		}
		d.note(Mutation{Op: "select", BackendID: backendID, Value: value})
	default:
		return fmt.Errorf("cannot set value on <%s>", target.Data)
	}
	return nil
}

// AppendHTML parses frag and appends it as children of the element matching
// sel (Playwright locator.evaluateHandle insertAdjacentHTML('beforeend')).
func (d *Doc) AppendHTML(sel, frag string) error {
	d.treeGen.Add(1)
	n, err := d.queryOne(sel)
	if err != nil {
		return err
	}
	sub, err := html.ParseFragment(strings.NewReader(frag), n)
	if err != nil {
		return err
	}
	for _, s := range sub {
		n.AppendChild(s)
	}
	d.note(Mutation{Op: "inner-html", BackendID: d.idOf(n), Value: frag})
	return nil
}

// RemoveNode detaches the element matching sel from its parent.
func (d *Doc) RemoveNode(sel string) error {
	d.treeGen.Add(1)
	n, err := d.queryOne(sel)
	if err != nil {
		return err
	}
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
	d.note(Mutation{Op: "remove", BackendID: d.idOf(n)})
	return nil
}

// DispatchEvent is a semantic stand-in for page.dispatchEvent: Sweef's
// engine has no JS event listeners in pure-Go builds, so we translate the
// events agents actually rely on into tree mutations that keep state coherent.
func (d *Doc) DispatchEvent(backendID int64, typ string, detail string) error {
	n := d.find(backendID)
	if n == nil {
		return fmt.Errorf("no node with backendNodeId %d", backendID)
	}
	switch typ {
	case "input", "change":
		if n.Data == "input" || n.Data == "textarea" {
			if detail != "" {
				return d.SetValue(backendID, detail)
			}
			d.note(Mutation{Op: "event", BackendID: backendID, Key: typ})
		}
	case "focus", "blur", "click", "mousedown", "mouseup", "keydown", "keyup":
		d.note(Mutation{Op: "event", BackendID: backendID, Key: typ, Value: detail})
	default:
		d.note(Mutation{Op: "event", BackendID: backendID, Key: typ})
	}
	return nil
}

func setAttr(n *html.Node, k, v string) {
	for i := range n.Attr {
		if n.Attr[i].Key == k {
			n.Attr[i].Val = v
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: k, Val: v})
}

func removeAttr(n *html.Node, k string) {
	out := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Key != k {
			out = append(out, a)
		}
	}
	n.Attr = out
}

func clearChildren(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		n.RemoveChild(c)
		c = next
	}
}

// FormFor finds the enclosing form of sel and collects name=value pairs.
func (d *Doc) FormFor(sel string) (method, action string, values map[string]string, err error) {
	infos, err := d.ResolveEngine(sel)
	if err != nil {
		return
	}
	n := d.find(infos[0].BackendID)
	if n == nil {
		err = fmt.Errorf("no node matches %q", sel)
		return
	}
	return d.formForNode(n)
}

// FormForBackend is FormFor addressed by node id.
func (d *Doc) FormForBackend(id int64) (method, action string, values map[string]string, err error) {
	n := d.find(id)
	if n == nil {
		err = fmt.Errorf("no node with id %d", id)
		return
	}
	return d.formForNode(n)
}

func (d *Doc) formForNode(n *html.Node) (method, action string, values map[string]string, err error) {
	form := n
	if n.Type == html.ElementNode && n.Data != "form" {
		if f := attr(n, "form"); f != "" {
			if fn := findByID(d.Root, f); fn != nil {
				form = fn
			}
		}
	}
	for form != nil && !(form.Type == html.ElementNode && form.Data == "form") {
		form = form.Parent
	}
	values = map[string]string{}
	method = "get"
	action = ""
	if form != nil {
		method = attr(form, "method")
		if method == "" {
			method = "get"
		}
		action = attr(form, "action")
		collectControls(form, values)
		return
	}
	// no form: just this control if named
	name := attr(n, "name")
	if name != "" {
		values[name] = currentVal(n)
	}
	return
}

func collectControls(form *html.Node, values map[string]string) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			name := attr(n, "name")
			switch n.Data {
			case "input":
				if name != "" {
					switch attr(n, "type") {
					case "checkbox", "radio":
						if hasAttr(n, "checked") {
							values[name] = attr(n, "value")
							if values[name] == "" {
								values[name] = "on"
							}
						}
					case "submit", "button", "reset":
					default:
						values[name] = currentVal(n)
					}
				}
			case "textarea":
				if name != "" {
					values[name] = innerText(n)
				}
			case "select":
				if name != "" {
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						if c.Data == "option" && hasAttr(c, "selected") {
							v := attr(c, "value")
							if v == "" {
								v = innerText(c)
							}
							values[name] = v
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(form)
}

func currentVal(n *html.Node) string {
	v := attr(n, "value")
	return v
}

// Info mirrors engine.NodeInfo without import cycle (engine converts).
type Info struct {
	NodeID    int64  `json:"nodeId"`
	BackendID int64  `json:"backendNodeId"`
	Ref       int64  `json:"ref,omitempty"` // snapshot ref (eN), 0 if never listed
	Name      string `json:"name"`
	ID        string `json:"id,omitempty"`
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Text      string `json:"text,omitempty"`
	Href      string `json:"href,omitempty"`
	Visible   bool   `json:"visible"`
	Disabled  bool   `json:"disabled"`
	Checked   bool   `json:"checked,omitempty"`
	Focusable bool   `json:"focusable"`
}

// CompiledScripts is mirrored here to avoid engine->net cycle noise.
type Script struct {
	Source string
	Name   string
}

func (d *Doc) Scripts() []Script {
	var out []Script
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "script" {
			src := attr(n, "src")
			if src != "" {
				out = append(out, Script{Name: src})
			} else if n.FirstChild != nil {
				out = append(out, Script{Source: n.FirstChild.Data, Name: "<inline>"})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(d.Root)
	return out
}

// RenderText draws a crude but useful text-mode page render.
func (d *Doc) RenderText(width int) string {
	var lines []string
	wrap := func(prefix, s string) {
		s = normWS(s)
		if s == "" {
			return
		}
		for len(s) > width {
			cut := width
			if i := strings.LastIndexByte(s[:cut], ' '); i > 0 {
				cut = i
			}
			lines = append(lines, prefix+s[:cut])
			s = strings.TrimLeft(s[cut:], " ")
		}
		lines = append(lines, prefix+s)
	}
	var walk func(*html.Node, int)
	walk = func(n *html.Node, depth int) {
		ind := strings.Repeat("  ", depth)
		switch {
		case n.Type == html.ElementNode && strings.HasPrefix(n.Data, "h"):
			wrap(ind+"# ", innerText(n))
			return
		case n.Type == html.ElementNode && n.Data == "li":
			wrap(ind+"- ", innerText(n))
			return
		case n.Type == html.ElementNode && n.Data == "a" && hasAttr(n, "href"):
			wrap(ind+"→ ", innerText(n)+" ("+attr(n, "href")+")")
			return
		case n.Type == html.ElementNode && (n.Data == "input" || n.Data == "textarea" || n.Data == "select"):
			label := attr(n, "placeholder")
			if label == "" {
				label = attr(n, "name")
			}
			wrap(ind+"[", fmt.Sprintf("%s: %s]", n.Data, label))
			return
		case n.Type == html.ElementNode && n.Data == "button":
			wrap(ind+"( ", innerText(n)+" )")
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, depth)
		}
	}
	walk(d.Root, 0)
	return strings.Join(lines, "\n")
}

// ---- network records + routes ---------------------------------------------

type RequestRecord struct {
	Method          string            `json:"request.method"`
	URL             string            `json:"request.url"`
	FinalURL        string            `json:"response.url,omitempty"`
	Status          int               `json:"response.status"`
	Headers         map[string]string `json:"request.headers,omitempty"`
	ResponseHeaders map[string]string `json:"response.headers,omitempty"`
	DurationMs      int64             `json:"time"`
}

// Route implements Playwright-style interception: glob patterns matched on URL
// path; handler returns (status, body, handled).
type Route struct {
	Pattern string
	Handler func(*http.Request) (int, string, bool)
}

func NewRoute(glob string, h func(*http.Request) (int, string, bool)) *Route {
	return &Route{Pattern: glob, Handler: h}
}

func (r *Route) Match(u string) bool {
	parsed, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return false
	}
	ok, _ := path.Match(r.Pattern, parsed.URL.Path)
	if ok {
		return true
	}
	return strings.Contains(u, strings.Trim(r.Pattern, "*"))
}

// MatchRoute runs a route's handler against a request if the URL matches.
func MatchRoute(r *Route, req *http.Request) (int, string, bool) {
	if r == nil || r.Handler == nil || !r.Match(req.URL.String()) {
		return 0, "", false
	}
	code, body, handled := r.Handler(req)
	return code, body, handled
}

// InfoByBackend returns the serialisable node info for a backend id, or nil.
func (d *Doc) InfoByBackend(backendID int64) *Info {
	n := d.find(backendID)
	if n == nil {
		return nil
	}
	return d.nodeToInfo(n)
}

// BackendForSelector resolves a selector to its stable backend node id.
func (d *Doc) BackendForSelector(sel string) (int64, error) {
	n, err := d.queryOne(sel)
	if err != nil {
		return 0, err
	}
	return d.idOf(n), nil
}

// NthForSelector resolves the nth (0-based) match of a selector.
func (d *Doc) NthForSelector(sel string, nth int) (int64, error) {
	all := d.queryAll(sel)
	if nth < 0 || nth >= len(all) {
		return 0, fmt.Errorf("no node matches %q at index %d", sel, nth)
	}
	return d.idOf(all[nth]), nil
}

// ---- Playwright-style locator engines --------------------------------------
// These extend plain CSS with the semantic selectors agents use most. They
// run as post-filters over queryAll results, so they cost zero extra memory.

// ByText returns nodes whose normalised inner text equals (or contains) s.
func (d *Doc) ByText(s string, exact bool) []*Info {
	var all []*html.Node
	for _, n := range d.queryAll("*") {
		t := innerText(n)
		if (exact && t == s) || (!exact && strings.Contains(strings.ToLower(t), strings.ToLower(s))) {
			all = append(all, n)
		}
	}
	// Like Playwright: keep only the deepest matching elements so a text
	// locator resolves to the anchor/leaf, not every ancestor.
	var out []*Info
	for _, n := range all {
		hasMatchChild := false
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			for _, m := range all {
				if htmlSubtreeContains(c, m) {
					hasMatchChild = true
					break
				}
			}
			if hasMatchChild {
				break
			}
		}
		if !hasMatchChild {
			out = append(out, d.nodeToInfo(n))
		}
	}
	return out
}

func htmlSubtreeContains(root, n *html.Node) bool {
	for x := n; x != nil; x = x.Parent {
		if x == root {
			return true
		}
	}
	return false
}

// ByRole returns nodes whose computed ARIA role matches role, optionally
// filtered by accessible name (text/aria-label).
func (d *Doc) ByRole(role, name string) []*Info {
	var out []*Info
	for _, n := range d.queryAll("*") {
		r := computeRole(n)
		if r != role {
			continue
		}
		if name != "" {
			// Match against the WAI-ARIA accessible name (aria-label,
			// labelledby, <label>, placeholder, title, text content...),
			// falling back to visible text — like Playwright's getByRole({name}).
			an := accessibleName(n)
			if an == "" {
				an = innerText(n)
			}
			want := strings.ToLower(strings.TrimSpace(name))
			got := strings.ToLower(strings.TrimSpace(an))
			exact := false
			if strings.HasPrefix(name, "i:") {
				// no-op, keep case-insensitive always for now
			}
			if exact {
				if got != want {
					continue
				}
			} else if !strings.Contains(got, want) && got != want {
				continue
			}
		}
		out = append(out, d.nodeToInfo(n))
	}
	return out
}

// ByLabel matches form controls by their associated <label for=>, aria-label,
// placeholder or title.
func (d *Doc) ByLabel(label string) []*Info {
	var out []*Info
	want := strings.ToLower(strings.TrimSpace(label))
	for _, n := range d.queryAll("input, textarea, select, button, meter, progress, output, [role=checkbox], [role=switch], [role=textbox], [role=combobox], [role=slider]") {
		if n.Data == "input" && strings.EqualFold(attr(n, "type"), "hidden") {
			continue
		}
		// Candidate names: the accessible name (aria-label, aria-labelledby,
		// <label for>, wrapping <label>, ...) plus placeholder and title.
		cands := []string{accessibleName(n), labelForControl(n), attr(n, "aria-label"), attr(n, "placeholder"), attr(n, "title")}
		for _, c := range cands {
			if c != "" && strings.Contains(strings.ToLower(c), want) {
				out = append(out, d.nodeToInfo(n))
				break
			}
		}
	}
	return out
}

// ByPlaceholder matches controls by placeholder text.
func (d *Doc) ByPlaceholder(p string) []*Info {
	var out []*Info
	for _, n := range d.queryAll("input, textarea") {
		if strings.Contains(strings.ToLower(attr(n, "placeholder")), strings.ToLower(p)) {
			out = append(out, d.nodeToInfo(n))
		}
	}
	return out
}

// ByAlt matches images by alt text.
func (d *Doc) ByAlt(a string) []*Info {
	var out []*Info
	for _, n := range d.queryAll("img") {
		if strings.Contains(strings.ToLower(attr(n, "alt")), strings.ToLower(a)) {
			out = append(out, d.nodeToInfo(n))
		}
	}
	return out
}

// ByTestId matches data-testid / data-test / test-id attributes.
func (d *Doc) ByTestId(id string) []*Info {
	var out []*Info
	for _, n := range d.queryAll("*") {
		v := attr(n, "data-testid")
		if v == "" {
			v = attr(n, "data-test")
		}
		if v == "" {
			v = attr(n, "test-id")
		}
		if v == id {
			out = append(out, d.nodeToInfo(n))
		}
	}
	return out
}

// ResolveEngine parses Playwright selector syntax: css default, plus
// text=..., text="exact", role=button[name="Ok"], label=..., placeholder=...,
// alt=..., testid=..., and engine prefixes css=/xpath-less variants.
func (d *Doc) ResolveEngine(sel string) ([]*Info, error) {
	if parts := splitChain(sel); len(parts) > 1 {
		return d.resolveChain(sel, parts)
	}
	return d.resolveEngineOne(sel)
}

func (d *Doc) resolveEngineOne(sel string) ([]*Info, error) {
	s := strings.TrimSpace(sel)
	// ref handles from AriaSnapshotYAML: "e42", "ref=e42", or "backend://e42"
	refStr := ""
	switch {
	case strings.HasPrefix(s, "backend://"):
		refStr = strings.TrimPrefix(s, "backend://")
	case strings.HasPrefix(s, "ref="):
		refStr = strings.TrimPrefix(s, "ref=")
	case len(s) > 1 && s[0] == 'e' && s[1] >= '0' && s[1] <= '9':
		refStr = s[1:]
	}
	if refStr != "" {
		var id int64
		if _, err := fmt.Sscanf(refStr, "%d", &id); err == nil {
			switch d.RefState(id) {
			case "attached":
				return []*Info{d.nodeToInfo(d.nodeByRef(id))}, nil
			case "detached":
				return nil, fmt.Errorf("ref e%d is detached from the document; take a new snapshot", id)
			}
			return nil, fmt.Errorf("ref e%d is not in any snapshot of this page; take a new snapshot", id)
		}
	}
	switch {
	case strings.HasPrefix(s, "text=is:"):
		return d.ByText(strings.TrimPrefix(s, "text=is:"), false), nil
	case strings.HasPrefix(s, "text=:"):
		return d.ByText(strings.TrimPrefix(s, "text=:"), true), nil
	case strings.HasPrefix(s, "text="):
		return d.ByText(strings.TrimPrefix(s, "text="), false), nil
	case strings.HasPrefix(s, "role="):
		body := strings.TrimPrefix(s, "role=")
		role, name := body, ""
		if i := strings.Index(body, "[name="); i >= 0 {
			role = body[:i]
			name = strings.Trim(body[i+len("[name="):], `"]`)
		}
		return d.ByRole(role, name), nil
	case strings.HasPrefix(s, "label="):
		return d.ByLabel(strings.TrimPrefix(s, "label=")), nil
	case strings.HasPrefix(s, "placeholder="):
		return d.ByPlaceholder(strings.TrimPrefix(s, "placeholder=")), nil
	case strings.HasPrefix(s, "alt="):
		return d.ByAlt(strings.TrimPrefix(s, "alt=")), nil
	case strings.HasPrefix(s, "testid="):
		return d.ByTestId(strings.TrimPrefix(s, "testid=")), nil
	case strings.HasPrefix(s, "css="):
		s = strings.TrimPrefix(s, "css=")
	}
	var out []*Info
	for _, n := range d.queryAll(s) {
		out = append(out, d.nodeToInfo(n))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no node matches %q", sel)
	}
	return out, nil
}

// BackendByEngine resolves a Playwright-style selector to the first backend id.
func (d *Doc) BackendByEngine(sel string) (int64, string, error) {
	infos, err := d.ResolveEngine(sel)
	if err != nil {
		return 0, "", err
	}
	return infos[0].BackendID, sel, nil
}

// ElementFromPoint returns the innermost element whose layout box contains
// (x, y), or 0. Smaller boxes win, then later document order (paint order).
func (d *Doc) ElementFromPoint(x, y int) int64 {
	l := d.Layout(d.LayoutWidth())
	if l.Hit != nil {
		if n := l.Hit(float64(x), float64(y)); n != nil {
			return d.idOf(n)
		}
		return 0
	}
	var best *html.Node
	bestArea := -1
	for n, b := range l.Boxes {
		if n.Data == "html" || n.Data == "body" {
			continue
		}
		if x >= b[0] && x < b[0]+b[2] && y >= b[1] && y < b[1]+b[3] {
			area := b[2] * b[3]
			if bestArea < 0 || area < bestArea || (area == bestArea && d.idOf(n) > d.idOf(best)) {
				best, bestArea = n, area
			}
		}
	}
	if best == nil {
		return 0
	}
	return d.idOf(best)
}

// TreeGen returns the structural mutation counter (see treeGen).
func (d *Doc) TreeGen() uint64 { return d.treeGen.Load() }
