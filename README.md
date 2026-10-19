# Sweef Browser

[![ci](https://github.com/sweefbrowser/sweef/actions/workflows/ci.yml/badge.svg)](https://github.com/sweefbrowser/sweef/actions/workflows/ci.yml)
[![security](https://github.com/sweefbrowser/sweef/actions/workflows/security.yml/badge.svg)](https://github.com/sweefbrowser/sweef/actions/workflows/security.yml)
[![license](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![status](https://img.shields.io/badge/status-alpha-orange.svg)](CHANGELOG.md)

**The agent browser that costs nothing to run.** Sweef is an independent browser engine written in Go: one static binary, no Chromium, a real ES2023 JavaScript engine, and a Model Context Protocol server built in. It gives AI agents a compact, readable view of a page plus reliable actions, on about 15 MB of RAM.

Sweef is not a fork of Chromium. It has its own CSS layout engine, rasterizer, software WebGL and media stack, all measured against Chromium (see [Rendering fidelity](#rendering-fidelity)). It bets that agents need **semantics** (structure, roles, names, text, form state, network truth) more than they need a rasterised screen, and it spends its budget there.

## What you get

| | Playwright + Chromium | Sweef |
|---|---|---|
| Install | ~250 MB browser download | **35.5 MiB static binary** (37.2 MB stripped, linux/amd64, measured 2026-10-14; 4.5 MiB of the 40 MiB release budget left) |
| Memory | ~90 to 150 MB idle, 1.1 GB process tree under agent-browser | **45 MB idle daemon, 70 to 110 MB with one typical page, 170 MB with a Wikipedia article; ~265 MB after 40 navigations across real sites** (`sweef serve`, Linux amd64, resident set, measured 2026-10-16; soft budget 384 MB, see [Architecture](#architecture)) |
| CPU | 2 to 4 cores wanted | **1 core** |
| Agent interface | CDP + language bindings | **MCP native**, plus a session CLI, Node and Python SDKs over a framed JSON-RPC socket |
| Page view for the model | DOM dumps or custom snapshots | **Playwright-format accessibility tree** with small `[ref=eN]` handles |
| JavaScript | V8 | **QuickJS (ES2023)** via [modernc.org/quickjs](https://pkg.go.dev/modernc.org/quickjs), pure Go, no CGO |
| Pixels, WebGL, media | Yes | **Yes**, pure Go: CSS layout within 1 px of Chromium on 99.5% of boxes, software WebGL 1/2, Canvas 2D, media and Web Audio |

## Benchmark against Vercel agent-browser

Same 11-step agent flow (open a landing page, snapshot, click through to a login form, fill it, submit, open a React app, click in it, eval, snapshot) on a local fixture site. Median of 7 runs, a fresh daemon for every run, agent-browser 0.27 on Chromium 1194. Reproduce with `bench/run.sh`. A harness for the same question on 39 real public sites (including the failures), and for real-LLM agent runs, is in `bench/realsites/`.

| step | Sweef | agent-browser | ratio |
|---|---|---|---|
| open page (cold daemon) | 24 ms | 578 ms | 24.1x |
| snapshot, interactive only | 8 ms | 239 ms | 29.9x |
| click that navigates | 9 ms | 261 ms | 29.0x |
| snapshot, full | 8 ms | 226 ms | 28.2x |
| fill | 8 ms | 223 ms | 27.9x |
| press Enter that submits | 9 ms | 231 ms | 25.7x |
| get title | 8 ms | 226 ms | 28.2x |
| open React 18 app | 28 ms | 256 ms | 9.1x |
| click in React | 12 ms | 226 ms | 18.8x |
| eval | 7 ms | 218 ms | 31.1x |
| whole flow, one command at a time | 149 ms | 2955 ms | 19.8x |
| **whole flow as one `sweef batch`** | **80 ms** | 2955 ms | **36.9x** |
| daemon memory | 39 MB | 1153 MB | 30x less |

Plain steps run 23x to 28x faster. The two React steps are slower (10x to open, 18x to click) because opening the app executes 140 KB of React in a pure-Go JavaScript engine (about 10 ms) and every CLI call pays a process spawn of about 6 ms. Command by command the whole flow is about 18 to 20x, and as one batch about 30 to 37x (run to run variation on this machine). Snapshots are equal or smaller (466 vs 480 bytes on the landing page, 68 vs 137 on the React app).

What makes navigation fast: pages with no scripts get no JavaScript VM at all; pages with scripts take a VM from a pre-warmed pool; framework bundles are compiled to bytecode once and the bytecode is kept on disk across daemon restarts; sub-resources go through an HTTP cache with ETag revalidation; scripts and stylesheets are prefetched in parallel; the previous page is torn down in the background.

Re-measured 2026-10-15 on a slower shared 4-CPU host, Sweef only (agent-browser was not installed): whole flow 199 ms one command at a time, 100 ms as one batch, daemon 47 MB; the previous build (bcabc8e) on the same host: 202 ms, 104 ms, 57 MB. The ratios in the table above are from the original run.

### Real public sites (no LLM)

47 tasks on 41 public sites (Wikipedia, GitHub, MDN, Hacker News, npm, PyPI, Next.js, React, Angular, Vue, login and e-commerce demos, news, the owner's sites), 3 runs per task, each run a fresh process with empty caches, against Playwright driving Chromium 141. A run passes only if the snapshot holds the expected text, every scripted step succeeds and the final state matches. Reproduce with `bench/realsites/node/modea.mjs`; every task and every failure is in [modea-2026-10-15-gaps.md](bench/realsites/results/modea-2026-10-15-gaps.md), and the comparison with the previous build in [modea-2026-10-15-gaps-before-after.md](bench/realsites/results/modea-2026-10-15-gaps-before-after.md). Live network, so single cells are noisy.

| | Sweef (previous build) | Sweef | Playwright + Chromium |
|---|---|---|---|
| runs passed | 130/141 | 130/141 | 129/141 |
| median cold time to a usable snapshot, the 44 tasks both Sweef builds pass | 2939 ms | **2188 ms** | |
| median cold time to a usable snapshot, the 42 tasks Sweef and Chromium both pass | | 2188 ms | **2029 ms** |
| tasks where it is faster than Chromium | 16 of 42 | **25** of 42 | 17 |
| median snapshot size, est. tokens (bytes/4) | 4088 | **4217** | 7356 |

Where Sweef wins: plain and server-rendered pages (Hacker News, news text, docs.python.org, Effective Go, rust-lang.org, httpbin, books.toscrape) and the TodoMVC, quotes and e-commerce demos open 1.1x to 3.8x faster, react.dev 1.8x and Angular's docs 1.2x faster, Wikipedia about even (1.15x slower to 1.12x faster), and snapshots are about 40% smaller. Where it loses: JavaScript-heavy apps. GitHub is 1.6x to 2x slower (4.6 to 6.1 s against 2.5 to 3.1 s; it was 3.3x to 4.6x), MDN 2.5x to 2.7x, PyPI 1.35x, Next.js docs 1.35x, the owner's React/Vite sites 1.2x to 1.8x. Sweef runs page JavaScript in an interpreter (QuickJS), so a site that executes megabytes of framework code before its content is final pays for it; at the load event Chromium has typically run only 13 to 290 ms of script on these pages (CDP metrics). Stack Overflow (a Cloudflare challenge) and old.reddit.com (a login wall) fail for Sweef; old.reddit.com also fails for Chromium.

Noise-free numbers from a local recording of ten pages ([replay-2026-10-15.md](bench/realsites/results/replay-2026-10-15.md), `bench/replay`, 5 runs each, median and spread): GitHub repo 7.7 -> 3.2 s (Chromium 1.2 s), GitHub issue 7.5 -> 3.3 s (1.5 s), Next.js docs 5.4 -> 1.9 s (0.7 s), the3rdacademy.com 4.0 -> 2.1 s (1.0 s), pipilot.dev 3.3 -> 1.6 s (1.1 s), Wikipedia 2.1 -> 1.3 s (1.1 s), MDN 2.8 -> 2.5 s (0.8 s), PyPI 3.6 -> 2.4 s (1.0 s); react.dev 1.2 s against 1.6 s and Hacker News 0.11 s against 0.43 s. Loading those ten pages costs Sweef 28 s of CPU (previous build 41 s, Chromium 14 s) and 6.7 s while they sit idle for 5 s each (Chromium 13.4 s), with a third of the memory ([resources-2026-10-15-replay.md](bench/realsites/results/resources-2026-10-15-replay.md)). The owner's sites in detail: [owner-sites-2026-10-15-gaps.md](bench/realsites/results/owner-sites-2026-10-15-gaps.md) (screenshot SSIM unchanged; a click that changes the route on pipilot.dev still takes 1.1 to 2.3 s against 0.25 to 0.4 s, because Sweef returns once the new route has rendered and Playwright as soon as the click is dispatched).

With real LLM agents (Mode B, `claude-haiku-5-5`, 40 tasks x 3 runs, 2026-10-15/16, this build, files `bench/realsites/results/modeb-2026-10-16.*`): both agents solved 119/120 (the Sweef agent's one miss is Stack Overflow's Cloudflare check; the Chromium agent's is a PyPI run). The Sweef agent took a median 5.2 s per task against 5.0 s for the Chromium agent (it was 9.0 s against 4.8 s before this round), a median of 1 turn against 2, 10.7K prompt tokens against 11.7K, and $0.0009 against $0.0009 per success ($0.102 against $0.106 for the whole run). Both agents use prompt caching, so most of those prompt tokens are cache reads. So on this suite the two are level on success, wall time and cost, with Sweef slightly ahead on turns and tokens; it does not yet win on wall time. What moved it: a 2.8 kB system prompt instead of 18 kB, the starting page sent with the task, route changes moving the tab URL, batch waits honouring their timeouts, and the faster load and input settles above. Wall times include live-network noise and a 3-run median per task.

## A look at the browser

Sweef draws its own interface (tabs, omnibox, sidebar, settings, first-run setup) with its own engine; there is no
Chromium or native widget toolkit underneath. These images are not mock-ups: `go run ./internal/ui/cmd/uipreview`
renders every screen of the real chrome pages through the engine and writes them to [`docs/design/`](docs/design/).

**First-run setup.** Bring your bookmarks and history from Chrome, Edge, Firefox or Safari (nothing is uploaded), pick a
look, and optionally make Sweef the default browser. The import cards use the real browser marks, not placeholder letters.

<p align="center">
  <img src="docs/design/onboarding-1@2x.png" alt="Sweef first-run setup: import cards for Chrome, Edge, Firefox and Safari with their real logos, default-browser and theme choices" width="880">
</p>

<table>
  <tr>
    <td width="50%"><img src="docs/design/onboarding-2@2x.png" alt="Meet your Sidekick: a live demo of the agent next to the bring-your-own-key card"></td>
    <td width="50%"><img src="docs/design/onboarding-4-dark@2x.png" alt="Last onboarding step, dark theme: tracker blocking and what the Sidekick may see"></td>
  </tr>
  <tr>
    <td align="center"><sub>Step 2: Sidekick, bring your own key (stored on your device only)</sub></td>
    <td align="center"><sub>Last step (dark theme): tracker blocking and Sidekick access, per site</sub></td>
  </tr>
</table>

**Browsing with the Sidekick** (light) and **Data Saver** (dark): the sidebar agent reads the page you are on and shows its
sources and speed; the data chip in the toolbar shows measured use against your plan.

<table>
  <tr>
    <td width="50%"><img src="docs/design/chrome-light-chat@2x.png" alt="Sweef with the Sidekick sidebar summarising an article, light theme"></td>
    <td width="50%"><img src="docs/design/data-popover-dark.png" alt="Data Saver popover in the dark theme: 39 MB used of 100 MB, measured versus estimated savings, top sites"></td>
  </tr>
  <tr>
    <td align="center"><sub>Sidekick sidebar</sub></td>
    <td align="center"><sub>Data Saver: measured vs estimated, per-site rules</sub></td>
  </tr>
</table>

<table>
  <tr>
    <td width="50%"><img src="docs/design/newtab-light.png" alt="New tab page: ask-or-type box, suggestions, top sites and a usage line"></td>
    <td width="50%"><img src="docs/design/chrome-omnibox.png" alt="Omnibox dropdown with history, bookmark and ask suggestions"></td>
  </tr>
  <tr>
    <td align="center"><sub>New tab</sub></td>
    <td align="center"><sub>Omnibox</sub></td>
  </tr>
</table>

## The desktop browser (alpha)

`sweef gui` opens a real window. The browser UI (tabs, omnibox, Sidekick panel, new tab, settings) is HTML
drawn by Sweef's own engine, composited with the page and handed to a thin pure-Go native shell (X11 today
verified; Windows and macOS shells compile but are **unverified at runtime**). Sidekick is an AI agent in the
sidebar that reads the page, acts on your real tabs, and asks before anything sensitive. The agent loop is
written in Go against the Anthropic Messages API directly (`claude-haiku-5-5`, bring your own key), with a
byte-stable cached prefix, snapshot deltas and early dispatch of tool calls; see `docs/agent.md`.

![Sweef on Linux under Xvfb](docs/design/real-gui-linux-xvfb.png)

Measured on one 4-vCPU Linux VM, relative to a headed Chromium 141 (Playwright build, not branded Chrome) under the
same Xvfb, with the same external method for both (`scripts/bench/vschrome.sh`; method, caveats and every number in
[docs/vs-chrome.md](docs/vs-chrome.md); 7 runs each, run on commit `f4584ca`). Xvfb has no GPU, so only the comparison is
meaningful, not the absolute milliseconds.

| | Sweef | Chromium |
|---|---|---|
| Typing in the omnibox, dropdown open (p50) | 5.0 ms | 9.1 ms |
| Tab switch (p50) | 5.3 ms | 18.4 ms |
| New tab (p50) | 7.5 ms | 49.8 ms |
| Start to first UI frame, cold cache | 492 ms | 770 ms |
| Memory, 1 tab / 10 tabs (PSS, whole process tree) | 101 / 260 MiB | 370 / 506 MiB |
| Installed size | 35.5 MiB (binary; was 25 MiB when measured, before WebRTC, WebAssembly, the web platform layer and Intl data, and 39.3 MiB before the embedded data was compressed) | 596 MiB |
| Idle CPU, static page | 0.00% | 0.45% |

Where Chromium is ahead, and we say so:
* **First key that opens the dropdown:** 16.3 ms in Sweef against 13.0 ms.
* **Page load:** on the heavy and single-page-app test pages Chromium reaches the load event 4 to 11 times sooner
  (QuickJS has no JIT), and real pages settle later in Sweef (Wikivoyage 1.4x, MDN 2.7x; Wikipedia is a tie).
  Sweef paints a first frame early, so pages become visible sooner than they finish.
* **Typing p99** is about the same (19 ms against 18 ms), and Sweef starts the dropdown-closed case slower.
* **Compatibility:** no extensions and no colour fonts. WebRTC calls, WebAssembly, IndexedDB, service workers, push,
  camera and microphone capture, iframes and popups are implemented (feature map below), but **none of it has been
  verified against the live sites that matter most**: YouTube, Google Meet, Zoom and WhatsApp Web were not tested (the
  build sandbox has no UDP and YouTube serves it no media); see [docs/compat.md](docs/compat.md). DRM streaming
  (Netflix, Spotify, Prime) needs a Widevine module the user already has plus a signing arrangement we do not have yet
  ([docs/drm.md](docs/drm.md)). Pages that need something we lack show an "Open in system browser" action.

In-process numbers on an idle machine (`go run ./internal/app/cmd/appbench`, `internal/app/BENCH.md`): typing with the
dropdown open is 4.6 ms p50 / 7.5 ms p99 at 1x and 6.3 / 9.7 ms at 2x; the first key that opens the dropdown was 12.7 to 13 ms
at 1x (about 11.3 ms after reusing dropdown rows instead of rebuilding them; measured in-process with
`appbench -open-profile`, not yet re-measured against Chromium) and 28 ms at 2x. Still above our 8 ms target. A live Sidekick task (3 model turns, real API) took 3.4 s and cost
$0.0014; provider time is about 85% of the wall clock. macOS and Windows windows, installers and signing are untested
(CI only builds and unit-tests them). Installers: `docs/install.md`.

## Data Saver (alpha)

For people on small data plans. Everything happens on the device: no proxy, no server-side transcoding, nothing about
your browsing leaves the machine. The browser counts the bytes that cross the network (TLS included), keeps a budget you
choose (for example 100 MB a month), warns at 70% and 90%, and can hard-stop at the limit. Three levels: **off** (counting
only), **standard** (lazy images near the viewport, big images become tap-to-load, media only on play, background tabs
idle, third-party embeds on tap, an HTTP cache that revalidates instead of refetching) and **strict** (text first: no
images, no third-party scripts, a script budget). "Load fully, once" and per-site rules are one tap away. The Sidekick
agent reads pages in a lite mode and reports what its research cost.

Measured on one Linux VM through an independent byte-counting proxy that neither browser controls (18 real public pages,
3 runs each, cold cache; method, site list, exclusions and caveats in [docs/data-saver-bench.md](docs/data-saver-bench.md),
reproduce with `scripts/bench/datasaver.sh -runs 3`):

| Mode | Mean data per page | Pages per 100 MB |
|---|---:|---:|
| Sweef off | 2.07 MB | 48 |
| Sweef standard | 1.20 MB | 83 |
| Sweef strict | 312 KB | 321 |
| Chromium (headless, no browser UI) | 2.73 MB | 37 |

* **The saver itself:** standard uses 42% less data than Sweef with the saver off (41% to 42% over the runs) and strict
  uses 85% less (83% to 85%).
* **Repeat visits:** a second visit costs about 13% of the first in Sweef (served from an on-disk HTTP cache) against 22%
  in Chromium.
* **Read the Chromium column with care.** Sweef did not fetch everything Chromium does when this was measured (no iframe
  documents, no prefetch hints; iframes load since, see [docs/frames-popups.md](docs/frames-popups.md), and this table
  was not re-measured), so part of the gap is a simpler page, not a better saver. The honest like-for-like number is the
  saver against our own "off" row. Chromium's Save-Data header saved nothing measurable.
* **Web fonts were added after this run.** These tables were measured before Sweef could load `@font-face` fonts. A
  separate single-run check on 16 of the sites with fonts on measured about +3% (off), +5% (standard) and +4% (strict)
  more bytes, with the savers still saving roughly 47% (standard) and 88% (strict) against off. A full three-mode rerun
  was started and stopped partway (the partial results are in `bench/datasaver/results/`), so treat the table as
  "before web fonts" and expect the totals to move by a few points.
* **Sites that did not count.** 5 of the 23 listed sites are excluded for every mode: three are blocked by bot detection
  (Cloudflare and Akamai challenges, documented in [docs/compat.md](docs/compat.md)), one URL was a 404 and one exceeded
  the 60 s navigation limit. The result describes these 18 pages, on one network, on one day.
* The Sidekick's own model traffic is not in the meter (it goes outside the engine); the Settings page says so.

## Batching: one call, many steps, one observation

Agents pay per tool call, and a model round trip costs seconds while a Sweef command costs milliseconds. Send the whole plan at once:

```bash
sweef batch "open https://acme.test/login" "fill label=Email ada@example.com" "check 'label=Remember me'" \
            "click 'text=Sign in'" "expect url /dashboard" "get text h1" --observe delta
```

```text
✓ open https://acme.test/login (4ms)  → Sign in
✓ fill label=Email ada@example.com (0ms)
✓ check label=Remember me (0ms)
✓ click text=Sign in (3ms)
✓ expect url /dashboard (0ms)  → {"passed":true}
✓ get text h1 (0ms)  → Welcome back, Ada
6/6 steps in 8ms  Dashboard  https://acme.test/dashboard
+ - heading "Welcome back, Ada" [level=1 ref=e7]
...
```

- **Steps** use the CLI verbs (open, click, fill, type, press, hover, check, select, scroll, get, is, wait, eval, back, forward, reload, snapshot, sleep, download, pdf) or any raw RPC method.
- **expect** asserts with auto-wait: `text`, `url`, `title`, `visible`, `hidden`, `count`, `value`.
- **Failure handling**: the batch stops at the first failing step and reports which one; `--continue` keeps going, and a step marked optional never stops it.
- **Observation**: the batch ends with one view of the page: `interactive`, `full`, `delta` (only the lines that changed since your last snapshot) or `text`.
- The same feature is the `browser_batch` MCP tool and the `Batch.run` RPC method, and `sweef batch -` reads steps from stdin.

## Quick start

Get the engine one of three ways: a prebuilt binary from the [Releases page](https://github.com/sweefbrowser/sweef/releases)
([docs/install.md](docs/install.md)); the SDKs (`npm install @sweef/browser`, `pip install sweef-browser`), which
download it on first use; or `go build` from a clone, as below. (`go install ...@latest` does not work: go.mod has
`replace` directives for the trimmed audio modules in `third_party/`.)

```bash
git clone https://github.com/sweefbrowser/sweef && cd sweef
go build -o sweef ./cmd/sweef          # pure Go; cross-compiles with GOOS/GOARCH
./sweef version

# session CLI (a daemon keeps the page alive between calls)
./sweef open https://example.com
./sweef snapshot -i                    # controls and headings with [ref=eN]
./sweef click @e1
./sweef get title
./sweef close

# one-shot
./sweef fetch https://example.com                 # visible text
./sweef snapshot https://example.com --aria       # accessibility tree, YAML
./sweef eval https://example.com 'document.querySelectorAll("p").length'
./sweef shot https://example.com --out page.png   # text-rendered PNG

./sweef mcp                                       # MCP server on stdio
./sweef serve --addr 127.0.0.1:9222               # socket server for SDKs
```

### Session CLI

Every verb agent-browser has for driving a page, at a fraction of the latency: `open`, `snapshot [-i] [-c] [-d n] [-s sel] [--delta]`, `click`, `dblclick`, `fill`, `type`, `press`, `hover`, `focus`, `check`, `uncheck`, `select`, `scroll`, `drag`, `get text|html|value|attr|title|url|count|box`, `is visible|enabled|checked`, `wait <sel|ms> | --text | --url | --fn | --load`, `eval`, `console`, `network`, `back`, `forward`, `reload`, `tab`, `screenshot [--full]`, `pdf [path]`, `downloads`, `download <url>`, `route <glob> --json v | --abort | --continue …`, `unroute`, `bind <name> [reply]`, `bindings`, `batch`, `find role|text|label|placeholder|alt|testid … click|fill|text`, `set viewport|offline|headers|media|geo|ua`, `cookies`, `storage`, `state save|load`, `dialog`, `close`. Refs are accepted as `@e3`, `e3` or `ref=e3`. `--session <name>` isolates sessions, `--json` gives machine output. The daemon exits after an hour idle (`SWEEF_IDLE_TIMEOUT`).

`sweef-cli` (built next to `sweef` by the release script) serves the same session commands without the browser engine linked in, so each command starts in about half the time (a `get title` round trip 4.3 ms against 9.9 ms for `sweef` and 10.2 ms for agent-browser, [bench/surfaces](bench/surfaces/results/2026-10-15-gaps/report.md)); it starts the `sweef` binary next to it as the daemon and hands every other command to it.

MCP configuration for any MCP host (Claude Code: `claude mcp add sweef -- /usr/local/bin/sweef mcp`):

```json
{ "mcpServers": { "sweef": { "command": "/usr/local/bin/sweef", "args": ["mcp"] } } }
```

SDKs (`sweef` binary on PATH or in `SWEEF_BIN`):

```js
const { Sweef } = require('@sweef/browser');           // npm install @sweef/browser
const sw = await Sweef.launch(); const page = await sw.newPage('https://example.com');
console.log(await page.snapshot({ interactiveOnly: true })); await sw.close();
```

```python
from sweef import Sweef                                 # pip install sweef-browser
with Sweef.launch() as sw:
    print(sw.new_page("https://example.com").snapshot(interactive_only=True))
```

Setup for each MCP host, what the tools return, the SDK and CLI references and the RPC method list: [docs/sdk-mcp.md](docs/sdk-mcp.md). Measured against `@playwright/mcp`, `playwright-core` and `agent-browser` on each surface: [bench/surfaces](bench/surfaces/README.md).

### The agent loop

```text
1. browser_navigate      {url}                       → title, final URL
2. browser_snapshot {}                              → YAML tree, controls carry [ref=eN]
3. browser_click         {selector: "e12"}           → acts on exactly that element; the reply lists what changed
4. browser_snapshot      {delta: true}               → only the lines changed since step 2
```

A snapshot of the GitHub login page looks like this:

```yaml
- link "Skip to content" [ref=e1]:
  - /url: "#start-of-content"
- main:
  - heading "Sign in to GitHub" [level=1 ref=e2]
  - form:
    - text: Username or email address
    - textbox "Username or email address" [required ref=e3]
    - text: Password
    - textbox "Password" [required type=password ref=e4]
    - link "Forgot password?" [ref=e5]:
      - /url: /password_reset
    - button "Sign in" [ref=e6]
```

Refs are small integers from a per-document table, so they stay valid while scripts mutate the page. A ref from a previous page returns `ref e12 is from a previous page; take a new snapshot`. Paragraphs and plain text carry no ref, which is where most of the token saving comes from. Snapshot options: `selector` to scope, `interactiveOnly` for controls and headings only, `maxChars` (default 60 000, truncated with a note), `maxTextLen`, `maxDepth`.

## How JavaScript works

Each tab owns a QuickJS VM with a 1 GiB heap cap (`--js-mem-mib`) and a 5 s per-script timeout. The Go DOM is the source of truth; a prelude implements `window`, `document`, elements, events, timers, `fetch`, `XMLHttpRequest`, `localStorage`, `MutationObserver` and the rest of the browser API on top of a small host bridge (see `internal/engine/jsbridge.md`). Scripts load the way browsers load them: inline and `src` scripts in order, `async` and `defer` after, `type=module` with relative imports, scripts inserted by page code (webpack chunks, tag managers) when they connect. After `load`, the event loop pumps until the page is idle or the settle budget (`--settle-ms`, default 1500) is spent, and again briefly after every action, so a click that triggers a fetch and a re-render is reflected in the next snapshot.

QuickJS is the only engine in shipped builds; an experimental, opt-in V8 backend (`--js-engine v8`, a separate library, Linux needs a special build) is 1.2-3.5x faster on the pages we measured but is missing several features and is not approved as a default: see [docs/v8.md](docs/v8.md).

VMs come from a pre-warmed pool, and the prelude and large scripts are cached as bytecode in memory and on disk, so a React page installs in about 10 ms. WebSockets are real connections, and Web Workers run in their own VM. What this means in practice: server-rendered sites, documentation, forms, dashboards, and React, Preact or jQuery apps run. example.com renders its script-injected paragraphs, react.dev renders its navigation and content, GitHub login and Wikipedia snapshot cleanly. Layout, canvas, WebGL and media playback work as described in the feature map below, and Sweef reports script errors in the console log rather than pretending.

## Feature map

Three states: **works** (covered by tests), **partial** (works for the common case, gaps noted), **not planned**.

| Capability | State |
|---|---|
| Locators: css (full selector engine incl. `:has`, `:nth-child`, attribute operators), `text=`, `role=`, `label=` (incl. wrapping labels), `placeholder=`, `alt=`, `testid=`, refs, `.nth/.first/.last/.filter` | works |
| Accessibility snapshot with content, states, refs, scoping, truncation, `--delta` | works |
| Batching: many steps per call with expectations and a final observation (CLI, MCP, RPC) | works |
| Actions: click, fill, type, press, check, uncheck, selectOption, hover, focus, drag, scroll, tap, with events delivered to page listeners and `preventDefault` honoured | works |
| JavaScript: ES2023, DOM API, events, timers, fetch/XHR, storage, modules, dynamic scripts, eval timeout | works |
| Web Workers on their own VM: URL, blob: and data: workers, `importScripts`, fetch, timers | works |
| WebSockets: text and binary frames, cookies sent, frames recorded in the network log | works |
| Visibility from the CSS cascade: inline and linked stylesheets, `@media`, specificity, `!important`, inherited `visibility` | works |
| Forms: GET and POST submission, cookies, redirects | works |
| Routes: glob or `/regex/`, method and body matching; fulfill, abort, or continue with URL, method, body and header rewrites; `times` | works |
| Downloads: attachments, `<a download>`, `blob:` and `data:` URLs, saved to a downloads directory | works |
| PDF export: headings, text, lists, tables, clickable links, pagination, page formats | works |
| Exposed bindings: page calls recorded, JSON replies returned to the page | works |
| Wait states: attached, visible, hidden, enabled, load, `networkidle` | works |
| Console, network (incl. POST bodies) and DOM-mutation logs (bounded rings), HAR export | works |
| Developer Tools: `F12` or `Ctrl+Shift+I` (`Cmd+Opt+I` on macOS) opens a resizable panel docked under the page with a Console (live log, errors with file and line and their stack, a prompt that evaluates in the page), Network (every request with status, type, size, cache state and a waterfall, trackers marked as blocked, headers and a body preview), Elements (DOM tree, the CSS rules that match, computed style, box model, the selected node highlighted on the page, live edits of attributes, text and `style`), Sources (scripts and stylesheets with a pretty printer) and Application (cookies, storage) ([docs/devtools.md](docs/devtools.md)) | works on Linux (X11 and headless); Windows and macOS compile-checked only; no breakpoints, no source maps, no click-to-pick |
| Cookies (RFC 6265 jar per context), storage state save/load | works |
| Multiple isolated contexts, tabs, history | works |
| Emulation: viewport, UA, offline, color scheme, extra headers, geolocation, permissions | works |
| Dialogs: alert, confirm, prompt answered from an armed response | works |
| Init scripts | works |
| Geometry: a real CSS engine (cascade, box model, block, inline, float, flex, grid, table, multicol, positioning, transforms) drives element boxes, `getBoundingClientRect`, `getClientRects`, offset/client/scroll metrics, hit-testing, coordinate clicks and screenshots, so they always agree | works (see fidelity below) |
| Screenshots: pixels from the real rasterizer (anti-aliased paths, real fonts, gradients, shadows, filters, blend modes, canvas, WebGL and video frames), viewport or full page | works (text is unhinted, so glyph edges differ slightly from Chromium) |
| Canvas 2D: paths, text, gradients, patterns, filters, shadows, compositing, `ImageData`, `OffscreenCanvas`, `toDataURL` | works |
| WebGL 1 and 2: GLSL ES 1.00 and 3.00 compiler, full state machine, MSAA, transform feedback, UBOs, instancing; three.js, PixiJS and regl render correctly | works |
| Media: `<video>` and `<audio>` element behavior, MSE, text tracks, Web Audio (all node types, worklets, offline rendering) | works (codec limits below) |
| WebRTC: `RTCPeerConnection`, data channels, VP8 and Opus calls, jitter buffers, NACK, PLI, congestion control, `getStats` ([docs/webrtc.md](docs/webrtc.md)) | works between two Sweef peers on loopback; **real Meet, Zoom and Jitsi calls untested** |
| Camera, microphone and screen capture: `getUserMedia`, `getDisplayMedia`, `enumerateDevices`, permission prompts and indicator ([docs/capture.md](docs/capture.md)) | works on Linux (V4L2 and PulseAudio, synthetic devices in tests); Windows and macOS compile-checked only |
| `MediaRecorder` of camera, microphone, screen, `canvas.captureStream()` and Web Audio streams to WebM (VP8, Opus) and Ogg Opus, with `timeslice`, pause and `isTypeSupported` ([docs/mediarecorder.md](docs/mediarecorder.md)) | works against synthetic devices and local servers; no MP4, VP9 or H.264 recording; **WhatsApp Web voice notes not tried on the real site** |
| WebAssembly: full JS API on wazero, live `memory.buffer`, workers, shared memory and atomics ([docs/webassembly.md](docs/webassembly.md)) | works; an Emscripten pthreads program was not tried |
| `SharedArrayBuffer`, `Atomics` (blocking `wait` in workers), cross-origin isolation (COOP + COEP) | works |
| WebCodecs: video and audio decoders for VP8, VP9, H.264, HEVC, AV1, Opus, AAC, MP3, Vorbis; VP8 and Opus encoders ([docs/webcodecs.md](docs/webcodecs.md)) | partial (no H.264 or AV1 encoder, no hardware path) |
| IndexedDB, Cache Storage, OPFS, `navigator.storage` with quota and eviction, on a disk-resident ordered store ([docs/storage.md](docs/storage.md)) | works |
| Storage partitioned by top-level site (cookies, `localStorage`, IndexedDB, caches, service workers, HTTP cache) ([docs/storage-partitioning.md](docs/storage-partitioning.md)) | works |
| Service workers with fetch interception, Web Push, notifications, Background and Periodic Sync, Web Locks ([docs/service-workers.md](docs/service-workers.md), [docs/push.md](docs/push.md)) | works on Linux; push only while the browser runs |
| Iframes painted into the page and interactive (clicks, typing, scrolling), `window.open` and popups with `opener`, `postMessage` and the popup blocker ([docs/frames-popups.md](docs/frames-popups.md)) | works against local test servers; real Google, Facebook and Stripe flows untested; no cross-window DOM access |
| Audio output (pure-Go PulseAudio on Linux; oto on Windows and macOS) | works on Linux; Windows and macOS compile-checked only |
| H.264 and VP9 playback: 1080p30 in real time on 4 cores, 1080p60 when the machine is not busy ([docs/media-streaming.md](docs/media-streaming.md)) | works (measured on synthetic clips) |
| Encrypted media: EME with Clear Key; Widevine through a user-installed CDM in the `sweef-cdm` helper; PlayReady and FairPlay stubs ([docs/drm.md](docs/drm.md)) | Clear Key works; **Widevine, PlayReady and FairPlay untested against real services**; no bundled CDM |
| `Intl`: `NumberFormat`, `DateTimeFormat`, `PluralRules`, `RelativeTimeFormat`, `ListFormat`, `DisplayNames`, `DurationFormat`, `Collator`, `Segmenter` on CLDR 47 data for 73 locales, with the IANA time zone database embedded | partial (against Node 22 on seeded matrices: numbers 99.6%, dates 98.3%, collation 99.85%; eleven calendars, pinyin and stroke order; no dictionary word breaking for Thai, Lao, Khmer, Burmese, Chinese and Japanese, no `chinese`/`dangi` calendar) |
| Echo cancellation, noise suppression and automatic gain on the WebRTC send path | works (reported in `getSettings`); not compared with Chromium's processing |
| Fullscreen API, Media Session (now-playing control in the browser), Async Clipboard with a per-site permission | works |
| Browser extensions | not planned |

## Rendering fidelity

Measured against Chromium 1194 on this repo's fixtures (`go run ./tools/fidelity`, `testdata/*`). Layout numbers are for 1280x800.

| Area | Result |
|---|---|
| Element boxes, 107 pages (basic CSS through shop, dashboard, email, docs, Bootstrap, container queries, subgrid, writing modes, multicol, clip-path, mask, SVG, bidi) at 1280x800 | mean IoU 0.996, 97.7% of boxes within 1 px, 98.9% within 4 px |
| Same pages at 390x844 and 1920x1080 | mean IoU 0.998 at both |
| Pixels (ink agreement with Chromium screenshots) | 98.5% overall; blog 99.6%, landing 99.9%, Arabic and bidi pages 93 to 96% |
| Glyph pixels, 1134 glyphs (9 faces x 14 sizes, 9 to 100 px) | 0.33% of pixels differ by more than 8/255, none by more than 24/255 |
| Page painter, 8 display-list fixtures | 96% to 100% of pixels within 8/255 (text case 99.9%) |
| Canvas 2D, 42 fixtures | shapes 99.4%, text 99.7% within 8/255 |
| SVG (paths, gradients, clips, masks, filters incl. turbulence and lighting, markers, text, `textPath`, `foreignObject`) | feature pages 98.9% to 99.8% ink |
| CSS `clip-path`, `mask`, multicol fragments | 98% to 99.9% ink |
| Transformed elements at fractional offsets, `content-visibility: auto`, `position: fixed` containing blocks, `foreignObject` contexts | boxes IoU 1.000, ink 97.9% to 100% on 5 fixtures |
| CSS 3D transforms (`rotateX/Y/Z`, `rotate3d`, `translateZ`, `scale3d`, `matrix3d`, `perspective()`, individual `translate`/`rotate`/`scale` with Z or an axis, `perspective`, `perspective-origin`, `transform-style: preserve-3d` with BSP depth sorting, `backface-visibility`, `transform-origin` z, `transform-box`), 9 fixtures incl. cube, carousel and flip card | boxes IoU 1.000 and 100% within 1 px (incl. behind-the-camera mirroring), ink 94.5% to 99.9% (mean 98.7%); hit-testing through the inverse projective map matches `elementFromPoint` except on edge pixels |
| WebGL, 76 fixtures | mean 99.9% of pixels within 4/255 (three.js 99.9%, PixiJS 99.99%, regl 100%) |
| Web Audio, 26 offline graphs | sample-exact for filters, delay, convolver and panner; oscillators within 6e-4 |
| Media elements, 21 scenarios | event order identical to Chromium; `canPlayType` matches in 67 of 67 recorded answers |

A 1280x800 text page paints in about 21 ms. A 100k-triangle WebGL frame at 512x512 takes about 170 ms on 4 cores.

What is still different, honestly:

- **Text.** Glyph edges match Chromium to within 12/255 at 9 to 32 px, including rotated, skewed and scaled text and glyphs with cubic outlines (CFF fonts such as Inter: 0.34% of pixels differ by more than 8/255). SVG text under skew, matrix and non-uniform scale transforms, per-glyph `rotate`, `textPath`, vertical writing mode, dashed text strokes (Skia's dash effect, stroker and analytic scan converter are ported) and SVG text decorations match to within 0.3% to 2.6%. Not matched: rotated HTML overlines over real text (about 19/255 off partway along the bar at 5 degrees), dotted, dashed, double and wavy decorations on rotated text, `rotate=` in vertical SVG text, `pathLength` on `textPath`, and Arabic diacritics, Hebrew points and Indic scripts.
- **3D transforms.** Layers are rasterized in their own plane and drawn through the projective map with bilinear sampling; the raster scale rule for perspective layers (1x, or the next whole number once the projection grows a layer by more than about 1.27x) was fitted to Chromium measurements, not derived from its source. Where two layers are exactly coplanar (a `preserve-3d` box and its flipped back face) the hit-test winner can differ from Chromium's. `overflow: hidden` on a preserve-3d child with 3D-transformed content shows a clipped sliver in Chromium that Sweef does not reproduce. SVG elements do not take part in 3D contexts, and `getClientRects` returns the projected fragments.
- **CSS not done:** `content-visibility: auto` reopening on scroll, find-in-page and anchors (a static load skips and opens exactly like Chromium), `@container style()` queries, subgrid line names, `mask-border`, `clip-path` and `mask` on plain inline elements, `box-decoration-break: clone` on wrapped inlines, vertical text with RTL or nested orthogonal flows.
- **SVG not done:** CSS `mask` image layers on SVG elements, `kernelUnitLength`, and a few filter inputs (BackgroundImage, FillPaint).
- **Codecs.** Everything below decodes in pure Go, with no ffmpeg. Audio: MP3, Vorbis, FLAC, PCM, Opus (SILK, CELT, hybrid; Ogg, WebM, MP4), AAC-LC and HE-AAC v1/v2 (SBR and PS; checked against real MPEG conformance streams at 72 to 78 dB) in ADTS and MP4, and USAC / xHE-AAC in MP4 (frequency-domain and LPD cores, eSBR with the harmonic transposer, PVC and inter-TES, MPEG Surround 2-1-2 upmix; checked against the real conformance streams at 61 to 89 dB and against libxaac). Video: VP8, VP9 (profiles 0 to 3, 8 to 12 bit, 4:2:0/4:2:2/4:4:0/4:4:4, intra-only and scaled references), H.264 (Baseline, Main, Extended with data partitioning and SP/SI slices, High, High 10, 4:2:2, 4:4:4, 9 to 14 bit, interlaced PAFF and MBAFF, FMO, long-term references, lossless), HEVC (Main, Main 10, Main Still Picture, the whole range-extension set including extended precision and 16-bit, tiles, wavefront, SAO, MV-HEVC with every layer decodable, and screen content coding with palette, intra block copy, adaptive colour transform and adaptive MV resolution) and AV1 (8, 10 and 12 bit, all common tools, film grain, superres, Annex B, layers and operating points, large-scale tile), bit-exact with ffmpeg, libdav1d, libaom, the JVT and JCT-VC conformance streams and the HM and SCM reference decoders on the test corpus. Frames are decoded on demand from the nearest preceding sync sample, so seeking and `drawImage(video)` work without ffmpeg (`SWEEF_NO_FFMPEG=1` proves it). Not decoded natively, so they use ffmpeg when installed: HEVC SHVC and 3D-HEVC, MV-HEVC POC reset and inferred scaling lists, USAC streams using time-warped MDCT, MPS212 residual coding, phase coding, temporal shaping or transient steering, and HEVC separate colour planes (H.264 separate colour planes decode natively but are only checked by eye). The player outputs the base view of multilayer HEVC. H.264 SI slices and the switching form of SP slices have no real conformance stream, so they are checked only against a model derived from the JM reference decoder and synthetic streams. USAC and HE-AAC multichannel layouts beyond 5.1 are unverified. CABAC with data partitioning cannot occur in a valid stream (the Extended profile requires CAVLC).
- **WebGL.** No compressed textures, `EXT_sRGB` or multi-draw; lines are 1 px; `sin` differs from SwiftShader in the last digit.
- **Canvas.** `getImageData` on a 1280x720 canvas takes about 0.4 s, because QuickJS cannot share typed arrays with Go. EME with Widevine, PlayReady or FairPlay and picture-in-picture reject.
- **Exact pixel identity with Chromium is not a goal.** Screenshots are not byte-identical.

Set `SWEEF_RENDERER=flow` to fall back to the old schematic layout.

## Architecture

```
cmd/sweef              CLI (gui | mcp | serve | fetch | snapshot | shot | eval | call | pushd | version)
internal/engine        Browser, BrowserContext, Tab, Locator, actions, waits,
                       dialogs, routes, storage state, PNG renderer, RSS governor,
                       jsvm.go (QuickJS VM + host bridge), scripts.go (loader + lifecycle),
                       dom_prelude.js (browser API in JavaScript), jsbridge.md (contract)
internal/style         CSS parsing, cascade, computed values, UA stylesheet
internal/render        CSS layout engine and display list (block, inline, flex, grid, table, ...)
internal/raster        Rasterizer, Canvas 2D context, page painter
internal/font          Font loading, metrics, shaping-lite, glyph outlines
internal/gl            Software WebGL 1/2 (state machine, rasterizer) + internal/gl/glsl compiler
internal/media         Container parsing, audio decode, frames; media/webaudio DSP graph;
                       h264, vp8, vp9, hevc, av1 decoders; vp8enc encoder; cdm (key systems)
internal/rtcmedia      WebRTC media pipeline (jitter buffers, NACK/PLI, congestion control)
internal/capture       Camera, microphone and screen capture (V4L2, PulseAudio, compositor)
internal/audioout      Audio output mixer and platform backends
internal/sitestore     Disk-resident per-site storage (IndexedDB, caches, OPFS, quota)
internal/webpush       Web Push client (RFC 8291/8292); internal/pushd the relay
cmd/sweef-cdm          Helper that hosts a user-installed Widevine CDM (the only dynamically linked binary)
internal/net           HTML parser, DOM tree with stable ids, CSS selector engine,
                       locator engines, accessible names, snapshot v2, routes, cookie jar
internal/rpc           SWF1 wire protocol server (framed JSON over TCP/unix)
internal/mcp           MCP server (stdio JSON-RPC, 40+ tools)
pkg/sdk/node           Node.js SDK (Playwright-shaped)
pkg/sdk/py             Python SDK (Playwright-shaped, stdlib only)
```

Concurrency: one VM per tab, serialised; the DOM is behind a tab lock; code that holds the DOM lock never calls into the VM.

Memory: ring buffers for events, 8 MiB cap per document, 1 GiB cap per VM (`--js-mem-mib`, a runaway-script guard: MDN's reference pages peak near 340 MiB while parsing their compatibility data, a runaway allocation loop is stopped within seconds), system fonts mapped rather than copied, and a soft resident-memory budget (`--max-rss-mib`, default 384): the Go runtime gets it as its soft memory limit (never below 192 MiB, so a small budget does not thrash; `GOMEMLIMIT` wins), and above it a governor collects, returns freed memory to the OS and trims the event rings. It is not a hard cap: one heavy page can take more while it is open. `Browser.stats` reports the resident set, the Go heap and `liveDocs` (documents still in memory: it should not grow as you navigate).

## Building and testing

```bash
go build ./...                      # pure Go, no CGO
go test ./...                       # includes golden snapshots of real pages and
                                    # React / Preact / jQuery fixtures under a local server
GOOS=linux GOARCH=arm64 go build -o sweef-arm64 ./cmd/sweef
```

Requires Go 1.26 (the `go` toolchain downloads it automatically when `GOTOOLCHAIN=auto`). The binary has zero runtime dependencies. Measured on 2026-10-11: `go test ./...` runs golden snapshots of six real pages, the selector engine, ref lifecycle, and JavaScript fixtures (inline DOM build, script order, fetch and timers, preventDefault, React 18 counter with a controlled input, Preact, jQuery incl. ajax, dynamic script insertion, ES modules, localStorage, `location.href` navigation, infinite-loop interrupt).

## Roadmap

- Verify on real hardware and sites: YouTube, Google Meet, Zoom, WhatsApp Web, real sign-in popups, Windows and macOS audio, capture and notifications
- Netflix-class DRM: a Widevine signing arrangement, and PlayReady and FairPlay verified on real machines
- Zoom web: an H.264 encoder (it may expect one) and an Emscripten pthreads run
- Binary size: 4.5 MiB of the 40 MiB budget is left (it was 0.7 MiB before the embedded data was compressed). New data must be embedded deflated and measured (`docs/development.md`, "Binary size budget"); four fifths of the file is code and its metadata, so the next big step has to come from code
- Iframes: scrollbars, cross-window DOM access, popups that inherit a sandbox
- `sweef watch` live event stream
- Playwright trace format subset for the trace viewer
- Faster first execution of large framework bundles

## Contributing

Contributions are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md) (setup, workflow, Conventional Commits, DCO),
then [docs/development.md](docs/development.md) for the package map and contracts. Project policy: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md),
[SECURITY.md](SECURITY.md) (report vulnerabilities privately), [GOVERNANCE.md](GOVERNANCE.md), [SUPPORT.md](SUPPORT.md).
What changed and when: [CHANGELOG.md](CHANGELOG.md); how releases are cut: [docs/releasing.md](docs/releasing.md).

## License

Apache License 2.0 (see `LICENSE` and `NOTICE`). It carries an explicit patent grant, which suits a browser engine, and
is compatible with the BSD, MIT and Apache licensed Go modules Sweef depends on.
