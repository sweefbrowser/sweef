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

## Lazy JavaScript mode

Lazy is the default of every agent entry point (CLI, daemon, MCP). `SWEEF_JS_MODE=eager` (or `--js-mode eager`) runs
every page's scripts at load like a browser would. The engine library itself still defaults to eager.

Lazy mode reads a server-rendered page without running its scripts, and runs them the
moment something needs the page to be alive (`internal/engine/jslazy.go`).

A snapshot of MDN, GitHub or PyPI is the same with or without the hydration bundle: the headings, text, links and form
fields are in the markup the server sent. Running the bundle costs seconds of QuickJS and a forced layout. In lazy mode a
headless navigation:

1. fetches and parses the document and applies its stylesheets;
2. runs the leading **inline** scripts only (the small head scripts that flip a class or set a flag the page's CSS depends
   on); the first external script ends this step, however ready it is;
3. returns, leaving the rest of the load pending exactly as a progressive load leaves it.

Whatever needs the page alive then runs the rest the way a blocking load would have (`Tab.wakeJS`): any action (click, fill,
type, press, hover, focus, check, select, drag, tap, scroll, wheel, mouse), `evaluate`, any wait (selector, function, load
state, URL, settle), a form submit, and a PNG screenshot. Reads do not wake it: snapshot, get text, find, console, network.

A page whose markup has little content (fewer than 400 visible characters or fewer than 3 links, buttons and fields: a
client-rendered shell with an empty root) is not deferred; its scripts run at load, because there is nothing to read until
they have.

What this changes for an agent: a snapshot taken right after `open` describes the page as served. Content that only the
scripts add (a list loaded by `fetch`, a hydration-only widget) appears after the first action or `evaluate`, or after a
wait. If your task reads such content directly, leave the mode off (the default).

## Snapshot size

A snapshot is capped at 20,000 characters (about 5k tokens) by default. When a page is longer, the snapshot ends with the
number of nodes cut and an outline of every heading on the page, so an agent can scope to the part it needs
(`selector=`, `interactiveOnly=true`) instead of reading everything. `sweef snapshot <url>` prints this tree; the old
uncapped JSON node list is `--aria=false`. On the Wikipedia article for Alan Turing the one-shot default went from
1,038,421 to 20,700 bytes.
