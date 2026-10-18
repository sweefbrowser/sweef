# Changelog

All notable changes to Sweef are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and versions follow [Semantic Versioning](https://semver.org/) (while we are `0.x` alpha, minor versions may break
things; the release notes say so when they do).

How to add an entry: see "Changelog" in [CONTRIBUTING.md](CONTRIBUTING.md). Maintainers cut a release by moving
`[Unreleased]` under a new version heading ([docs/releasing.md](docs/releasing.md)).

## [Unreleased]

### Pluggable JavaScript runtime (V8, opt-in prototype)

- New: `Options.JSEngine`, `--js-engine quickjs|v8|auto` and `SWEEF_JS_ENGINE` choose the runtime that executes a page. `quickjs` stays the default and its behaviour is unchanged; `v8` runs pages on V8 through a separate library (`SWEEF_V8_LIB`, or next to the binary) loaded with purego, so the binary stays `CGO_ENABLED=0` and its size is unaffected; `auto` falls back to QuickJS with one line on stderr when the library is missing. Design, feature scope and the measurements that gate the default: [docs/js-runtime.md](docs/js-runtime.md).
- Changed: the prelude uses a native UTF-8 encoder/decoder when the runtime offers one (no change on QuickJS).

### SDKs install the engine themselves (0.2.1)

- `@sweef/browser` and `sweef-browser` 0.2.1 no longer require a manual download: the engine is resolved from `binary`, `SWEEF_BIN`, `sweef` on PATH, a per-user cache, and otherwise downloaded once from GitHub Releases (pinned `0.1.0-alpha.4`, SHA-256 verified before execution, no PATH changes, safe under concurrent first runs). Overrides: `SWEEF_VERSION`, `SWEEF_DOWNLOAD_BASE` (mirrors), `SWEEF_ENGINE_CACHE`, `SWEEF_SKIP_DOWNLOAD=1` (old behavior), standard proxy variables.
- New: `ensureBinary()` / `ensure_binary()`, a `sweef` command from `npm i -g @sweef/browser` and `pip` / `pipx install sweef-browser`, `sweef-install` / `python -m sweef` to pre-fetch (`--print-path`), and a best-effort npm `postinstall` that never fails the install. Still zero dependencies; `import sweef` is side-effect free.

### Script-heavy pages, the agent loop and CLI start

Measured 2026-10-15 on a 4 vCPU Xeon against Chromium 141 / Playwright 1.56 (`bench/realsites/results/*-gaps*`, `replay-2026-10-15.md`, `resources-2026-10-15-replay.md`, `bench/surfaces/results/2026-10-15-gaps/`):

- Mode A (47 tasks x 3 fresh runs, live network): median cold time to a usable snapshot 2939 -> 2188 ms on the 44 tasks both builds pass (Chromium 2029 ms on the 42 both pass); faster than Chromium on 25 tasks (was 16), slower on 17; runs passed 130/141 (Chromium 129/141). Through a local recording (5 runs): GitHub repo 7.7 -> 3.2 s (Chromium 1.2), GitHub issue 7.5 -> 3.3 s (1.5), Next.js docs 5.4 -> 1.9 s (0.7), the3rdacademy.com 4.0 -> 2.1 s (1.0), pipilot.dev 3.3 -> 1.6 s (1.1), Wikipedia 2.1 -> 1.3 s (1.1). Load CPU over ten pages 41 -> 28 s (Chromium 14 s). JavaScript-heavy pages stay slower than Chromium.
- Loading: ancestor bloom filter for selector matching; an inline svg is built only when laid out, without building every other svg for an id union it does not need; inherited SVG properties from an index of only the rules that declare them; a parsed-stylesheet cache of 160 entries with order-shifted reuse; one layout pass where a container-query page needed two; images requested when the document is parsed; layouts forced by page script no longer wait for images, web fonts or script-inserted stylesheets; scripts and modules compiled on up to three worker runtimes as they arrive, read back by a new module loader; the module prefetcher skips non-module specifiers and no longer uses a regexp.
- Readiness: a headless load settles for at most 600 ms (`--nav-settle-ms`, half before the load event) instead of the 1.5 s action budget; router prefetches (`Next-Router-Prefetch`, `Purpose: prefetch`) do not hold a settle; style-only mutations (animation loops) do not keep a settle from going quiet; fill, type, select, check and hover drain what they started instead of a full settle (330 -> ~15 ms per field).
- Fixed: `history.pushState`/`replaceState` never moved the tab URL (the prelude called a host function that did not exist), so `wait --url`, `expect url` and the agent's `wait {url}` never matched after a client-side route change; batch steps ignored their timeouts (a `wait 500` slept 1 s, a `wait url|text` ran 30 s instead of 10 s); a frame about to load counted as idle (the race behind the flaky `TestSandboxedFrameWindowIsReachable`).
- Agent (`sweef agent`): the starting page's snapshot comes with the task (`-no-initial-snapshot` turns it off), the system prompt is 2.8 kB instead of 18 kB and the tool schema 3.3 kB instead of 5.2 kB (Haiku 5.5 caches from 512 tokens), and pages keep running between model turns. The Mode B report shows uncached, cache-read and cache-write tokens for both agents. A new Mode B run is pending.
- CLI: the compiled prelude is embedded (`make embeds`; first eval in a fresh session 155 -> 15 ms in the surfaces flow); new `sweef-cli`, the session client without the engine: `get title` 9.9 -> 4.3 ms (agent-browser 10.2 ms); the agent's regexps and the MCP tool table are built on first use.
- Screenshots encode PNG with one row filter, RGB when opaque (35 -> 9 ms per 1280x800 encode).
- Agent mode also blocks self-hosted analytics hosts (`analytics.`, `stats.`, Plausible, Matomo, Umami paths) and developer-site ad networks.
- Bench: `bench/replay` (record pages once, serve them from localhost), `replay_table.mjs` (median and spread), `resources.mjs` over ten sites; the session CLI smoke test runs on Windows (AF_UNIX probe).

### Agent interfaces: MCP, SDKs and CLI brought up to date, tested and benchmarked

Measured in [bench/surfaces](bench/surfaces/README.md) on 2026-10-15 (4 vCPU Xeon, local fixture unless noted):

- MCP (`sweef mcp`) against `@playwright/mcp` 0.0.83: cold start to first snapshot 20 ms vs 810 ms; the fixture flow (navigate, snapshot, click, snapshot, fill, evaluate, screenshot, back) 22.5 ms vs 1388 ms of call latency and 425 vs 856 text tokens; peak RSS 91 vs 945 MiB. Real script-heavy pages navigate slower in Sweef (GitHub 7.1 s vs 0.9 s, Wikipedia 2.4 s vs 1.0 s); see the report.
- MCP replies are leaner: plain text instead of JSON wrappers, log lines with cursors, a 20k-character cap with an `offset` to continue, image-only screenshots (the Wikipedia text render was 32k tokens per screenshot), and after an action the snapshot lines that changed (`SWEEF_MCP_AUTO_DELTA=0` turns it off). `browser_snapshot` returns the YAML tree; new tools `browser_type`, `browser_scroll`, `browser_reload`, `browser_wait`, `browser_wait_for_url` and `browser_call` (any RPC method).
- Fixed: `browser_emulate` ignored `kind`; armed dialogs were always dismissed (MCP, CLI and SDKs send `action: "accept"`); raw key presses and scrolling from the CLI, batch, MCP and SDKs were no-ops (parameter names); `sweef --json get url` was rejected; inline storage state was ignored.
- Node SDK 0.2.0 (`@sweef/browser`) and Python SDK 0.2.0 (`sweef-browser`): `launch()`, `session()` (shares the CLI's daemon), batch, keyboard/mouse, `getBy*`, waits, routes, bindings, PNG screenshots, `call()`; the 0.1 SDKs called a method that does not exist (`url()`, `title()`) and read every `is*()` as false. Publish-ready, Apache-2.0, not yet published. SDK vs `playwright-core`: process start to first snapshot 77 vs 720 ms.
- Session CLI against `agent-browser` 0.39.1: the flow in 445 ms vs 1554 ms with cold caches and 223 ms vs 1536 ms with warm ones; `eval` with a cold cache directory is slower (155 ms vs 7 ms). New `sweef rpc <Method> [json]`.
- Engine: the first layout of a process no longer reads every system font file (lazy probe plus a font index in the cache directory: first screenshot 208 ms -> 68 ms cold, CLI screenshot 97 -> 35 ms warm); the warm JS VM pool builds in parallel and agent servers keep two ready (SDK evaluate 13.9 -> 0.84 ms); RPC frames are written unescaped in one write.
- Tests: `internal/mcp` (handshake, schemas, a full flow, batch, errors, truncation), a session CLI smoke test, Node and Python SDK suites against a real `sweef serve` (`make test-sdk`, run in CI on Linux).

### Agent browsing: speed and reliability on real sites
Measured on 47 tasks / 41 public sites, 3 fresh-process runs each with empty caches, against Playwright + Chromium 141
(`bench/realsites/results/modea-2026-10-15*.md`): runs passed 95/141 -> 130/141 (Chromium 127/141); median cold time to
a usable snapshot on the tasks both builds pass 2221 -> 2237 ms (flat overall: GitHub now runs its scripts and is slower,
most other sites are faster, e.g. Next.js docs 15.0 -> 6.7 s, MDN 9.7 -> 4.7 s, React docs 6.4 -> 2.4 s, Angular 6.3 ->
3.1 s, Effective Go 5.0 -> 0.8 s). Against Chromium Sweef is faster on 16 of 42 tasks and slower on 26 (median 2.9 s vs
2.1 s); JavaScript-heavy sites (GitHub, Next.js, MDN, the owner's React/Vite sites) stay 1.8x to 4.6x slower.
- Headless settle is bounded and adaptive: it ends after 300 ms of quiet, and requests started after the settle deadline
  no longer extend it; a batch of due timers stops at the pump deadline.
- Entry modules compile and run under their URL (`import.meta.url`, relative `import()`), so GitHub's code view and
  pipilot.dev's chunks load (pipilot.dev used to reload itself in a loop); queued `import()` calls download in parallel
  (GitHub issue page 16 -> 10 s).
- Cheaper layouts: the cross-pass style cache works with `@container` and `:has()`, intermediate container-query passes are
  not finished, CSS background images are fetched in parallel before painting, the SVG filter precheck is exact;
  IntersectionObserver / ResizeObserver deliveries are batched per task.
- Known third-party trackers are not loaded in agent sessions (`--allow-trackers` / `SWEEF_ALLOW_TRACKERS=1` loads them).
- Agent sessions do not preload media or decode a playing video past its first picture (`SWEEF_AUTOPLAY=1` restores it):
  pipilot.dev idle CPU 9.2 -> 0.8 CPU-s per 5 s, peak memory 707 -> 334 MiB (one run before, median of 3 after).
- Platform functions print as native code (the prelude is compiled without its source): Optimizely deleted element.style
  on the-internet.herokuapp.com and jQuery did not start. Fixture flow total 218 -> 199 ms, daemon 58 -> 47 MB.
- Forms submit through the `submit` event (React forms, saucedemo); Enter no longer submits twice; snapshot refs whose
  element was replaced by an equivalent control are healed; waits honour `timeoutMs` and keep the page running;
  `innerText` follows the rendering; `opacity: 0` no longer hides content from snapshots; same-origin links are listed as
  paths and navigation accepts them.
- Web Animations (`Element.animate`) run and finish as a task after the frame; `matchMedia` agrees with CSS (hover and
  pointer are desktop's); scrollbars are hidden in agent screenshots like headless Chromium; viewport screenshots of a
  scrolled page show the scrolled viewport; `-webkit-line-clamp`.
- The snapshot's visibility cascade handles CSS escapes and nesting (Tailwind v4): pipilot.dev's desktop navigation was
  missing and a click on it waited 35 s. `@font-face` `url()format()` without a space loads the font (nextjs.org).
- The agent loop survives an API rejection of a thinking block (every failed Sweef run of the 2026-10-14 Mode B baseline
  ended this way; not yet re-measured) and records the error of failed runs.
- Bench: cold runs start with empty caches (the 2026-10-14 Sweef cold numbers were partly warm), owner-site tasks
  (the3rdacademy.com, pipilot.dev, zaide.com), `fidelity.mjs` (screenshots and interaction parity vs Chromium),
  `resources.mjs` (memory, CPU, idle CPU vs Chromium's process tree), `compare.mjs` (before/after tables), `loadtl`
  `-init/-eval/-run-ms/-memprofile`.

### Added
- **Right-click menus everywhere.** One themed menu (light, dark, incognito) drawn by the chrome above the page, so it can extend
  outside the page area and looks the same on every OS. Arrow keys, Enter, Esc, submenus; it closes on an outside click, scroll,
  resize, window blur and tab change, and a right click elsewhere opens the menu for that spot. Web pages get link, image,
  selection, editable-field, video/audio and plain-page menus (the page sees `contextmenu` first and can cancel it); the tab strip,
  the empty strip, the omnibox, the bookmarks bar, the new tab page (tiles, background), History, Bookmarks, Downloads, Settings
  site rows and the Sidekick (messages, input, conversation) have their own. See [docs/context-menus.md](docs/context-menus.md).
- Tab actions: Duplicate (keeps history), Close other tabs, Close tabs to the right, Mute site, New tab to the right.
- New tab page: Edit shortcut and Remove on top-site tiles (`newtab.sites` setting); bookmarks can be edited from the bar.
- Undo and Redo in text fields; a page that cancels `mousedown` keeps the focus where it was.
- `Inspect` opens Developer Tools for the tab.
- **The Popover API** (`popover`, `showPopover` / `hidePopover` / `togglePopover`, `popovertarget`, `:popover-open`, `beforetoggle` /
  `toggle`): a closed popover is no longer painted (YouTube's tooltips showed as grey squares). See [docs/web-platform.md](docs/web-platform.md).

### Fixed
- **youtube.com renders and plays.** Search results and the watch page show real titles, thumbnails and avatars, and the player plays
  AV1 + Opus through MSE (see "youtube.com" in [docs/compat.md](docs/compat.md) for what was measured and what is still wrong: no
  icons in the mini guide, no sound check, slow first paint, and a home page without videos because the server sends none to a visit
  without cookies). Each of these was a difference from browsers that a library YouTube ships depends on:
  - `host` and `mode` moved from `DocumentFragment.prototype` to `ShadowRoot.prototype`, and the `Node.prototype` accessors
    (`firstChild`, `lastChild`, `childNodes`, `parentNode`, `nextSibling`, `textContent`, ...) work on fragments: the Shady DOM
    polyfill failed with an empty `Uncaught` in `attachShadow` and saw every shadow root as empty.
  - An uncaught value is printed as `Name: message` plus the stack, or, for a thrown string, number, object or `null`, with its
    type (`Uncaught non-Error string thrown: "..."`); engine stacks have no header, so errors used to read as an empty `Uncaught`.
  - `Element.attributes` is array-like (`Array.prototype.map/slice/forEach.call(el.attributes)` returned holes).
  - Custom elements in `<template>` content and in nodes made by the template's owner document are inert (not constructed until
    imported or inserted), as in browsers; Polymer walks its templates with bindings still in the attributes.
  - Reflected properties live on the interfaces that own them (`href`, `src`, `data`, `name`, `type`, ... no longer exist on every
    `HTMLElement`), `form`/`labels`/`validity`/`willValidate`/`checkValidity` are on form controls only, and
    `createElement(name, {is})` no longer sets an `is` attribute. Polymer Resin replaced the `data` of every custom element with
    its placeholder because of these.
  - A same-origin `<iframe sandbox="allow-same-origin">` has a window the embedder can use (`contentWindow.history`); its own
    scripts and inline handlers stay blocked. `bind`/`call`/`apply` of a method taken from another window run on the caller's
    `this`: a page that borrows an iframe's `History.prototype.replaceState` and binds it to its own `window.history` no longer
    fails with "origin 'null'".
  - A `position: fixed` box inside an absolutely positioned one is laid out (it stayed 0x0: YouTube's masthead was invisible).
  - A lowercase `viewbox` attribute next to the real `viewBox` of an inline SVG no longer overrides it (the logo was drawn at the
    wrong scale).
  - The default single-task script timeout is 30 s instead of 5 s (`--js-timeout-ms`): YouTube's first render has tasks longer than
    5 s on a loaded machine, and the interrupted task left the watch page at "Video unavailable".
- The embedded UI archive (`internal/ui/web.zar`) was out of date with `chrome.css`; regenerated (`make embeds`).
- **Windows: the Start-menu and desktop shortcut started a console that closed at once.** The GUI launcher (`Sweef.exe`) and the
  command-line binary (`sweef.exe`) differ only in case, so on Windows (and on macOS inside `Sweef.app`) one overwrote the other.
  The launcher is now `SweefLauncher.exe` / `SweefLauncher`. This also explains why the macOS `.app` failed `codesign`.
- Windows showed two sets of window buttons (the native title bar and one drawn by the page chrome); only the native ones remain.
- Response bodies were cut silently at 8 MiB, which broke pages with large scripts (YouTube's main script is about 11 MB) with a
  `SyntaxError` at the cut. The limit is now 128 MiB.
- A tab that left its `sweef://` page no longer logs a `ReferenceError` for every UI state update.
- The GUI writes panics and fatal runtime errors to `crash.log` (`%LocalAppData%\Sweef` on Windows).
- **Developer Tools.** `F12`, `Ctrl+Shift+I` (`Cmd+Opt+I` on macOS), the menu (More tools, Developer tools) and the command
  palette open a resizable panel docked under the page (the height is the setting `devtools.height`). Console: live
  `console.*` and uncaught errors with level and text filters, `file:line`, stacks and repeat counts, plus a prompt that
  evaluates in the page with history. Network: every request with status, type, size, time, cache state (`x-sweef-cache`)
  and a waterfall, entries blocked by the tracker blocker marked, a preserve-log switch, request and response headers and a
  text, JSON or image preview. Elements: the DOM tree, the matched CSS rules (with the losing declarations struck through),
  computed style and box model, the selected node highlighted on the page, and live edits of attributes, text, nodes and
  `element.style`. Sources: the page's scripts and stylesheets with line numbers and a pretty printer. Application: cookies,
  `localStorage`, `sessionStorage`. Windows and macOS are unverified at runtime. Adds 148 KB to the Linux binary
  (37,175,422 to 37,326,974 bytes stripped). See [docs/devtools.md](docs/devtools.md).
- Engine inspection API on `Tab` (`internal/engine/devtools_*.go`): a network recorder with bodies under a budget, the DOM and
  CSS views, a node highlight drawn into the page's display list, an evaluator that describes its result, sources and storage.
  Console entries carry the script and line they came from when the engine knows it.
- App entry points for a page context menu's "Inspect": `openDevTools(tabID)` and `openDevToolsAt(tabID, x, y)`.

### Fixed
- An uncaught error raised from a listener, timer or microtask is logged with its name and message, not only its stack.
- The app no longer evaluates its UI messages (state patches, which now include the Developer Tools binding) in a tab whose
  document is a web page, which could have defined `window.sweef` while the tab was still on the internal-page list.
- Scrolling: the page texture is a window (band) around the view instead of the whole prefix of the page, which removes the
  80 ms to 1.7 s repaints on deep pages and the blank frames of a fast flick (long text page, headless 1920x1080: repaint p50
  80-150 ms to 17-23 ms, blank frames 348/425 to 0). Wheel notches and arrows use a critically damped spring, precision
  touchpad deltas follow the finger and then fling with exponential decay, PageUp/PageDown/Space/Home/End ease. Scrolling
  chains inner scroller, frame, viewport and honours `overflow` and `overscroll-behavior`. `scroll` / `scrollend`,
  `scroll-behavior: smooth`, `scrollTo/By/IntoView` and fragment navigation work. Wheel events reach the page first
  (`preventDefault` cancels; window/document/body wheel listeners are passive by default).
- Text selection: drag with autoscroll, double click (word), triple click (paragraph), shift+click, shift/ctrl+shift+arrows,
  Ctrl/Cmd+A, a highlight painted from the layout (so it matches `getSelection()` / `Range`), `user-select: none`, text controls
  with `selectionStart/End`, `setSelectionRange`, `setRangeText`, find-in-page highlights with the active match scrolled in.
- Clipboard: copy (Ctrl+C, Ctrl+Insert) puts `text/plain` and `text/html` on the system clipboard, cut (Ctrl+X, Shift+Delete),
  paste (Ctrl+V, Shift+Insert) with cancelable `copy`/`cut`/`paste` events, a paste sanitiser for `contenteditable`, undo/redo
  (Ctrl+Z, Ctrl+Y). `shell.RichClipboard`: CF_HTML on Windows, `text/html` on X11, `public.html` on macOS.
  The Windows and macOS clipboard code is UNVERIFIED: it builds (GOOS=windows/darwin) and the CF_HTML framing is unit tested,
  but it was not run on those systems.

### Fixed
- A page-initiated navigation whose load was started by the page pump before it was reported to the app could stay "loading"
  forever.

### Known gaps
- `::selection` colours (the default colours are used), caret blink, wheel scrolling of a textarea, horizontal viewport scroll,
  `position: fixed` elements inside the scrolled page texture, selection painting inside frames, smooth programmatic scrolling
  of inner elements (instant).

## [0.1.0-alpha] - 2026-10-14

### Security
- State is partitioned by top-level site: cookies, `localStorage`, IndexedDB, Cache Storage, the Origin Private File System, service
  worker registrations, `BroadcastChannel`/`SharedWorker`/Web Locks namespaces and the HTTP cache are keyed by (origin, top-level site)
  in a third-party context. Third-party cookies are neither sent nor stored unpartitioned; `Partitioned` and `SameSite` are honoured;
  the Storage Access API is wired to the policy ([docs/storage-partitioning.md](docs/storage-partitioning.md)).
- `document.cookie` no longer exposes `HttpOnly` cookies; `ClearCookies("")` clears all cookies; the Origin Private File System is no
  longer shared between a private window and the normal one.

### Added
- **A real `Intl`.** `NumberFormat`, `DateTimeFormat`, `PluralRules`, `RelativeTimeFormat`, `ListFormat`, `DisplayNames`,
  `DurationFormat`, `Collator` and `Segmenter` now run on CLDR 47 data for 73 locales with the IANA time zone database
  embedded, instead of an English-only shim; `toLocaleString`, `toLocaleDateString` and `localeCompare` use them. Compared with
  Node 22 case by case (`tools/intlgen`, `internal/engine/intl_test.go`); gaps are listed in
  [docs/web-platform.md](docs/web-platform.md). Adds about 1.2 MB to the binary.
- **`Intl` parity with Chromium.** Measured against Node 22 (ICU 77, CLDR 47) on seeded random matrices (`node tools/intlgen/matrix.js`,
  same seeds before and after): `NumberFormat.format` matches in 99.6% of cases (was 93.4%), `formatRange` 99.4% (was 24.8%),
  `formatToParts` 99.5% (was 75.7%); `DateTimeFormat.format` 98.3% (was 57.4%), `formatRange` / `formatRangeToParts` 98.1% (was 41.2% /
  36.1%), `formatToParts` 98.6% (was 57.3%), `resolvedOptions` 99.3% (was 79.6%); `Collator` 99.85% of comparisons (was 93.05%) and
  100% of resolved locales (was 80.5%). What changed: the date and time pattern of every locale now comes from a port of ICU's
  `DateTimePatternGenerator` (the glue between date and time, `dayPeriod`, hour cycles) and ranges from a port of `DateIntervalFormat`
  (`platformjs/intlpat.js`, `intlrange.js`); time zone names follow ICU's specific, generic, location and localized-GMT rules with
  metazones; the `japanese`, `roc`, `islamic` (all variants), `hebrew`, `indian`, `coptic`, `ethiopic` and `ethioaa` calendars join
  `buddhist` and `persian` (non-Gregorian calendars in the locales that ship their names: 98.1% of 4,000 cases, was 16.5%);
  numbering systems carry their own patterns and symbols; number ranges collapse shared currency symbols, units and compact text the
  way ICU does and use plural ranges; compact currency, percent and unit parts are split into the parts Chrome reports; collation
  follows ICU's root order for the Arabic, Hebrew, Indic, Thai, Lao, Khmer, Myanmar, Georgian, Armenian, Greek, Cyrillic, Hangul and
  kana scripts, reorders a locale's own script first, orders Chinese by pinyin (GB 2312) or stroke (Big5 level 1), and resolves to the
  locale ICU has collation data for. `tools/intlgen/golden.js` now writes about 2,900 cases from Node's output
  (`engine/intl_test.go`), `segcases.js` the segmentation cases of `internal/intl/segment`. The binary grows by 180,224 bytes
  (41,234,558 to 41,414,782). Still missing: dictionary word breaking for Thai, Lao, Khmer, Burmese, Chinese and Japanese (ICU's
  dictionaries are 93 KB to 1.6 MB gzipped, `tools/intlgen/icudict.js`), the `chinese` and `dangi` calendars, and non-English time zone
  city names ([docs/web-platform.md](docs/web-platform.md)).
- **Video-call and player platform APIs.** Fullscreen API with the app giving the page the window; Media Session wired to
  a now-playing control; `ManagedMediaSource`; Async Clipboard API with a per-site permission, copy/cut/paste events,
  `DataTransfer`, `FileList`; a real `WebSocket` client (binary, fragments, close codes, backpressure); native
  `btoa`/`atob`; faster IndexedDB cursors; page visibility; echo cancellation, noise suppression and automatic gain on
  the WebRTC send path; `MediaStreamTrackProcessor`, `MediaStreamTrackGenerator` and `VideoTrackGenerator`; WebRTC
  `MediaCapabilities`. The compatibility status of Google Meet, WhatsApp Web and YouTube, with what was and was not tried
  against the live sites, is in [docs/compat.md](docs/compat.md).
- **`MediaRecorder` and `BlobEvent`.** Record `getUserMedia` / `getDisplayMedia` streams, `canvas.captureStream()` and
  `MediaStreamAudioDestinationNode` streams to WebM (VP8, Opus) or Ogg Opus (RFC 7845), with `timeslice`, `pause()`,
  `resume()`, `requestData()`, `audioBitrateMode`, the specified state machine and `InvalidStateError` / `NotSupportedError` /
  `InvalidModificationError` behaviour, an honest `isTypeSupported()`, and a stop when every track ends. Encoding (a streaming
  resampler, an Opus mixer, VP8, WebM and Ogg writers) runs off the JS thread in the new `internal/mediarec`; the chunks
  concatenate into a file that plays in `<audio>` / `<video>` and decodes with the repo's demuxers. Voice-note flows such as
  WhatsApp Web's no longer hit `NotSupportedError` (the real site was not tried). No MP4, VP9 or H.264 recording; only the
  first video track is recorded ([docs/mediarecorder.md](docs/mediarecorder.md)).
- **Iframes render and take input.** Every `<iframe>` is now a child document with its own JavaScript VM, layout and
  paint, drawn into the parent at the iframe's box (clipped, with the transforms, opacity and radius of its ancestors),
  scrollable inside its box, repainted when it changes, and loaded lazily for `loading="lazy"`. `srcdoc`, nested frames,
  `sandbox` (scripts, same-origin, popups, forms, top navigation), `X-Frame-Options` and CSP `frame-ancestors` are
  supported. Pointer, wheel, keyboard and text input are routed to the frame under the pointer or holding focus, in the
  desktop browser and through the engine API (`MouseClick`, `Keyboard`, `WheelAt`). Not supported: access to another
  window's DOM beyond what same-origin proxies forward, process isolation, scrollbars inside frames. See
  [docs/frames-popups.md](docs/frames-popups.md).
- **`window.open` and popups.** `window.open` returns a `WindowProxy` (`opener`, `name`, `closed`, `close()`, `focus()`,
  `postMessage`, `location`, same-origin property access and calls); links and forms honour `target` (`_blank`, `_top`, `_parent`, frame and window
  names, `rel=opener` / `noopener`); windows open only during a user gesture (5 s in the engine, 2 s in the app, one
  popup per gesture) and a blocked popup is recorded (`Page.popups`) and shown as a toast; `window.close()` closes windows
  that script opened; `postMessage` works across frames, the opener and popups with `targetOrigin` and structured clone
  of common types; `Cross-Origin-Opener-Policy` can sever the opener. In the desktop browser a popup is a tab next to
  its opener with an accent bar in the tab strip. An OAuth round trip (popup, cross-origin redirect back, `opener.postMessage`,
  `window.close`) is covered by tests against local servers; real Google or Facebook sign-in was not tried. Not supported:
  small-window features (size, position).
- Agent: the snapshot includes the content of frames with frame-qualified refs (`f1e5`), and click, fill, type, press and the
  other ref actions reach elements inside same-origin and cross-origin frames. The Sidekick reads a frame only when the
  site of the frame is one the person allowed. `Page.frames` and `Page.popups` RPC methods.
- Cross-origin isolation, `SharedArrayBuffer` and `Atomics` across the page and its dedicated Workers: `crossOriginIsolated` follows
  COOP `same-origin` plus COEP `require-corp` / `credentialless`, `SharedArrayBuffer` exists only then, `Atomics.wait` blocks in
  Workers (and throws on the page), `notify` and `waitAsync` work across VMs. COEP is enforced for cross-origin scripts,
  stylesheets, images and media, frames and Worker scripts; `credentialless` drops credentials. Not done: shared memory with
  SharedWorkers or Service Workers, COOP browsing-context-group switching ([docs/web-platform.md](docs/web-platform.md)).
- WebAssembly threads: shared `WebAssembly.Memory`, atomic instructions and `memory.atomic.wait/notify`, and posting a shared
  `Memory` or a `Module` to Workers, on cross-origin isolated pages. Verified with hand-assembled modules, not with an
  emscripten or wasm-bindgen program ([docs/webassembly.md](docs/webassembly.md)).
- WebCodecs: `VideoDecoder` (VP8, VP9, H.264, HEVC, AV1), `AudioDecoder` (Opus, AAC, MP3, Vorbis, PCM, A-law, mu-law),
  `VideoEncoder` (VP8 only), `AudioEncoder` (Opus only), `VideoFrame`, `AudioData`, `EncodedVideoChunk`, `EncodedAudioChunk`,
  `ImageDecoder` and `isConfigSupported`, off the JS thread, with frames drawable to canvas and creatable from canvas,
  `ImageBitmap` and `<video>`. No H.264/VP9/AV1/AAC encoder and no hardware paths ([docs/webcodecs.md](docs/webcodecs.md)).
- Encrypted Media Extensions with Clear Key: `requestMediaKeySystemAccess('org.w3.clearkey')`, `MediaKeys`, `MediaKeySession`
  (temporary sessions), `setMediaKeys`, `encrypted` / `waitingforkey` events, and `cenc`, `cens`, `cbc1` and `cbcs` decryption of
  fragmented MP4 in the MSE pipeline. Widevine, PlayReady and FairPlay still reject with `NotSupportedError`; no persistent
  sessions, no encrypted WebM, no encrypted progressive `src` ([docs/media-streaming.md](docs/media-streaming.md)).
- `h264.Decoder.SetThreads` / `Flush` and `yuv.Frame.Wait`; a 1080p High profile H.264 fixture with ffmpeg MD5s
  (`internal/media/testdata/h264_1080p_high.mp4`), differential tests for the new kernels (motion compensation, CABAC,
  deblocking, VP9 loop filter, video blit) and benchmarks (`BenchmarkH264Decode1080p`, `BenchmarkConvert1080p`,
  `BenchmarkDrawVideoFrame*`); `TestPlaybackQuality` can paint the page while it plays (`SWEEF_BENCH_PAINT=WxH`).
- Dedicated Workers run on a full script VM of their own goroutine: IndexedDB, Cache Storage, `navigator.storage`, the Origin Private
  File System with `FileSystemSyncAccessHandle`, `fetch`, WebAssembly and nested workers work inside them; real
  `SharedWorker`, `BroadcastChannel` and `navigator.locks`; structured clone with transferred `MessagePort`s and `ArrayBuffer`s.
- `<iframe>` documents are loaded as nested browsing contexts: separate origin, window and VM, `postMessage` with origin checks,
  `parent`/`top`/`frameElement`, same-origin `contentWindow`/`contentDocument`, `sandbox`, `srcdoc`, `X-Frame-Options` and CSP
  `frame-ancestors` ([docs/storage-partitioning.md](docs/storage-partitioning.md)); painting and input are in the entry above.
- `BrowserContext.SetPartitionPolicy`, `GrantThirdPartyCookies`, `engine.SetHostMap` (resolver override), `Options.DisableFrames`.
- Real camera, microphone and tab/window capture for `getUserMedia`, `getDisplayMedia` and `enumerateDevices`: V4L2 cameras and PulseAudio/PipeWire microphones on Linux (pure Go), a permission prompt with per-site choices and in-use indicators, `SWEEF_FAKE_DEVICES=1` test devices and `capdemo`; Windows and macOS backends are included but unverified on hardware (`docs/capture.md`).

- Notifications: `Notification`, `showNotification`/`getNotifications`, `notificationclick`/`notificationclose`,
  `clients.openWindow`, `WindowClient.focus/navigate`, per-site permission with a prompt next to the address bar (quiet UI
  without a gesture, pause after three dismissals), a site info entry, Do not disturb, Permissions API for notifications
  and push. Delivery through `org.freedesktop.Notifications` on Linux (tested against a real `dbus-daemon`), balloon
  notifications on Windows and `NSUserNotification` on macOS (compiled only), and an in-window toast otherwise
  ([docs/notifications.md](docs/notifications.md)).
- Web Push: `PushManager`, `PushSubscription`, `push` and `pushsubscriptionchange` events, RFC 8291 decryption and
  RFC 8292 VAPID (checked against the RFC test vectors), an autopush client, and `sweef pushd`, a self-hostable push
  relay. No push service is built in: set `SWEEF_PUSH_SERVICE` or the `push.service` setting
  ([docs/push.md](docs/push.md)).
- Background Sync (retry 5 min then 15 min, three attempts, `lastChance`), Periodic Background Sync (12 hour floor,
  granted to bookmarked sites), `navigator.setAppBadge`, Web Locks across tabs and workers
  ([docs/background-sync.md](docs/background-sync.md)).
- Apache-2.0 `LICENSE` and `NOTICE`.
- Contribution and project health files: `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `SUPPORT.md`,
  `GOVERNANCE.md`, `CODEOWNERS`, issue forms, pull request template, `Makefile`, `.editorconfig`.
- Automation: CodeQL, `govulncheck`, dependency review, Dependabot, pull request title check, gofmt check.
- `docs/development.md` (package map and contracts), `docs/releasing.md`, and an ADR log in `docs/adr/`.
- `appbench -open-profile` times the key that opens the omnibox dropdown and the key that closes it separately and
  writes a CPU profile.
- Sound for the desktop browser: a pure-Go audio mixer with an `oto` device backend, per-tab audible indicator and mute,
  and the audio clock driving `<video>`/`<audio>` and Web Audio (`docs/audio-output.md`).
- Adaptive streaming: MSE rebuilt on parsed coded frames with a native player, decode-ahead and late-frame handling,
  measured decode and dropped-frame numbers, and a per-site status table (`docs/media-streaming.md`); EME/DRM is not supported.
- WebRTC on pion (pure Go): `RTCPeerConnection`, data channels, transceivers, stats; local IPs hidden behind mDNS by
  default, no STUN unless the page configures it ([docs/webrtc.md](docs/webrtc.md)).
- WebAssembly JavaScript API on wazero (pure Go): `Module`, `Instance`, `Memory` (an ArrayBuffer over the live linear memory), `Table`, `Global`, `validate`/`compile`/`instantiate` and the streaming variants, in pages and Web Workers; no threads, `Tag` or `externref` ([docs/webassembly.md](docs/webassembly.md)).
- IndexedDB, `navigator.storage` and Cache Storage, persisted per origin under the profile's `storage` directory (append-only logs, response bodies on disk, quota, wiped by "Clear browsing data", memory-only in private windows); see docs/storage.md.
- Service workers: `navigator.serviceWorker`, the install/activate/fetch/message lifecycle, real interception of navigations, `fetch()`, XHR and sub-resource loads, registrations that survive a restart; see docs/service-workers.md.
- Web platform layer (see `docs/web-platform.md`): real Streams plus Compression/Decompression and TextEncoder/TextDecoder streams, XPath, URLPattern, the Navigation API, ElementInternals, View Transitions, the CSS Highlight and `@property` registries, `Array.fromAsync`, `scheduler`, the Origin Private File System and honest stand-ins for device, credential, payment and Picture-in-Picture APIs, all loaded lazily.
- WebRTC video and audio: a pure Go VP8 encoder, VP8 and Opus send and receive with jitter buffers, NACK/PLI and bandwidth estimation, `getStats` media entries ([docs/webrtc.md](docs/webrtc.md), [docs/webrtc-interop.md](docs/webrtc-interop.md)).

- Quota management for site storage: a global budget (60% of the profile volume's available space, at most 64 GiB) with at most 20% per origin, exact usage accounting across IndexedDB, Cache Storage, the origin private file system and service worker scripts, `QuotaExceededError` on writes over the cap, eviction of whole least-recently-used best-effort origins under pressure (open connections get `close`), a working `navigator.storage.persist()` decided by the application (bookmarked sites, or a prompt the embedder installs), `estimate()` with real usage and quota, and a per-site storage list with a "Clear" button in Settings > Data. `engine.Options.StorageBudgetMiB`; see docs/storage.md.
- ADR 0002 (the ordered store, with measurements) and `tools/kvbench`, the harness behind it.
- `internal/media/cdm`: the key system layer behind Encrypted Media Extensions, one Go interface for every key system
  (`Registry` with capability negotiation, `Backend`/`Session`, events, `Decrypt`, optional decrypt-and-decode and platform
  renderer interfaces), with Clear Key implemented in full (cenc, cens, cbc1, cbcs). The JavaScript EME API is not part of
  this change. Contract and status: [docs/drm.md](docs/drm.md).
- Widevine host (`com.widevine.alpha`): `cmd/sweef-cdm`, a separate dynamically linked helper process that loads the Widevine
  CDM the user already has (explicit path or `SWEEF_WIDEVINE_CDM`, else the WidevineCdm directory of an installed Google
  Chrome, Chromium or Edge) through purego and implements Chromium's CDM host ABI (interface versions 10, 11, 12), and a
  framed stdin/stdout protocol to the browser, so the Linux browser binary stays static. A VMP signature next to the helper
  (`sweef-cdm.sig`, as castLabs EVS writes it) is passed to the CDM; no signature is ever fabricated. Sweef does not bundle or
  download a CDM. **Not tested against a real Widevine library** (none in the build environment): the host is tested against
  a fake CDM that implements the same binary interface in Go. The desktop CDM is software (L3), typically capped at 720p by
  services.
- PlayReady (Windows, Media Foundation) and FairPlay (macOS, AVContentKeySession) backends, **unverified and compile-checked
  only**, off unless `SWEEF_PLAYREADY=1` / `SWEEF_FAIRPLAY=1`; license exchange only, playback through the platform renderer
  is not implemented.
- `scripts/release/build.sh` also builds `sweef-cdm` next to `sweef` (`--skip-cdm-host` to omit); packages do not install it yet.
- Binary-size tooling ([docs/development.md](docs/development.md), "Binary size budget"): `tools/sizeprof` (sections, bytes per
  package, largest symbols of a build), `tools/startbench` (start-up, first page, first frame, peak RSS and CPU time of two
  binaries, run alternately), `go test ./cmd/sweef -run BinarySizeBudget` (fails over the budget or when the binary grows
  more than 1 MiB past `scripts/release/size-baseline.txt`), headroom and the per-package table in
  `scripts/release/build.sh`, `make size` and `make embeds`. `internal/zpack` is the deflated-embed format.

### Changed
- A page that embeds a third-party frame now makes real requests for it, unless the Data Saver's click-to-load frames are on.
  The README's data-saver comparison with Chromium was measured before frames were loaded and still says Sweef fetches no
  iframe documents.
- `Tab.Keyboard` types into the focused element (it dispatched to the document before).
- `window.open` from page script is no longer "open a tab from the chrome, return null": it goes through the engine
  (popup blocker, opener link, named windows).
- The release binary-size budget is 40 MiB (was 32 MiB): WebRTC (pion), WebAssembly (wazero) and the web platform layer add about 12 MiB. The stripped linux/amd64 build is about 34.7 MiB.
- Embedded data is deflated and inflated on first use (`internal/zpack`, standard-library `compress/flate`): the JavaScript
  preludes and platform modules (`NAME.js.z`), the chrome UI (`internal/ui/web.zar`), the eight Go fonts
  (`internal/font/gofont.zar`) and the IANA time zone database (`internal/intl/zoneinfo.zar`, which replaces the
  `time/tzdata` import; the operating system's database is still looked at first). **After editing a `.js` file under
  `internal/engine` or a file under `internal/ui/web`, run `make embeds` and commit the generated files**; the tests fail
  with that instruction when they are stale. The prelude bytecode cache is now keyed by the deflated bytes, so its file
  names changed once.
- The embedded Go fonts no longer carry their TrueType hinting program (`fpgm`, `prep`, `cvt ` and the glyph instructions,
  more than half of the glyph data); Sweef uses unhinted outlines everywhere and every glyph, advance, mapping and name is
  compared with the x/image originals by tests.
- Sidekick's HTTP/2 health check uses `http.Transport.HTTP2` (`SendPingTimeout`, `PingTimeout`) instead of importing
  `golang.org/x/net/http2`, which linked a second HTTP/2 stack.
- `keymap.ComposeDead` (X11 dead keys) answers from a generated table of the NFC compositions of the dead keys' accents
  instead of linking `golang.org/x/text/unicode/norm`; checked against NFC for every code point of the first three
  planes. It returns "" for a combining mark that is not a dead key's (the package never passed one).
- The Opus theta tables in `third_party/gopus` are `int16` (every value fits; the compiler checks).
- The release build compiles the connection set-up and control-plane packages without inlining
  (`scripts/release/noinline-packages.txt`) and links with `-funcalign=16`. A plain `go build` is unchanged.
- IndexedDB, Cache Storage and the origin private file system are stored in one disk-resident ordered key-value store per origin (goleveldb) instead of an append-only log replayed into memory: memory no longer depends on database size (a 1 GB database opens in 44 ms and reads at about 30 MB resident), cursors stream, transactions commit as one atomic batch, `clear()` and `deleteDatabase` are O(1), values and cache bodies above 64 KiB are files, and a crash or kill leaves each transaction whole or absent. The origin private file system now persists across restarts and is no longer limited to 256 MiB of memory.
- `navigator.storage.persist()` no longer always succeeds in a persistent context: it asks the application's policy and answers no without one. `StorageQuotaMiB` is now a cap per origin that overrides the derived one (0 = derived), not a fixed 1 GiB.
- README shows the real browser UI (first-run setup with the real browser logos, Sidekick, Data Saver, new tab, omnibox), rendered by `uipreview`.
- The omnibox dropdown uses a lighter shadow (the `--sh-2` token plus a 1 px ring instead of `--sh-3`).

### Fixed
- `fetch` and `XMLHttpRequest` send the request headers page code sets (Supabase `apikey`, `Authorization`); they were dropped.
- `<img>` elements in the document fire `error` when the response is not an image and report their real `naturalWidth`/`naturalHeight`.
- Images with `max-width: 100%` (Tailwind's reset) are no longer 0 px wide in inline-block, inline-flex, float, flex-item and max-content boxes.
- Grid area and line names keep their case (Wikipedia's camelCase areas now place their items).
- `sweef eval`, `sweef console` and `sweef network` print results again (the RPC returns `.result`, `.messages`, `.entries`), and the first navigation of a session loads the page once, not twice.
- `getComputedStyle` answers from the cascade for display, position, sizes, margins, padding, borders, colours, fonts, line-height, opacity and z-index.
- Variable web fonts paint at the requested weight (they drew their default instance), and the `opsz` axis follows the font size.
- Absolutely positioned children of flex containers take their static position from `justify-content` and `align-items`/`align-self`.
- Quirks-mode pages no longer grow a needless scrollbar when the first child's top margin collapses through `body`; `document.compatMode` reports `BackCompat` for them.
- Selectors the DOM builds for ids and names escape backslashes and line breaks, and the selector parser understands CSS hex escapes; `CSSStyleSheet.insertRule` keeps nested braces in the declaration block.
- A download name cannot leave the downloads directory (`../x`, `..\x`), clicking a link never navigates the top level to `data:` or runs `vbscript:`, and the settings page ignores section names it does not define.

### Performance
- **The stripped linux/amd64 release binary is 4.11 MiB smaller**: 36,921,470 bytes (35.21 MiB) against 41,234,558
  (39.32 MiB), so 4.79 MiB of the 40 MiB budget are left instead of 0.68 MiB, with every feature and the same output. Each
  step, measured on its own build in this order: JavaScript preludes and platform modules deflated 1,105,920 bytes; chrome
  UI and time zone database deflated 491,520; Opus theta tables as `int16` 196,608; Go fonts deflated 729,088; no
  `x/net/http2` 376,832; Go fonts without hinting programs 266,240; no `x/text/norm` 102,400; control-plane packages
  built without inlining 757,760; `-funcalign=16` 286,720. The other release targets save 3.88 MiB (linux/arm64), 3.92
  (darwin/arm64), 3.89 (windows/arm64), 4.15 (darwin/amd64) and 4.11 (windows/amd64); the tightest target is now darwin/amd64
  with 4.34 MiB left (0.19 MiB before). Start-up did not get slower (medians of alternating runs, old and new binary):
  `sweef version` 7.5 to 7.4 ms; first page with an empty bytecode cache 343.1 to 341.2 ms; the same page with a warm cache
  177.5 to 165.4 ms (it no longer hashes 1.2 MB of script); headless start to first UI frame 1173.4 to 1174.5 ms; peak RSS
  0.8% to 7% lower in every case. The in-process benchmarks of the packages built without inlining stayed within noise
  (`BenchmarkChromeKeystroke` 10.89 to 10.88 ms best of eight; net/http and crypto/tls microbenchmarks within 10% on
  microsecond operations). Details, rejected ideas and the method are in
  [docs/development.md](docs/development.md).
- H.264 video decoding uses frame threads (up to one per core, at most four; `SWEEF_H264_THREADS` overrides, 0 turns them
  off), a faster deblocking filter, motion compensation, CABAC engine and display conversion. Measured on a shared
  4 vCPU VM, medians of five alternating runs of the old and the new build (frames per second of the whole decode /
  CPU time per frame): 720p30 clip 54 to 167 fps (23.5 to 15.4 ms), 1080p30 clip 24.8 to 97 fps (50.7 to 33.1 ms),
  1080p60 clip 28 to 130 fps (46.7 to 27.2 ms). Every picture is identical to the old decoder's (ffmpeg MD5s of a new
  1080p fixture, the repository's conformance streams, 2, 3 and 4 threads, `-race`). Details in
  [docs/media-streaming.md](docs/media-streaming.md).
- Native player, 20 s of 1080p30 H.264 through the page: dropped frames 21%, 53% and 43% before, 0.5%, 0.3% and 12% after
  (load average 3.6 to 7; the 8.7% in the previous measurement was at load 1), and 1080p60 59 to 67% before, 1.8% (load 4)
  to 36% (load 14) after. Painted into a fullscreen box with the page repainting at 60 Hz: 40 to 68% dropped before,
  0% after.
- Video frames are converted and drawn much faster: the RGBA conversion of a 1080p picture 7.6 to 8.8 ms to 3.4 to 3.8 ms
  (four goroutines), drawing it scaled into a 1920x1080 / 1280x720 / 640x360 box 28 to 30 / 19 to 22 / 6.5 to 7.8 ms to
  0.55 / 2.5 to 3.4 / 0.5 to 0.8 ms (pixel-identical to the general image shader), one page paint with a 1080p picture
  40 to 87 ms to 8 to 12 ms (median); opaque solid fills (page backgrounds) replace pixels instead of blending them.
- VP9 1080p: 56 to 44 ms of CPU per frame (48 fps both before and after; the frame rate was already above real time) from
  a loop filter without per-sample overhead, inter prediction written into the frame and waits that sleep instead of
  yielding in a loop (which used 46% of the CPU of a profile taken on a busy machine). The H.264 deblocking wavefront
  uses the same waits.
- `navigator.mediaCapabilities` reports H.264 up to 1080p30 as smooth (the pixel-rate budget was 720p30).
- Opening the omnibox dropdown: rows and section headings are pooled and reused, and rows no longer get `id`/index
  attribute writes when they move (about 13.0 ms to 11.3 ms median in-process at 1x with history present; the shadow
  change takes roughly another 1 ms off).

### Security
- Updated `golang.org/x/net` from v0.17.0 to v0.61.0 (HTML parser denial-of-service and XSS-class advisories GO-2024-3333, GO-2025-3595, GO-2026-4440, GO-2026-5025 to 5030, HTTP/2 GO-2024-2687, GO-2026-4918, IDNA GO-2026-5026) and the Go toolchain to 1.26.9 (about 30 standard library advisories in `net/http`, `crypto/tls`, `crypto/x509`, `net/url`, ...). `govulncheck ./...` now reports no vulnerabilities; it runs weekly and on every PR.

### Removed
- The append-only log format of IndexedDB and Cache Storage. It never shipped; leftover `idb/` and `caches/` directories are deleted when found, and a store in an unknown or unrecoverable format is kept as `kv.bak` while a new one starts.
- A 32 MB `appbench` binary that had been committed by mistake (it stays in git history).

## Pre-release history (first alpha baseline, included in 0.1.0-alpha)

First public alpha of the desktop browser. Everything below is measured or marked as not verified in the README.

### Added
- **Desktop browser:** chrome (tabs, omnibox, bookmarks, history, downloads, find, settings, onboarding with real browser
  logos for import) written as HTML/JS and drawn by Sweef's own engine; native window shells for X11 (verified), Win32 and
  Cocoa (build-tested only); installers and packaging for Linux, macOS and Windows (Linux verified).
- **Sidekick agent:** Go-owned agent loop on the Anthropic Messages API (streaming, byte-stable cached prefix with rolling
  cache breakpoints, snapshot deltas, early tool dispatch, safety and confirmation gates). Bring your own key.
- **Data Saver:** exact socket byte counting, off/standard/strict levels, lazy images, size caps with early abort, media on
  play, background tab suspension, RFC 9111 HTTP cache with a 100 MB disk tier, day/week/month budgets with warnings and a
  hard stop.
- **Web fonts:** `@font-face` with WOFF, WOFF2, TTF/OTF and variable fonts, lazy `unicode-range`, CORS, disk cache,
  `document.fonts` / `FontFace`, data-saver gates, hardened decoders with fuzz tests.
- **Progressive page loads:** first frame after HTML plus render-blocking CSS, preload scanner, async fetch/XHR/images,
  module graph prefetch and bytecode cache.
- **Benchmarks:** `bench/vschrome` (same-method comparison with headed Chromium under Xvfb), `bench/datasaver`,
  `internal/app/cmd/appbench`.
- Earlier engine work: Go DOM, QuickJS host bindings, CSS layout and raster, Canvas 2D, software WebGL, media codecs,
  agent CLI and MCP server, batching, snapshot format v2.

### Performance
- Typing in the omnibox with the dropdown open: 4.6 ms p50 / 7.5 ms p99 at 1x (was 13.9 / 20.2 ms) via in-place dropdown
  patching, a cross-pass style cache, an SVG scene cache, exact damage rectangles and concurrent repaint bands.
- First frame on real pages 0.5 to 1.1 s (was 1.3 to 11.9 s).

### Fixed
- Omnibox inserted a typed space twice.
- Render panic on pages whose text starts with a forced break (go.dev).
- Quadratic DOM-collection behaviour (go.dev), module import resolution (MDN, GitHub), `template.innerHTML`, bare
  `addEventListener`, `about:blank`, detached `new Image().src`.

### Known limits
- Chromium is still ahead on JavaScript-heavy page loads (no JIT), on the first key that opens the omnibox dropdown, and
  on compatibility (no extensions; DRM is Clear Key plus a Widevine helper that needs a user-installed CDM, see docs/drm.md).
- Windows and macOS windows and installers have never been run; CI only builds and unit-tests them.
- Data Saver percentages were measured before web fonts landed and may move by a few points.

[Unreleased]: https://github.com/sweefbrowser/sweef/compare/v0.1.0-alpha...HEAD
[0.1.0-alpha]: https://github.com/sweefbrowser/sweef/releases/tag/v0.1.0-alpha
