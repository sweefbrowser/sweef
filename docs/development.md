# Development guide

How the code is organised and which documents are contracts. Start with [CONTRIBUTING.md](../CONTRIBUTING.md) for the workflow.

## Shape of the system

```
 cmd/sweef            CLI entry: mcp | serve | fetch | snapshot | shot | eval | call | open/click/... (daemon-backed) | version
   └── internal/mcp, rpc, protocol, cliclient   the automation surface (MCP server, wire format, CLI daemon client)

 internal/engine          a tab: loads a page, runs JS, lays out and paints; ties everything below together
   ├── internal/net       HTTP client, cache, DOM tree (Go DOM) and snapshots
   ├── internal/style     CSS parsing, cascade and the cross-pass style cache
   ├── internal/render    box tree, layout (block, inline, flex, grid, table), display list
   ├── internal/raster    software rasteriser, Canvas 2D, SVG, shadows, filters, damage-rect repaint
   ├── internal/font      font discovery, shaping, WOFF/WOFF2/TTF/OTF decoding
   ├── internal/privacy   tracker blocklist
   ├── internal/sitestore per-site storage (IndexedDB, caches, OPFS)
   ├── internal/intl      Intl on CLDR data
   └── internal/pdf       PDF export
```

An agent browser has no window, no media player, no WebRTC and no WebGL: they were removed on purpose (see CHANGELOG).

JavaScript runs in QuickJS (no JIT) through `modernc.org/quickjs`; the DOM is implemented in Go and exposed through host
functions and a JavaScript prelude (`internal/engine/*_prelude.js`).

## Contracts you must not break casually

| Document | What it fixes |
|---|---|
| [compat.md](compat.md) | what is supported and what is a known gap |
| [frames-popups.md](frames-popups.md) | iframes, window.open, popups, postMessage between windows: what works, what does not |

A change that alters one of these needs an ADR ([adr/](adr/README.md)) or at least a discussion in the issue.

## Testing

* `go test -short ./...` is what CI runs on Linux. Heavy codec tests are FATE-gated and only run without `-short`.
* **Fidelity:** `fidelity/` and `tools/fidelity` compare layout and ink against Chromium reference captures made on Linux
  with a fixed font set. They only make sense on Linux with that font set (CI installs it).
* **Paint goldens:** `internal/engine/testdata/paint_golden.txt` pins what the chrome and onboarding pages paint. A moved
  golden must be explained in the PR and regenerated from a clean checkout.
* **Debug cross-checks:** `SWEEF_LAYOUT_CHECK=1` compares every cached layout with a from-scratch one;
  `SWEEF_INCR_CHECK=1` does the same for incremental paint. Run the UI tests with them on after touching layout caches.

## Benchmarks and the evidence rule

| Tool | Measures |
|---|---|
| `scripts/bench/datasaver.sh` | bytes on the wire for the same pages, three modes plus Chromium → `docs/data-saver-bench.md` |
| `bench/loadtl` | page-load timelines |
| `go run ./tools/startbench -a before -b after` | process start, first page (cold and warm prelude cache), first UI frame and peak RSS of two binaries, run alternately |

Benchmarks refuse to run on a busy machine where noise would mislead; do not edit generated reports by hand.

## Binary size budget

The stripped release binary has a budget of 40 MiB (`SWEEF_MAX_BINARY_BYTES` in `scripts/release/build.sh`, checked for
every target). The stripped linux/amd64 build is 36,921,470 bytes (35.21 MiB), so 4.79 MiB are left; darwin/amd64 is the
tightest target with 4.34 MiB. Two checks guard it:

* `scripts/release/build.sh` prints size and headroom for every target, fails a target that is over the budget (with the
  per-package table below) and warns when less than 2 MiB is left (`SWEEF_MIN_HEADROOM_BYTES`).
* `go test ./cmd/sweef -run BinarySizeBudget -v` (skipped with `-short`) builds the browser with the release flags and
  fails when it is over the budget **or** more than 1 MiB bigger than `scripts/release/size-baseline.txt`, the size of the
  last accepted build. A change that grows the binary on purpose updates the baseline in the same commit and says why in
  the CHANGELOG; a change that shrinks it lowers the baseline.

**See what is big.** `make size` builds linux/amd64 with the release script and prints the ELF sections, the symbols per
Go package split into code, read-only data and initialised data, and the largest symbols (`tools/sizeprof`, which also
takes any unstripped build: `go run ./tools/sizeprof -bin <unstripped> -stripped <stripped>`). Read the table with the
section sizes next to it: `.gopclntab` (function metadata, about a third of the file) and the type descriptors grow
with the amount of code and `go tool nm` attributes neither to a package; embedded files are in `.rodata`; tables that are
filled at start-up (`var t [N]T` assigned in `init`) are zero-initialised and cost nothing in the file. `go build
-ldflags=-dumpdep` shows why a package is linked, `go list -deps ./cmd/sweef` lists what is.

**Add data without breaking the budget.** Never embed text or binary data as it is. In order of preference:

1. *Embed it deflated and inflate it on first use* (`internal/zpack`; standard-library `compress/flate`, no new
   dependency). The source file stays in the repository and is what you edit; the compressed twin is what `go:embed`
   picks up, and a test decodes it and compares it with the source, so a stale twin fails the tests instead of shipping.
   `make embeds` regenerates all of them (`go generate ./internal/engine ./internal/font ./internal/intl`).
   * JavaScript in `internal/engine` (`*.js`, `platformjs/*.js`) becomes `NAME.js.z`. Preludes are registered with
     `registerPreludeZ`; a warm start never inflates them, because the prelude bytecode cache is keyed by the deflated
     bytes (`preludeKey`) and not by 1.2 MB of source. `platformjs/` modules are inflated when a page first touches
     their API. **If you add or edit a `.js` file there, run `make embeds` and commit the `.z`**; `TestEmbeddedJSIsCurrent`
     says so when you forget.
   * The eight Go fonts are in `internal/font/gofont.zar`, **without their TrueType hinting program** (more than half of the
     glyph data; Sweef asks for unhinted outlines everywhere). `TestGoFontArchive` and
     `TestEmbeddedGoFontsGlyfMatchOriginal` compare every glyph, advance, mapping, name and metric with the x/image fonts.
   * The IANA time zone database is `internal/intl/zoneinfo.zar` (aliases stored once; it replaces `time/tzdata`, which keeps
     the toolchain's 408 KB zip uncompressed). The operating system's database is still consulted first. It is the
     toolchain's tzdata at the time of `go generate ./internal/intl`; the test logs when the toolchain has a newer one.
   * Locale data (`internal/intl/data/*.json.gz`) is already gzipped, one file per locale, loaded on demand.
2. *Use the smallest type that holds the values* (`int16` instead of `int` for large lookup tables).
3. *Compute it at start-up or on first use* when that is cheap, instead of storing it.
4. Download on demand is allowed only for rarely used data, cached on disk, pinned by a SHA-256 in the binary, working
   offline for the common case, and documented. Nothing is downloaded today.

**Check what a dependency drags in** (`go list -deps ./cmd/sweef | grep <module>`, then `tools/sizeprof`). A second copy
of something the standard library has is the usual find: `golang.org/x/net/http2`, imported for a ping timeout, linked a
whole second HTTP/2 stack (377 KB); `http.Transport.HTTP2` has done that since Go 1.24.

**Release builds compile some packages without inlining** (`scripts/release/noinline-packages.txt`, `-gcflags=<pkg>=-l`):
inlined copies are recorded in the function metadata, so inlining costs size twice. The list is the connection set-up and
control plane (pion, `net`, `net/http`, `crypto/tls`, `crypto/x509`, the agent, MCP, RPC, `internal/app`, ...) and saves
757,760 bytes. Everything on a per-pixel, per-byte, per-node or per-request path stays inlined; an experiment that also
turned it off for the tracker filter made `privacy.MatchBlocked` 36% slower (1.07 to 1.46 us) and it was removed again.
Add a package to the list only with a benchmark that shows no change. A plain `go build` does not use the list, so
development builds are unchanged. The release also links with `-funcalign=16` (functions on 16 instead of 32 bytes on
amd64; the linker still honours larger alignments that assembly or loops ask for): 286,720 bytes.

**Start-up must not regress.** Compare binaries with `go run ./tools/startbench -a before -b after`: process start, first
page with an empty and a warm bytecode cache, first UI frame, peak RSS and CPU time, the two binaries run alternately.
The work above, against the binary before it (medians, 4-vCPU VM shared with other jobs):

| case | before | after |
|---|---|---|
| `sweef version`, 60 runs | 7.5 ms, 19.2 MiB | 7.4 ms (-0.7%), 19.0 MiB |
| first page (Intl, time zone, URLPattern), empty prelude cache, 25 runs | 343.1 ms, 59.7 MiB | 341.2 ms (-0.6%), 57.8 MiB |
| the same page, warm cache, 30 runs | 177.5 ms, 58.7 MiB | 165.4 ms (-6.8%), 54.6 MiB |
| `sweef gui --headless` to the first frame and exit, 20 runs | 1173.4 ms, 197.2 MiB | 1174.5 ms (+0.1%), 191.9 MiB |

The warm start is faster because it no longer concatenates and hashes 1.2 MB of JavaScript to name the cache file.

**What was tried and left out.** A preset dictionary for the locale files: 6% of 758 KB. Replacing repeated strings of
the locale files by a shared table: 4%. A solid xz stream would be 44% smaller but needs a decoder this repository does not
have. `-gcflags=-l` for all of `std` saves 1.76 MB when measured alone, but it includes the runtime's allocator and
`strings`, `bytes` and `sort`; not adopted. Disabling inlining in wazero (-278 KB) would put the WebAssembly compiler and interpreter loop at risk; not benchmarked, not adopted. Replacing `github.com/pion/dtls`'s
`encoding/gob` (123 KB) needs a fork of a module whose `ConnectionState` depends on it. AV1's 100 KB quantizer-matrix table
compresses to 38 KB but is generated from the specification by `tools/av1gen`.

## Code style

* `gofmt` (CI enforces it, `third_party/` excluded), `go vet -composites=false`.
* Comments explain *why*; keep them short and near the code they explain.
* No cgo. Platform code lives in build-tagged files (`*_unix.go`, `*_windows.go`, `*_darwin.go`).
* Errors are returned, not logged and swallowed; background goroutines must stop when their tab or app closes (a leak
  test guards this).
