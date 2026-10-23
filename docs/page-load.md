# How a page loads

This is the load path of `internal/engine` and `internal/app`: what is waited for before the window shows a page, what is
not, and how to see where the time of a particular page goes.

## Two contracts

| | headless (`sweef snapshot`, `shot`, `batch`, MCP, RPC, the agent) | window (the GUI) |
|---|---|---|
| call | `Tab.Navigate` | `Tab.NavigateProgressive`, then `LoadStep` between paints and input |
| returns | when the page has settled: scripts ran, `load` fired, the settle budget (`SettleMs`) is spent, every image of the first layout is in | when a first frame can be painted |
| images | fetched by layout, in parallel (`prefetchDocImages`) | fetched in the background (`SetAsyncImages`), the window repaints as they arrive (`SetUpdateHook`) |

Both drive the same state machine (`progload.go`), so the document a window ends with is the one a headless load
produces (`TestProgressiveAndBlockingLoadsEndInTheSameDocument`, and the pixels of all 107 fidelity pages in
`TestProgressiveFinalPaintEqualsBlockingOnFidelityPages`). The agent drives its tabs through the app's own navigation
path but with the blocking contract (`etab.agentLoad`): `AgentNavigate` returns the settled page.

## Window path

1. The document request. While its body arrives, the **preload scanner** (`preload.go`) tokenizes what came in and starts
   the stylesheet and script requests it finds, so they overlap the rest of the download.
2. Parse, then wait for the **render-blocking stylesheets** (`applyStyles`, at most 10 s). Scripts are fetched in
   parallel at the same time but do not hold the first frame.
3. The scripts that are already downloaded and sit at the head of the blocking order run for at most 40 ms
   (`prepaint`): a page whose first inline script flips a class gets its first frame in the flipped state.
4. **First frame**: the worker lays out and paints, `onCommitted` fixes the address, title and history entry.
5. `driveLoad` steps the rest in 25 ms slices, each its own worker op, so input and paint requests queue in
   between: remaining scripts (a script that is still downloading yields the worker instead of parking it), then
   `DOMContentLoaded`, a short pump, `load` (the spinner stops here, `onLoaded`), then the settle pump. A slice
   repaints when the document changed, at most every 150 ms.
6. Images arrive in the background; a relayout is requested at most every 90 ms.

A unit of page work (one script, one timer batch) is not interrupted: a script that runs for seconds still blocks
that tab's worker for that long (QuickJS has no JIT; the 5 s script timeout applies). Other tabs and the chrome have
their own workers.

## Page requests are asynchronous

`fetch()`, asynchronous `XMLHttpRequest` and `sendBeacon` run on their own goroutine (`asynchttp.go`); the outcome is
queued for the event loop like a socket frame and handed to the page's callback by the pump. Before, they were
made in place from a host call, which parked the tab for the length of every request: a page's ten requests came one
after the other, and one that hangs (a 30 s ad ping on MDN) froze the tab and its input for 30 s
(`TestFetchIsAsynchronous`). Synchronous XHR still blocks, as the page asked, and requests made while a module is
being evaluated complete in place (QuickJS awaits a module's top-level promise without an event loop).

A headless load still returns a settled page: its pumps wait for requests that are in flight, but for none that is
older than `httpGrace` (4 s), so a request that hangs does not hold the page. A window never waits: results arrive
as the tab is woken (`SetWakeHook`).

The static imports of a module are found as each source arrives and download in parallel (`prefetchModuleDeps`)
instead of one round trip at a time from inside the evaluation.

## Fetching

* One request per URL per navigation: `fetchRes` joins a request that is in flight (preload scanner, prefetch, loader,
  layout) and keeps what the HTTP cache may not store (`no-store`, `Vary: *`, error statuses) in a per-navigation memo.
  `TestSubresourcesRequestedOncePerNavigation` counts requests on an `httptest` server. (This was the cause of the
  duplicate stylesheet and script requests on the benchmark fixtures: they are served `no-store`.)
* Scripts that page code inserts are downloaded as soon as they are inserted.
* Compiled bytecode is cached per source hash, classic scripts and modules, in memory and in the profile's cache
  directory; the timeline marks every script `bytecode:hit|miss|small`.
* Module scripts resolve relative imports against the importing script (they used to resolve against the document) and
  bare specifiers through `<script type="importmap">`.

## Looking at one page

```
go run ./bench/loadtl -url https://go.dev/doc/effective_go                 # headless contract: per-phase timeline
go run ./bench/loadtl -url https://go.dev/doc/effective_go -progressive    # the window's path
go run ./bench/loadtl -sites bench/datasaver/sites.txt -n 5 -summary       # one line per page
go run ./bench/loadtl -url ... -twice -cache /tmp/c -console               # second visit, page console (script errors)
go run ./bench/loadtl -url ... -cpuprofile cpu.prof                        # where the CPU goes
```

HTTPS_PROXY is honoured. The timeline lists, with start offset and duration in ms: `doc.fetch` (and with the network
trace `net.dns`, `net.connect`, `net.tls`, `net.ttfb`), `doc.parse`, `css.fetch`, `http.*` (every request),
`script.fetch`, `script.exec`, `layout`, `img.fetch`, `event.DOMContentLoaded`, `event.load`, `pump.*`,
`first-paint-ready` (what `NavigateProgressive` returned at). In code: `Tab.LoadTimeline()`.

The `SWEEF_STAGES=1` trace (see `stagetrace.go`) is the older, per-evaluate view.

## Screenshots wait for the page to stop moving

A load returns, and an action settles, once the document's *content* has been quiet for a moment. Style attribute
writes are not content (`fnet.Doc.ContentGen`), so that an endless animation does not hold every command. A
photograph needs the opposite: sites reveal themselves with animation libraries that rewrite `opacity` and
`transform` every frame, and a capture taken at the end of the settle shows the intro's first frames (pipilot.dev:
a blank page; the3rdacademy.com: faded headings).

`Tab.SettleVisual(timeout, quiet)` (`internal/engine/visualsettle.go`) runs before every PNG screenshot
(`sweef shot`, `sweef screenshot`, `Page.screenshot`, `browser_screenshot`): it pumps the page until
`Doc.MutationGen`, which counts style writes too, has been still for 300 ms, for at most 2 s.

* A page without script returns at once (nothing moves; CSS animations and transitions are not run).
* Elements that animate for ever (a ticker, a spinner, a pulsing dot) do not hold it: once only the same few
  elements (at most 12) have been written for 1.2 s, all of them already in the first half of that window, and
  nothing else changed, the page counts as settled.
* A page that keeps changing its content (a scripted live demo) cannot go still; the wait ends at the timeout and
  the RPC result carries `"stable": false`, `sweef shot` prints a note.
* `--wait <duration>` (`sweef shot`), `--wait <ms>` / `--no-wait` (`sweef screenshot`), `waitMs` / `noWait`
  (RPC and MCP) change or turn off the wait.

Measured on two real sites, `sweef shot` with a fast load (`--nav-settle-ms 100 --nav-post-load-ms 50`), mean
absolute pixel difference (0 to 255, 640x360) against a Chromium capture 2.5 s after load; lower is closer:

| `--wait` | pipilot.dev | the3rdacademy.com |
|---|---|---|
| 0 (the previous behaviour) | 28.1 (blank page, 5 KB PNG) | 21.0 (faded) |
| 500 ms | 29.2 | 13.7 |
| 1 s | 29.7 | 9.2 |
| 1.5 s | 19.3 | 8.7 |
| 2 s (default) | 19.3 | 8.7 |
| 3 s | 19.3 | 8.7 |

What remains is not timing: nav alignment and a background on pipilot.dev, and a stripe overlay on
the3rdacademy.com. Two sites, one machine, a live network: the numbers show the direction, not a guarantee for other
sites; an intro longer than the default needs `--wait`.
