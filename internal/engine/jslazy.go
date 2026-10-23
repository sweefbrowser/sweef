package engine

import (
	"strings"

	"golang.org/x/net/html"

	fnet "github.com/sweefbrowser/sweef/internal/net"
)

// jslazy.go: Options.JSMode "lazy".
//
// Reading a server-rendered page does not need its scripts: the headings, text, links and form fields are in the
// markup the server sent, and the hydration bundle of a React or Next.js page only attaches handlers to them (MDN, GitHub, PyPI:
// two seconds of QuickJS and a forced layout for a snapshot that is the same without it). In lazy mode a headless
// navigation of such a page installs the document, runs the scripts that are already downloaded and quick (the
// prepaint step, as a window would), and returns. The rest of the load (remaining scripts, DOMContentLoaded, load,
// the settle pump) is held, exactly as a progressive load holds it, until something needs the page to be alive:
// an action on it, an evaluate, a wait, a screenshot, a form submit. wakeJS then runs it out the way a blocking
// load would have, and the action goes on from the page the user would have had.
//
// A page whose markup carries little content (a client-rendered shell: an empty root element) is not deferred:
// there is nothing to read until the scripts have run, so they run at once.
//
// Reads (snapshot, get text, find, console, network) never wake the page. They describe the page as served.

const (
	lazyMinChars       = 400 // visible characters of text in the markup for it to count as content
	lazyMinInteractive = 3   // links, buttons and fields
)

// wakeJS runs the scripts a lazy load held back, once, and returns when the page has loaded the way an eager
// navigation would have left it. A tab that was not deferred returns at once.
func (t *Tab) wakeJS() {
	if t == nil || !t.lazyJS.Load() {
		return
	}
	if t.lazyJS.CompareAndSwap(true, false) {
		t.tmark("js.wake", "")
		t.LoadFinish()
	}
}

// JSDeferred reports whether the current document's scripts are still held back by a lazy load.
func (t *Tab) JSDeferred() bool { return t.lazyJS.Load() }

// serverRendered reports whether the document's own markup carries enough content to be read without running
// its scripts: at least lazyMinChars of text outside script, style, noscript and template, and lazyMinInteractive
// interactive elements.
func serverRendered(doc *fnet.Doc) bool {
	if doc == nil || doc.Root == nil {
		return false
	}
	chars, inter := 0, 0
	var walk func(n *html.Node, inBody bool) bool
	walk = func(n *html.Node, inBody bool) bool {
		switch n.Type {
		case html.TextNode:
			if inBody {
				chars += len(strings.TrimSpace(n.Data))
			}
		case html.ElementNode:
			switch n.Data {
			case "script", "style", "noscript", "template", "head", "svg":
				return false
			case "body":
				inBody = true
			case "a":
				for _, a := range n.Attr {
					if a.Key == "href" {
						inter++
						break
					}
				}
			case "button", "select", "textarea":
				inter++
			case "input":
				hidden := false
				for _, a := range n.Attr {
					if a.Key == "type" && strings.EqualFold(a.Val, "hidden") {
						hidden = true
					}
				}
				if !hidden {
					inter++
				}
			}
		}
		if chars >= lazyMinChars && inter >= lazyMinInteractive {
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c, inBody) {
				return true
			}
		}
		return false
	}
	return walk(doc.Root, false)
}
