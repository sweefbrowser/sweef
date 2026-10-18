# Real-site benchmark: Sweef vs Chromium for AI agents

A harness for an honest, reproducible comparison on **real public websites**, not a local fixture. It has two modes:

| | Mode A | Mode B |
|---|---|---|
| LLM | none, deterministic scripted commands | real model, real agent loops |
| Needs | network, preinstalled Chromium | network, Chromium, `ANTHROPIC_API_KEY` |
| Measures | time to a usable snapshot (cold and warm), snapshot size, step latency, pass/fail | wall time, agent turns, input/output tokens, cost, task success |
| Compares | Sweef CLI daemon vs Playwright + Chromium (vs agent-browser when installed) | `sweef agent` vs a generic Playwright + Chromium tool loop |
| Status | **run, results in `results/`** | **written and plumbing-tested against a mock only; never run against the live API** |

Everything lives in this directory; nothing is added to the `sweef` binary's dependencies. (The one change outside it is a small `-json` flag on `sweef agent`, `internal/agent/cli.go`, so a benchmark can read the final answer and run stats as one JSON object. It is covered by `TestRunCLIJSON`.)

```
tasks.json            39 tasks on real sites; the single source of truth for both modes
prices.json           USD per million tokens used to cost Mode B runs (edit and verify)
node/                 Playwright baseline + Mode A/B drivers + report generators (own lockfile)
  modea.mjs           Mode A driver
  pw_one.mjs          one fresh-process Playwright run (called by modea.mjs)
  report.mjs          results JSON -> markdown table
  modeb.mjs           Mode B driver (refuses to run without the key)
  pw_agent.mjs        the Playwright/Chromium agent loop used as the Mode B baseline
  report_b.mjs        Mode B results JSON -> markdown
  mock_anthropic.mjs  TEST-ONLY mock of the Messages API
test_plumbing.sh      TEST-ONLY Mode B plumbing check against the mock
results/              raw JSON and generated markdown
```

## Tasks

`tasks.json` has 39 tasks across Wikipedia, GitHub (repo, issue, file view), Hacker News, MDN, npm, PyPI, Stack Overflow, old Reddit, React and Next.js docs, SPA docs (Angular, Vue, Redux), Python/Go/Rust docs, arXiv, an SPA login (saucedemo), form logins, JS-rendered pages (quotes.toscrape.com/js, TodoMVC React), e-commerce demos (books.toscrape.com, webscraper.io), a search results page, and news front pages. Each task has:

- `url`: where the run starts.
- `goal`: the natural-language instruction given to an agent in Mode B.
- `snapshot_check`: regex the first snapshot of the start page must match (Mode A "page loaded and is usable").
- `steps` and `final_check`: a scripted interaction (click, fill, press, select, wait_text) and a regex over `location.href + page text` after it (Mode A "the interaction worked").
- `answer_check`: regex the agent's final answer must match (Mode B correctness). `null` means the content is dynamic and there is no stable answer; those tasks are Mode A only (`mode_b: false`).
- `risk`: known hazards (rate limits, bot blocks, TLS, availability). They are **kept in the benchmark** so the table does not hide them.

Checks were calibrated by running the specs once against Playwright + Chromium and confirming that every `answer_check` appears in the final page state (`answer_in_page` in the Mode A JSON). They were not tuned to make Sweef pass or fail.

Live sites change. A failing check on a later date may mean the site changed; the raw JSON keeps a snapshot excerpt per run to tell the two apart.

## Mode A: methodology

```
cd bench/realsites/node && npm ci          # playwright-core only; uses the preinstalled Chromium, never downloads one
CGO_ENABLED=0 go build -ldflags='-s -w' -o /tmp/sweef ../../../cmd/sweef
node modea.mjs --sweef /tmp/sweef --runs 3 --timeout 20 --pace-ms 1500 --out ../results/modea-$(date +%F).json
node report.mjs ../results/modea-$(date +%F).json > ../results/modea-$(date +%F).md
```

Flags: `--tools sweef,playwright,agent-browser,lightpanda`, `--only id1,id2`, `--runs N`, `--timeout S`, `--pace-ms`, `--skip-warm`. `agent-browser` is measured only if it is on `PATH` (or `--agent-browser path`); otherwise it is reported as not measured. Its adapter is written from the CLI shape in `bench/run.sh` and has **not** been exercised here.

`lightpanda` ([lightpanda-io/browser](https://github.com/lightpanda-io/browser), AGPL-3.0, Zig + V8, no rendering) is measured only when asked for: `--tools lightpanda --lightpanda /path/to/lightpanda` (or `LIGHTPANDA`, or on `PATH`). Each run starts a fresh `lightpanda serve` (telemetry off with `LIGHTPANDA_DISABLE_TELEMETRY=true`, stylesheets on as in its MCP/agent defaults, the egress proxy and CA bundle passed as `--http-proxy` / `--ca-cert`) and drives it through `chromium.connectOverCDP` with the same `pw_one.mjs` steps, snapshot (`page._snapshotForAI()`) and checks as the Playwright adapter. Nothing of it is vendored or linked; the binary is a download and is never committed.

Per task, per tool, per run:

1. **Fresh process.** Sweef: new `SWEEF_HOME`, so a new daemon, killed afterwards. Playwright: a new `node` process and a new Chromium (new context, empty cache). Chromium comes from `PLAYWRIGHT_BROWSERS_PATH=/opt/pw-browsers` (`/opt/pw-browsers/chromium`).
2. **Cold, time to a usable snapshot.** From invoking the tool until a snapshot containing `snapshot_check` is in hand. Sweef: `sweef open <url>` (starts the daemon) then `sweef snapshot`, two CLI spawns, timed from the parent. Playwright: process start to `page._snapshotForAI()` (node boot, Chromium launch, `goto` to `load`, snapshot). If the first snapshot lacks the text (late-rendering SPA), both tools poll every 250 ms for up to 10 s; the wait counts. A page that never produces the text is a failure, not a slow success.
3. **Snapshot size.** Bytes and an estimated token count (bytes/4, same rule of thumb the Sweef agent uses; there is no tokenizer in the harness, so treat it as an estimate). Sweef's `-i` (interactive only) size is recorded as an extra. Sweef's snapshot is a YAML accessibility tree; Playwright's `_snapshotForAI` is Playwright's own aria tree with refs. They are different formats; the comparison is "what each tool hands an agent", not like-for-like encodings.
4. **Step latency.** Each scripted step timed. Sweef/agent-browser steps are CLI invocations and include process spawn; Playwright steps are in-process calls. That favours Playwright; click and fill also return at different points (auto-wait vs settle). Indicative only.
5. **Final check**, polled up to 10 s, then **warm**: the same URL opened again in the same live session (browser/daemon up, HTTP cache populated) and a usable snapshot again.

Fairness rules:

- Same machine, same network (the sandbox egress proxy), same per-navigation timeout, same polling policy, same checks for every tool.
- Tool order is rotated per run; requests are paced (`--pace-ms`) so one tool is not the one that trips a site's rate limit. Pacing cannot remove it (Wikimedia in particular answers 429 under repeated load; a 429 page fails the check for whichever tool hit it, and is reported).
- **Cold and warm are both reported.** Medians are over successful runs only; failures are counted and listed in their own table with the failure class (`load_error`, `snapshot_mismatch`, `step_error`, `final_mismatch`, `harness`). Every task appears in every table. No task is dropped for being slow, blocked or failing.
- Playwright uses `ignoreHTTPSErrors`: the sandbox proxy re-signs TLS with a CA that Chromium does not trust but Sweef's Go TLS stack is given. This gives both tools the same trust outcome.
- The pass rate is the headline; speed on a page a tool failed to render is not a win.

## Mode B: methodology

```
export ANTHROPIC_API_KEY=...        # the only way the key is supplied; never a flag, never printed, never written to results
cd bench/realsites/node
node modeb.mjs --sweef /tmp/sweef --runs 3               # default model claude-haiku-5-5
node modeb.mjs --sweef /tmp/sweef --model claude-sonnet-5-5 --only wiki-turing,github-issue-9
node report_b.mjs ../results/modeb-<stamp>.json > ../results/modeb-<stamp>.md
```

Without the key, `modeb.mjs` exits with status 2, prints that `ANTHROPIC_API_KEY` is required, and makes no requests.

Both agents get the same prompt (`goal` plus "the browser is already open on <url>"), the same model, `--max-turns` (default 40), `--max-tokens` (8192 per response), `--effort` (low), `--thinking` (adaptive; `disabled` is valid at effort <= high on Haiku), a snapshot cap of 6000 estimated tokens, and a cost cap (`--max-cost`, default $0.50). Default model is `claude-haiku-5-5`; change it with `--model`.

- **Sweef side:** `sweef agent -json -yes -model M -url U -max-turns N -max-cost C -effort E "<prompt>"` in a fresh `SWEEF_HOME`. Tools `act` (batched steps), `tabs`, `read_page`, `find`, `ask_user`, `done`; Sweef's own long cached system prompt, snapshot deltas, streaming with early tool dispatch.
- **Chromium side:** `pw_agent.mjs`: a plain non-streaming Messages API loop with tools `open`, `snapshot`, `click(ref)`, `fill(ref,text,submit?)`, `press`, `eval`, `done`, over Playwright + the preinstalled Chromium. Snapshots come from `_snapshotForAI()` with refs; after each state-changing action the tool returns a fresh (capped) snapshot, as Playwright-MCP style agents do (`--pw-observe none` returns only the URL). Short system prompt, prompt-cache breakpoints on tools, system and the newest message.
- Recorded per run: wall time (including process/browser start), turns, requests, tool calls, input/cache/output tokens, cost, stop reason, the answer, and success. **Success** means the run ended with `done` (or a final text) and the answer matches `answer_check`. **Cost** is computed from the reported token usage with `prices.json` for both agents (the Sweef binary's own estimate is stored beside it as `cost_usd_sweef_estimate`).
- Prices in `prices.json` come from the model table the author had (haiku-5-5 $0.10/$0.50 per MTok up to 100K prompt tokens and $0.50/$2.50 beyond; sonnet-5-5 $2/$10); cache reads at 0.1x and writes at 1.25x. Verify against your invoice before quoting cost.

What this does and does not show: it compares **complete stacks** (browser engine + agent design: prompts, tool schemas, batching, deltas). A win for `sweef agent` mixes the engine and the agent loop. It does not isolate the browser engine; the Mode A numbers do that, without an LLM. Isolating the engine under the same agent would need the Sweef agent running on a Chromium `Backend`, which this harness does not do.

### Plumbing test (not results)

`SWEEF=/tmp/sweef bash bench/realsites/test_plumbing.sh` checks, against `mock_anthropic.mjs` only, that Mode B refuses without a key, that both agents complete a scripted two-turn run, that usage and cost are recorded, and that no key reaches the results. `--plumbing-test` only accepts a localhost `--base-url`, labels the output file `PLUMBING-TEST-NOT-RESULTS-*` and sets `"mock": true` with a warning in the JSON and in the generated markdown. Numbers from the mock mean nothing.

### Unverified until someone runs it with a key

- Mode B has never touched the live API. Request shapes were written from the API reference; the mock only checks our own plumbing. Expect to fix small things on first contact (for example parameter validation for a given model).
- The Sweef agent's thinking/effort handling and `-no-thinking` on `claude-sonnet-5-5` (that model rejects `thinking: disabled`; use the default adaptive thinking there).
- Whether the cost caps stop the two agents at comparable points (the Sweef cap uses its internal Haiku price table, the baseline uses `prices.json`; they coincide only for Haiku).

## Reading the results

`results/modea-*.md` is generated by `report.mjs` from the matching JSON: pass rates, cold and warm tables for every task, snapshot sizes, step latency, a failures table, and the five slowest Sweef cases. Read the pass rates and the failures table first.

## Results: Mode A, 2026-10-14 (this sandbox, 3 fresh runs per task, 20 s timeout)

Full tables: `results/modea-2026-10-14.md` (generated) and `results/modea-2026-10-14.json` (raw, one record per run). Host: 4 vCPU Xeon @ 2.1 GHz, Chromium 141.0.7390.37 via playwright-core 1.56.1, `sweef 0.1.0` (static, stripped). agent-browser was **not on PATH, so it was not measured**. Playwright is the Chrome baseline here, not agent-browser; numbers are not comparable to the README's agent-browser table.

**Sweef does not win across the board on real sites.**

- Pass rate: Sweef 94/117 runs, Playwright 101/117. Tasks with all runs passing: Sweef 31/39, Playwright 33/39.
- On the 30 tasks both passed, cold time to a usable snapshot: Sweef faster on 15, slower on 15. Median Sweef 2358 ms vs Playwright 1798 ms. Warm (repeat visit in the same session) Playwright is faster on most script-heavy pages (for example mdn-array-map 4365 vs 319 ms, nextjs-docs 12674 vs 288 ms).
- Where Sweef wins cold: small or server-rendered pages. Hacker News 2.4-3.4x, example.com 2.8x, httpbin form 2.8x, quotes.toscrape JS version 3.9x, NPR/CNN text 2.3-3.3x, arxiv 2.2x, books.toscrape 1.8x, GitHub repo and issue pages 1.4-1.9x, practice login 1.7x. One 16x outlier (rust-lang-home, 112 ms) is unexplained and worth re-measuring before quoting.
- Where Sweef loses cold: heavy client-side pages and Wikipedia/MDN/docs sites, 1.3x to 9.2x slower (wiki-turing 1.6x, mdn-array-map 2.8x, pypi 2.6x, angular 2.1x, redux 2.0x, nextjs-docs 9.2x).
- Snapshot size: Sweef is smaller than Playwright's `_snapshotForAI` on nearly every task where both worked (for example wiki-turing 58 KB vs 620 KB). **Caveat:** Sweef's snapshots are capped near 58-59 KB (about 15K estimated tokens), so on big pages part of the difference is truncation, not a more compact encoding. The expected text still survived on every passing run.
- Step latency: not an apples-to-apples column (CLI spawn per Sweef step vs in-process Playwright). Sweef steps were fast on form pages (about 12-150 ms) and slow where a click navigates (webscraper-laptops 2985 ms vs 294 ms; wiki-search-flow 1639 vs 114 ms).

### Failures (all reported, nothing dropped)

| task | Sweef | Playwright | cause (observed or guessed) |
|---|---|---|---|
| github-license-file | fails 3/3 | passes | Observed console error `Automatic publicPath is not supported in this browser` from GitHub's webpack runtime, so the blob view never renders. Likely a missing API (for example `document.currentScript`); not confirmed. |
| so-sorted-array | fails 3/3 | passes | Sweef gets the Cloudflare "Just a moment..." challenge (observed) and does not pass it; Chromium does. |
| saucedemo-login | fails 3/3 | passes | After `fill` + `click`, the form was submitted natively (URL gained `?user-name=...&password=...`), so React's handler did not run. Possibly `fill` not updating React-controlled state; unconfirmed. |
| todomvc-react | fails 3/3 | passes | Same symptom: after `fill` + `press Enter` the todo is not added. Same suspected cause. |
| npm-left-pad | passes | fails 3/3 | Cloudflare challenge blocks Chromium; Sweef loaded the real page. (Fragile; may change with their bot rules.) |
| search-ddg | passes 1/3 | fails 3/3 | DuckDuckGo shows its bot challenge to both most of the time; one Sweef run got through. Weak signal, do not read as a win. |
| reddit-old-programming | fails 3/3 | fails 3/3 | Network-security block for both. |
| internet-login, internet-dynamic-loading | fails 3/3 | fails 3/3 | the-internet.herokuapp.com timed out for both tools from this sandbox (host unreachable); not a browser result. |
| wiki-go-language | passes | fails 1/3 | Most likely a Wikimedia 429 (seen in probes before the run; not recorded per run). Wikipedia rate-limits everyone; treat as noise. |

### Slowest 5 Sweef cases (cold, passing runs) with a guess at the cause

| rank | task | Sweef ms (Playwright ms) | guess |
|---|---|---|---|
| 1 | nextjs-docs | 13665 (1484) | `open` itself takes 13.5 s while no single request in `sweef network` is slow (largest 360 ms): most likely script execution or the engine's load-settle wait on a streaming Next.js app, not the network. Unconfirmed. |
| 2 | npm-left-pad | 6391 (fail) | Heavy client bundle; same script-execution suspicion. |
| 3 | pypi-requests | 5198 (2008) | `open` is 5.2 s for a mostly static page, so likely the load-settle wait or third-party scripts rather than layout. Unconfirmed. |
| 4 | angular-overview | 4943 (2386) | Angular SPA bootstrap executed by the pure-Go JS engine (script execution). |
| 5 | mdn-array-map | 4702 (1671) | Script execution plus subresource fetches; snapshot itself takes 18 ms. |

In every one of the five, the time is in `open`, not in the snapshot step. These causes are guesses from timings and a few probes; nothing was profiled.

### What is not established

- One machine, one network path (a sandbox egress proxy), one day, 3 runs per cell. Timing noise on real sites is large; the cold column for sites that were also rate-limited is the least reliable.
- No agent-browser numbers. No Mode B numbers (no API key in this sandbox).
- Page-load semantics differ (Playwright waits for `load`; Sweef `open` has its own settle logic), and the usable-snapshot polling absorbs part of that. A fully equal definition of "page ready" does not exist across engines.
