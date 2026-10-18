package engine

// preload.go: the preload scanner. While the document body is still arriving, a
// second reader tokenizes what has come in and starts the stylesheet and script
// requests it finds, so they overlap the rest of the download instead of
// starting after the whole document was received and parsed (a browser does the
// same). The requests join the navigation's request table (fetchres.go): the
// parser's own pass finds them in flight or already read, and nothing is fetched
// twice.

import (
	"io"
	"strings"
	"sync"

	"golang.org/x/net/html"
)

type preloadScanner struct {
	pw   *io.PipeWriter
	done chan struct{}
}

// startPreloadScan returns a scanner fed with Write; Close ends it. base is the document's URL.
func (t *Tab) startPreloadScan(base string) *preloadScanner {
	pr, pw := io.Pipe()
	s := &preloadScanner{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer io.Copy(io.Discard, pr) // never leave the writer blocked
		t.scanPreloads(pr, base)
	}()
	return s
}

func (s *preloadScanner) Write(p []byte) { _, _ = s.pw.Write(p) }

func (s *preloadScanner) Close() {
	_ = s.pw.Close()
	<-s.done
}

// scanPreloads tokenizes r and starts the fetches. It looks at tags only, never at text.
func (t *Tab) scanPreloads(r io.Reader, base string) {
	z := html.NewTokenizer(r)
	pol := t.policy()
	js := !t.eng.opts.DisableJS && pol.ScriptMaxBytes == 0 // a script budget reads scripts in document order, see applyStyles
	baseSet := false
	seen := map[string]bool{}
	// The static imports of a module script start downloading as soon as its source is in, and theirs as those
	// arrive: a browser fetches a module graph from the parser's discovery of the entry module on, while the
	// engine's loader (jsVM.prefetchModuleDeps) only began when the module ran, behind every script before it.
	// Bare specifiers need the import map, which only the page's VM knows: it picks those up when the module runs.
	var graphMu sync.Mutex
	graphSeen := map[string]bool{}
	var graph func(modURL, src string)
	graph = func(modURL, src string) {
		for _, spec := range staticImportSpecs(src) {
			if !moduleLikePath(spec) {
				continue
			}
			var abs string
			switch {
			case strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://"):
				abs = spec
			case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || strings.HasPrefix(spec, "/"):
				u, err := resolveURL(modURL, spec)
				if err != nil {
					continue
				}
				abs = u
			default:
				continue
			}
			if !(strings.HasPrefix(abs, "http://") || strings.HasPrefix(abs, "https://")) || (pol.BlockThirdPartyScripts && thirdParty(base, abs)) {
				continue
			}
			graphMu.Lock()
			dup := graphSeen[abs] || len(graphSeen) >= maxPrefetchedModules
			graphSeen[abs] = true
			graphMu.Unlock()
			if dup {
				continue
			}
			go func() {
				t.preloadSem <- struct{}{}
				body, status, err := t.fetchRes(abs, fetchOpts{kind: KindScript, prefetch: true})
				<-t.preloadSem
				if err == nil && status < 400 {
					t.eng.pc.submitModule(abs, body)
					graph(abs, body)
				}
			}()
		}
	}
	start := func(kind DataKind, ref string, module bool) {
		abs, err := resolveURL(base, strings.TrimSpace(ref))
		if err != nil || !(strings.HasPrefix(abs, "http://") || strings.HasPrefix(abs, "https://")) || seen[abs] {
			return
		}
		seen[abs] = true
		if kind == KindScript && pol.BlockThirdPartyScripts && thirdParty(base, abs) {
			return
		}
		t.tmark("preload."+string(kind), abs)
		go func() {
			t.preloadSem <- struct{}{}
			defer func() { <-t.preloadSem }()
			body, status, err := t.fetchRes(abs, fetchOpts{kind: kind, page: base, prefetch: true})
			if kind == KindScript && err == nil && status < 400 { // compiled on another core while the page loads
				if module {
					t.eng.pc.submitModule(abs, body)
					graph(abs, body)
				} else {
					t.eng.pc.submitClassic(body)
				}
			}
		}()
	}
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if !hasAttr {
			continue
		}
		tag := string(name)
		if tag != "link" && tag != "script" && tag != "base" {
			continue
		}
		attrs := map[string]string{}
		for {
			k, v, more := z.TagAttr()
			if _, dup := attrs[string(k)]; !dup { // the first of a repeated attribute wins
				attrs[string(k)] = string(v)
			}
			if !more {
				break
			}
		}
		switch tag {
		case "base":
			if h := strings.TrimSpace(attrs["href"]); h != "" && !baseSet {
				baseSet = true
				if abs, err := resolveURL(base, h); err == nil {
					base = abs
				}
			}
		case "link":
			isSheet, alternate := false, false
			for _, tok := range strings.Fields(strings.ToLower(attrs["rel"])) {
				switch tok {
				case "stylesheet":
					isSheet = true
				case "alternate":
					alternate = true
				}
			}
			if isSheet && !alternate && attrs["href"] != "" && !strings.EqualFold(strings.TrimSpace(attrs["media"]), "print") {
				if _, dis := attrs["disabled"]; !dis {
					start(KindStyle, attrs["href"], false)
				}
			}
		case "script":
			if js && attrs["src"] != "" {
				if _, nm := attrs["nomodule"]; !nm && isJavaScriptType(attrs["type"]) {
					start(KindScript, attrs["src"], strings.EqualFold(strings.TrimSpace(attrs["type"]), "module"))
				}
			}
		}
	}
}
