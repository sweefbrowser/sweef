package engine

// platform.go: the web-platform layer that sits on top of the DOM prelude
// (docs/web-platform.md lists every API and its status).
//
// Start-up cost matters (the prelude is loaded into every VM), so this file
// registers only a small bootstrap, platformjs/boot.js. The bootstrap installs
// lazy accessors for every API in this layer; the implementation of a module
// (platformjs/<name>.js) is fetched through __sw.platformSrc and evaluated the
// first time one of its names is touched. A page that never uses XPath,
// URLPattern, the Navigation API, OPFS... pays one accessor per name and no
// parsing.

import (
	"embed"
	"fmt"
	"strings"

	"github.com/sweefbrowser/sweef/internal/zpack"
)

// The platform modules are embedded deflated (platformjs/NAME.js.z, made by `go generate`, see internal/zpack) and
// inflated when a page asks for one.
//
//go:embed platformjs/*.js.z
var platformFS embed.FS

func platformModule(name string) string {
	if strings.ContainsAny(name, "/.\\") {
		return ""
	}
	b, err := platformFS.ReadFile("platformjs/" + name + ".js.z")
	if err != nil {
		return ""
	}
	return zpack.String(string(b))
}

func init() {
	b, err := platformFS.ReadFile("platformjs/boot.js.z")
	if err != nil {
		panic(fmt.Sprintf("platform: %v", err))
	}
	registerPreludeZ("platform", string(b))
	registerHostExtension("platform", func(j *jsVM) map[string]hostFn {
		f := map[string]hostFn{
			"platformSrc": func(a []any) (any, error) { return platformModule(argS(a, 0)), nil },
		}
		j.codecHost(f)
		j.opfsHost(f)
		j.cryptoHost(f)
		return f
	})
}
