package v8shim

// Stretch slice: boot the engine's real dom_prelude.js on V8 against a tiny
// Go-side DOM for example.com. Read-only use of internal/engine; skipped when
// the file is not present (e.g. when this module is checked out alone).

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const preludePath = "../../../internal/engine/dom_prelude.js"

type fnode struct {
	typ      int64
	tag      string
	text     string
	parent   int64
	children []int64
	attrs    map[string]string
}

type fakeDoc struct {
	nodes  map[int64]*fnode
	next   int64
	called map[string]int
}

func newExampleDoc() *fakeDoc {
	d := &fakeDoc{nodes: map[int64]*fnode{}, next: 1, called: map[string]int{}}
	add := func(parent int64, n *fnode) int64 {
		id := d.next
		d.next++
		n.parent = parent
		d.nodes[id] = n
		if parent != 0 {
			d.nodes[parent].children = append(d.nodes[parent].children, id)
		}
		return id
	}
	doc := add(0, &fnode{typ: 9})
	html := add(doc, &fnode{typ: 1, tag: "html"})
	head := add(html, &fnode{typ: 1, tag: "head"})
	title := add(head, &fnode{typ: 1, tag: "title"})
	add(title, &fnode{typ: 3, text: "Example Domain"})
	body := add(html, &fnode{typ: 1, tag: "body"})
	div := add(body, &fnode{typ: 1, tag: "div"})
	h1 := add(div, &fnode{typ: 1, tag: "h1"})
	add(h1, &fnode{typ: 3, text: "Example Domain"})
	p := add(div, &fnode{typ: 1, tag: "p"})
	add(p, &fnode{typ: 3, text: "This domain is for use in documentation examples."})
	p2 := add(div, &fnode{typ: 1, tag: "p"})
	a := add(p2, &fnode{typ: 1, tag: "a", attrs: map[string]string{"href": "https://iana.org/domains/example"}})
	add(a, &fnode{typ: 3, text: "Learn more"})
	return d
}

func (d *fakeDoc) node(c *Call, i int) *fnode { return d.nodes[c.Int(i)] }

func ids(v []int64) []int32 {
	out := make([]int32, len(v))
	for i, x := range v {
		out[i] = int32(x)
	}
	return out
}

func (d *fakeDoc) sibling(id int64, delta int) int64 {
	n := d.nodes[id]
	if n == nil || n.parent == 0 {
		return 0
	}
	sib := d.nodes[n.parent].children
	for i, c := range sib {
		if c == id && i+delta >= 0 && i+delta < len(sib) {
			return sib[i+delta]
		}
	}
	return 0
}

func TestPreludeBootsOnV8(t *testing.T) {
	src, err := os.ReadFile(preludePath)
	if err != nil {
		t.Skip("engine prelude not available:", err)
	}
	_, c := newCtx(t, Options{})
	d := newExampleDoc()
	names := regexp.MustCompile(`\bsw\.([A-Za-z0-9_]+)`).FindAllStringSubmatch(string(src), -1)
	seen := map[string]bool{}
	impl := map[string]HostFunc{
		"docId":       func(call *Call) { call.RetInt(1) },
		"nodeType":    func(call *Call) { call.RetInt(d.node(call, 0).typ) },
		"tagName":     func(call *Call) { call.RetString(d.node(call, 0).tag) },
		"parent":      func(call *Call) { call.RetInt(d.node(call, 0).parent) },
		"children":    func(call *Call) { call.RetInt32s(ids(d.node(call, 0).children)) },
		"nextSibling": func(call *Call) { call.RetInt(d.sibling(call.Int(0), 1)) },
		"prevSibling": func(call *Call) { call.RetInt(d.sibling(call.Int(0), -1)) },
		"firstChild": func(call *Call) {
			if ch := d.node(call, 0).children; len(ch) > 0 {
				call.RetInt(ch[0])
			} else {
				call.RetInt(0)
			}
		},
		"lastChild": func(call *Call) {
			if ch := d.node(call, 0).children; len(ch) > 0 {
				call.RetInt(ch[len(ch)-1])
			} else {
				call.RetInt(0)
			}
		},
		"textContent": func(call *Call) {
			var sb strings.Builder
			var walk func(id int64)
			walk = func(id int64) {
				n := d.nodes[id]
				if n.typ == 3 {
					sb.WriteString(n.text)
				}
				for _, ch := range n.children {
					walk(ch)
				}
			}
			walk(call.Int(0))
			call.RetString(sb.String())
		},
		"treeGen":  func(call *Call) { call.RetInt(1) },
		"mutation": func(call *Call) {},
		"createElement": func(call *Call) {
			id := d.next
			d.next++
			d.nodes[id] = &fnode{typ: 1, tag: strings.ToLower(call.Str(0))}
			call.RetInt(id)
		},
		"elementsByTag": func(call *Call) {
			var out []int64
			var walk func(id int64)
			walk = func(id int64) {
				for _, ch := range d.nodes[id].children {
					n := d.nodes[ch]
					if n.typ == 1 && (call.Str(1) == "*" || strings.EqualFold(n.tag, call.Str(1))) {
						out = append(out, ch)
					}
					walk(ch)
				}
			}
			root := call.Int(0)
			if root == 0 {
				root = 1
			}
			walk(root)
			call.RetInt32s(ids(out))
		},
		"nodeValue": func(call *Call) { call.RetString(d.node(call, 0).text) },
		"getAttr": func(call *Call) {
			if v, ok := d.node(call, 0).attrs[call.Str(1)]; ok {
				call.RetString(v)
			} else {
				call.RetNull()
			}
		},
		"hasAttr": func(call *Call) { _, ok := d.node(call, 0).attrs[call.Str(1)]; call.RetBool(ok) },
	}
	if _, err := c.Eval("globalThis.__sw = {};", "host.js"); err != nil {
		t.Fatal(err)
	}
	for _, m := range names {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		fn, ok := impl[name]
		if !ok {
			n := name
			fn = func(call *Call) { d.called[n]++ } // unimplemented host call: returns undefined
		}
		if err := c.Register("__sw."+name, fn); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Eval(string(src), "dom_prelude.js"); err != nil {
		t.Fatalf("prelude failed to boot under V8: %v", err)
	}
	if _, err := c.Eval("__swSetDocument('https://example.com/', 'complete')", "bind.js"); err != nil {
		t.Fatal(err)
	}
	probe := func(expr string) string {
		v, err := c.Eval(expr, "probe.js")
		if err != nil {
			return "ERR " + strings.SplitN(err.Error(), "\n", 2)[0]
		}
		return v.String()
	}
	checks := []struct{ expr, want string }{
		{"typeof window + ',' + typeof document + ',' + typeof Element + ',' + typeof MutationObserver + ',' + typeof fetch", "object,object,function,function,function"},
		{"document.documentElement.tagName", "HTML"},
		{"document.body.tagName", "BODY"},
		{"document.body.children.length", "1"},
		{"document.body.firstElementChild.firstElementChild.textContent", "Example Domain"},
		{"document.getElementsByTagName('a').length", "1"},
		{"(function(){var e=document.createElement('section'); e.setAttribute('x','1'); return e.tagName})()", "SECTION"},
	}
	for _, ck := range checks {
		if got := probe(ck.expr); got != ck.want {
			t.Errorf("%s = %q, want %q", ck.expr, got, ck.want)
		}
	}
	var unimpl []string
	for k, n := range d.called {
		unimpl = append(unimpl, k+"="+strings.Repeat("", 0)+string(rune('0'+n%10)))
	}
	sort.Strings(unimpl)
	t.Logf("prelude: %d bytes booted; %d host names referenced; unimplemented host calls actually made: %v", len(src), len(seen), unimpl)
}
