#!/usr/bin/env node
// Mode A: deterministic, no-LLM benchmark. For each task, each tool, N runs, each run in a
// fresh process (fresh Sweef daemon + fresh HOME / fresh Playwright process + fresh Chromium).
//
//   node modea.mjs --sweef /path/to/sweef [--runs 3] [--tools sweef,playwright,agent-browser,lightpanda]
//                  [--only id1,id2] [--timeout 30] [--pace-ms 1500] [--out results/modea-X.json]
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn, spawnSync } from 'node:child_process';
import { performance } from 'node:perf_hooks';
import { HERE, CHROMIUM, loadTasks, re, estTokens, sleep, USABLE_POLL_MS, USABLE_MAX_MS, engineBanner, rmTree } from './lib.mjs';

const args = process.argv.slice(2);
const CHALLENGE_RE = /just a moment|attention required|security verification|verify you are (a )?human|are you a robot|bots use|access denied|captcha/i;
const opt = { runs: 3, tools: 'sweef,playwright,agent-browser', timeout: 30, paceMs: 1500, only: '', sweef: process.env.SWEEF || 'sweef', out: '', skipWarm: false };
for (let i = 0; i < args.length; i++) {
  const a = args[i];
  const nx = () => args[++i];
  if (a === '--runs') opt.runs = +nx();
  else if (a === '--tools') opt.tools = nx();
  else if (a === '--only') opt.only = nx();
  else if (a === '--timeout') opt.timeout = +nx();
  else if (a === '--pace-ms') opt.paceMs = +nx();
  else if (a === '--sweef') opt.sweef = nx();
  else if (a === '--agent-browser') opt.ab = nx();
  else if (a === '--lightpanda') opt.lp = nx();
  else if (a === '--out') opt.out = nx();
  else if (a === '--skip-warm') opt.skipWarm = true;
  else { console.error('unknown arg ' + a); process.exit(2); }
}
opt.ab = opt.ab || process.env.AGENT_BROWSER || which('agent-browser');
opt.lp = opt.lp || process.env.LIGHTPANDA || which('lightpanda');
function which(n) { const r = spawnSync('sh', ['-c', `command -v ${n}`], { encoding: 'utf8' }); return r.stdout.trim() || null; }

const tasks = loadTasks(undefined, opt.only ? opt.only.split(',') : null);
const tools = opt.tools.split(',').filter(Boolean);
const TMS = opt.timeout * 1000;

// ---- generic CLI adapter (sweef, agent-browser) ----------------------------------
function exec(cmd, argv, env, timeoutMs) {
  return new Promise((resolve) => {
    const t = performance.now();
    const p = spawn(cmd, argv, { env, stdio: ['ignore', 'pipe', 'pipe'] });
    let out = '', err = '';
    p.stdout.on('data', (d) => (out += d));
    p.stderr.on('data', (d) => (err += d));
    const to = setTimeout(() => p.kill('SIGKILL'), timeoutMs);
    p.on('close', (code) => { clearTimeout(to); resolve({ code, out, err, ms: performance.now() - t }); });
  });
}

const tcl = (s) => s; // placeholder for symmetry
function cliAdapter(kind) {
  const bin = kind === 'sweef' ? opt.sweef : opt.ab;
  return async function runCli(task) {
    const home = fs.mkdtempSync(path.join(os.tmpdir(), kind + '-home-'));
    // A cold run starts with empty caches, as the Playwright side does (a fresh Chromium profile): Sweef's
    // bytecode and HTTP caches default to ~/.cache/sweef, which every earlier run (and day) had filled, so
    // until 2026-10-15 Sweef's "cold" runs replayed cached bytecode and responses. The warm visit below runs
    // in the same session and so still sees what the cold visit cached.
    const env = { ...process.env, SWEEF_HOME: home, SWEEF_CACHE_DIR: path.join(home, 'cache'), AGENT_BROWSER_EXECUTABLE_PATH: CHROMIUM, AGENT_BROWSER_SESSION: 'bench' + process.pid };
    const tf = `${TMS / 1000}s`;
    const run = (a) => exec(bin, a, env, TMS + 15000);
    const res = { tool: kind, steps: [], ok: false };
    const sweef = kind === 'sweef';
    const open = (url) => run(sweef ? ['open', url, '--timeout', tf] : ['open', url]);
    const snapshot = () => run(['snapshot']);
    const evalJs = (js) => run(sweef ? ['eval', js] : ['eval', js]);
    const visit = async (t0) => {
      const o = {};
      const tOpen = performance.now();
      const r = await open(task.url);
      o.open_ms = performance.now() - tOpen;
      if (r.code !== 0) { o.load_error = (r.out + r.err).trim().split('\n')[0].slice(0, 200); return o; }
      const want = re(task.snapshot_check);
      let first = true;
      const tu = performance.now();
      let s;
      for (;;) {
        s = await snapshot();
        if (first) { o.snapshot_ms = s.ms; first = false; }
        if ((s.code === 0 && want.test(s.out)) || performance.now() - tu > USABLE_MAX_MS) break;
        await sleep(USABLE_POLL_MS);
      }
      o.usable_ms = o.open_ms + (performance.now() - tu);
      o.snapshot_ok = s.code === 0 && want.test(s.out);
      o.engine_error = engineBanner(s.out);
      o.snapshot_bytes = Buffer.byteLength(s.out);
      o.snapshot_tokens_est = estTokens(s.out);
      o.snapshot_sample = s.out.slice(0, 160);
      if (sweef && !o.snapshot_i_bytes) {
        const si = await run(['snapshot', '-i']);
        if (si.code === 0) o.snapshot_i_bytes = Buffer.byteLength(si.out);
      }
      return o;
    };
    try {
      const tStart = performance.now();
      const cold = await visit();
      cold.ttfs_ms = performance.now() - tStart - (cold.snapshot_i_bytes != null ? 0 : 0);
      res.cold = cold;
      if (cold.load_error) { res.error = 'load_error: ' + cold.load_error; res.fail_class = 'load_error'; }
      else {
        let stepFail = null;
        for (const st of task.steps || []) {
          let a;
          const sel = st.selector;
          if (st.op === 'click') a = sweef || !/^text=/.test(sel) ? ['click', sel] : ['find', 'text', sel.slice(5), 'click'];
          else if (st.op === 'fill') a = ['fill', sel, st.value];
          else if (st.op === 'press') a = sweef ? ['press', st.key, ...(sel ? [sel] : [])] : ['press', st.key];
          else if (st.op === 'select') a = ['select', sel, st.value];
          else if (st.op === 'eval') a = ['eval', st.js];
          else if (st.op === 'wait_text') a = sweef ? ['wait', '--text', st.text] : ['wait', '--text', st.text];
          else a = null;
          if (sweef && a) a.push('--timeout', tf);
          const r = a ? await run(a) : { code: 1, out: '', err: 'unknown op', ms: 0 };
          const ok = r.code === 0;
          res.steps.push({ op: st.op, ms: r.ms, ok, err: ok ? null : (r.out + r.err).trim().split('\n')[0].slice(0, 200) });
          if (!ok) { stepFail = res.steps.at(-1).err; break; }
        }
        res.steps_ms_total = res.steps.reduce((a, s) => a + s.ms, 0);
        const fc = re((sweef && task.final_check_sweef) || task.final_check || task.snapshot_check);
        const tf0 = performance.now();
        let txt = '';
        for (;;) {
          const r = await evalJs('location.href + "\\n" + document.body.innerText');
          txt = r.code === 0 ? r.out : '';
          // sweef eval prints the value as JSON: decode it, or line breaks reach the regex as the two characters \n
          try { const v = JSON.parse(txt.trim()); if (typeof v === 'string') txt = v; } catch { /* not JSON: keep the raw text */ }
          if (fc.test(txt) || performance.now() - tf0 > USABLE_MAX_MS) break;
          await sleep(USABLE_POLL_MS);
        }
        res.final_wait_ms = performance.now() - tf0;
        res.final_ok = fc.test(txt);
        res.answer_in_page = task.answer_check ? re(task.answer_check).test(txt) : null;
        const s2 = await snapshot();
        res.engine_error = cold.engine_error || engineBanner(txt) || engineBanner(s2.out);
        res.post_snapshot_ms = s2.ms;
        res.post_snapshot_bytes = Buffer.byteLength(s2.out);
        if (!opt.skipWarm) { const w = await visit(); w.ttfs_ms = w.usable_ms; res.warm = w; }
        if (stepFail) { res.error = 'step_error: ' + stepFail; res.fail_class = 'step_error'; }
        else if (res.engine_error) { res.error = 'engine error rendered into the page: ' + res.engine_error; res.fail_class = 'engine_error'; }
        else if (!cold.snapshot_ok) {
          // tell a bot-challenge page (nothing the browser can legitimately fix) from a real content mismatch
          const tt = await evalJs('document.title');
          const title = tt.code === 0 ? tt.out : '';
          if (CHALLENGE_RE.test(title + ' ' + (cold.snapshot_sample || ''))) { res.error = 'bot challenge page: ' + title.trim().slice(0, 60); res.fail_class = 'bot_challenge'; }
          else { res.error = 'snapshot lacks expected text'; res.fail_class = 'snapshot_mismatch'; }
        }
        else if (!res.final_ok) { res.error = 'final check failed'; res.fail_class = 'final_mismatch'; }
        else res.ok = true;
      }
    } catch (e) { res.error = 'harness: ' + e.message; res.fail_class = 'harness'; }
    await exec(bin, ['close'], env, 15000);
    // make sure no daemon outlives the run (fresh daemon per run is part of the method)
    if (sweef) spawnSync('pkill', ['-f', `${home}/default.sock`]);
    rmTree(home);
    return res;
  };
}

// ---- Playwright adapter: fresh node process per run -------------------------------
function cdpAdapter(tool) { return (task) => runPlaywright(task, tool); }
async function runPlaywright(task, tool = 'playwright') {
  const f = path.join(os.tmpdir(), `pwtask-${process.pid}-${task.id}.json`);
  fs.writeFileSync(f, JSON.stringify(task));
  const r = await exec('node', [path.join(HERE, 'pw_one.mjs'), f, JSON.stringify({ tool, lightpanda: opt.lp, timeoutMs: TMS, skipWarm: opt.skipWarm })],
    { ...process.env, PLAYWRIGHT_BROWSERS_PATH: '/opt/pw-browsers' }, TMS * 4 + 30000);
  fs.rmSync(f, { force: true });
  const line = r.out.trim().split('\n').pop();
  try { const j = JSON.parse(line); j.process_wall_ms = r.ms; return j; }
  catch { return { tool, ok: false, steps: [], error: 'harness: no result (' + r.err.slice(0, 160) + ')', fail_class: 'harness', process_wall_ms: r.ms }; }
}

const adapters = { sweef: cliAdapter('sweef'), playwright: cdpAdapter('playwright'), 'agent-browser': cliAdapter('agent-browser'), lightpanda: cdpAdapter('lightpanda') };
const available = {};
for (const t of tools) {
  if (t === 'agent-browser' && !opt.ab) { console.error('agent-browser: not on PATH, skipped (pass --agent-browser or set AGENT_BROWSER)'); continue; }
  if (t === 'lightpanda' && !opt.lp) { console.error('lightpanda: not on PATH, skipped (pass --lightpanda or set LIGHTPANDA)'); continue; }
  available[t] = true;
}
const active = tools.filter((t) => available[t]);

const ver = (cmd, a) => { const r = spawnSync(cmd, a, { encoding: 'utf8' }); return (r.stdout || r.stderr || '').trim().split('\n')[0]; };
const meta = {
  started: new Date().toISOString(),
  host: { platform: process.platform, arch: process.arch, cpus: os.cpus().length, cpu_model: os.cpus()[0]?.model, mem_gb: +(os.totalmem() / 2 ** 30).toFixed(1), kernel: os.release() },
  versions: { sweef: ver(opt.sweef, ['version']), chromium: ver(CHROMIUM, ['--version']), playwright_core: JSON.parse(fs.readFileSync(path.join(HERE, 'node_modules/playwright-core/package.json'))).version, node: process.version, agent_browser: opt.ab ? ver(opt.ab, ['--version']) : null, lightpanda: opt.lp && active.includes('lightpanda') ? ver(opt.lp, ['version']) : null },
  config: { runs: opt.runs, timeout_s: opt.timeout, pace_ms: opt.paceMs, tools: active, tasks: tasks.length, usable_poll_ms: USABLE_POLL_MS, usable_max_ms: USABLE_MAX_MS, network: 'sandbox egress proxy; all tools share it' },
  notes: [
    'Each run: fresh process (Sweef: new SWEEF_HOME and daemon killed after; Playwright: new node process and new Chromium).',
    'cold = first visit in that fresh process (includes daemon / browser launch); warm = repeat visit of the same URL in the same session (HTTP cache populated).',
    'Tool order is rotated per run and requests are paced to limit rate-limit bias.',
  ],
};
const runs = [];
const out = opt.out || path.join(HERE, '..', 'results', `modea-${meta.started.replace(/[:.]/g, '-')}.json`);
fs.mkdirSync(path.dirname(out), { recursive: true });
const flush = () => fs.writeFileSync(out, JSON.stringify({ meta, runs }, null, 1));

console.error(`Mode A: ${tasks.length} tasks x ${active.join('+')} x ${opt.runs} runs -> ${out}`);
for (const task of tasks) {
  for (let r = 0; r < opt.runs; r++) {
    const order = active.map((_, i) => active[(i + r + tasks.indexOf(task)) % active.length]);
    for (const tool of order) {
      const res = await adapters[tool](task);
      res.task = task.id; res.run = r + 1; res.tool = tool;
      runs.push(res);
      const ttfs = res.cold?.ttfs_ms ? Math.round(res.cold.ttfs_ms) : '-';
      console.error(`${task.id.padEnd(26)} ${tool.padEnd(13)} run${r + 1} ${res.ok ? 'ok  ' : 'FAIL'} cold ${String(ttfs).padStart(6)}ms ${res.ok ? '' : res.error || ''}`);
      await sleep(opt.paceMs);
    }
  }
  flush();
}
flush();
console.error('done: ' + out);
