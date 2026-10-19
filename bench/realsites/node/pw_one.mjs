// One fresh-process Playwright run for one task (Mode A baseline: Chromium via
// Playwright, snapshot = page._snapshotForAI(), the same accessibility-tree-with-refs
// format Playwright MCP gives agents). Prints exactly one JSON line.
import fs from 'node:fs';
import { performance } from 'node:perf_hooks';
import { chromium } from 'playwright-core';
import { CHROMIUM, re, estTokens, sleep, USABLE_POLL_MS, USABLE_MAX_MS, engineBanner } from './lib.mjs';

const CHALLENGE_RE = /just a moment|attention required|security verification|verify you are (a )?human|are you a robot|bots use|access denied|captcha/i;
const task = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const opt = JSON.parse(process.argv[3] || '{}');
const T = (opt.timeoutMs || 30000);
const bootMs = () => Date.now() - performance.timeOrigin; // since process start
const res = { tool: 'playwright', steps: [], ok: false };
let browser;
try {
  const t0 = performance.now();
  browser = await chromium.launch({ executablePath: CHROMIUM, headless: true, args: ['--no-sandbox'] });
  // ignoreHTTPSErrors: the sandbox egress proxy re-signs TLS with a CA that Chromium
  // does not trust (Sweef's Go TLS stack is configured with it). Same trust outcome.
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await ctx.newPage();
  page.setDefaultTimeout(T);
  res.launch_ms = performance.now() - t0;

  const snap = async () => {
    const t = performance.now();
    const s = await page._snapshotForAI();
    return { text: typeof s === 'string' ? s : (s.full || JSON.stringify(s)), ms: performance.now() - t };
  };
  // visit: navigate + snapshot until it contains the expected text (usable).
  const visit = async (phase) => {
    const o = {};
    const t = performance.now();
    try {
      const r = await page.goto(task.url, { timeout: T, waitUntil: 'load' });
      o.http_status = r ? r.status() : null;
    } catch (e) { o.load_error = String(e.message).split('\n')[0]; o.open_ms = performance.now() - t; return o; }
    o.open_ms = performance.now() - t;
    const want = re(task.snapshot_check);
    let first = true;
    const tu = performance.now();
    let s;
    for (;;) {
      s = await snap();
      if (first) { o.snapshot_ms = s.ms; first = false; }
      if (want.test(s.text) || performance.now() - tu > USABLE_MAX_MS) break;
      await sleep(USABLE_POLL_MS);
    }
    o.usable_ms = o.open_ms + (performance.now() - tu);
    o.snapshot_ok = want.test(s.text);
    o.engine_error = engineBanner(s.text);
    o.snapshot_bytes = Buffer.byteLength(s.text);
    o.snapshot_tokens_est = estTokens(s.text);
    o.snapshot_sample = s.text.slice(0, 160);
    return o;
  };

  const cold = await visit('cold');
  cold.ttfs_ms = res.launch_ms + cold.usable_ms; // launch + open + usable snapshot (excl. node boot)
  cold.ttfs_incl_boot_ms = bootMs();
  res.cold = cold;
  if (cold.load_error) { res.error = 'load_error: ' + cold.load_error; res.fail_class = 'load_error'; }
  else {
    // scripted steps
    let stepFail = null;
    for (const st of task.steps || []) {
      const t = performance.now();
      let ok = true, err = null;
      try {
        if (st.op === 'click') await page.click(st.selector, { timeout: T });
        else if (st.op === 'fill') await page.fill(st.selector, st.value, { timeout: T });
        else if (st.op === 'press') { if (st.selector) await page.press(st.selector, st.key, { timeout: T }); else await page.keyboard.press(st.key); }
        else if (st.op === 'select') await page.selectOption(st.selector, st.value, { timeout: T });
        else if (st.op === 'eval') await page.evaluate(st.js);
        else if (st.op === 'wait_text') await page.waitForFunction((x) => document.body.innerText.includes(x), st.text, { timeout: T });
        else throw new Error('unknown op ' + st.op);
      } catch (e) { ok = false; err = String(e.message).split('\n')[0]; }
      res.steps.push({ op: st.op, ms: performance.now() - t, ok, err });
      if (!ok) { stepFail = err; break; }
    }
    res.steps_ms_total = res.steps.reduce((a, s) => a + s.ms, 0);
    // final check: url + visible text must match, polled up to USABLE_MAX_MS
    const fc = re(task.final_check || task.snapshot_check);
    const tf = performance.now();
    let txt = '';
    for (;;) {
      try { txt = await page.evaluate(() => location.href + '\n' + document.body.innerText); } catch { txt = ''; }
      if (fc.test(txt) || performance.now() - tf > USABLE_MAX_MS) break;
      await sleep(USABLE_POLL_MS);
    }
    res.final_wait_ms = performance.now() - tf;
    res.final_ok = fc.test(txt);
    res.answer_in_page = task.answer_check ? re(task.answer_check).test(txt) : null; // spec self-check: the graded answer must exist in the page state
    // step latency of one follow-up snapshot on the current page
    const s2 = await snap();
    res.engine_error = cold.engine_error || engineBanner(txt) || engineBanner(s2.text);
    res.post_snapshot_ms = s2.ms;
    res.post_snapshot_bytes = Buffer.byteLength(s2.text);
    // warm: same session, repeat visit (browser up, HTTP cache populated)
    if (!opt.skipWarm) { const w = await visit('warm'); w.ttfs_ms = w.usable_ms; res.warm = w; }
    if (stepFail) { res.error = 'step_error: ' + stepFail; res.fail_class = 'step_error'; }
    else if (res.engine_error) { res.error = 'engine error rendered into the page: ' + res.engine_error; res.fail_class = 'engine_error'; }
    else if (!cold.snapshot_ok) {
      const title = await page.title().catch(() => '');
      if (CHALLENGE_RE.test(title + ' ' + (cold.snapshot_sample || ''))) { res.error = 'bot challenge page: ' + title.slice(0, 60); res.fail_class = 'bot_challenge'; }
      else { res.error = 'snapshot lacks expected text'; res.fail_class = 'snapshot_mismatch'; }
    }
    else if (!res.final_ok) { res.error = 'final check failed'; res.fail_class = 'final_mismatch'; }
    else res.ok = true;
  }
} catch (e) { res.error = 'harness: ' + String(e.message).split('\n')[0]; res.fail_class = 'harness'; }
finally { try { await browser?.close(); } catch {} }
console.log(JSON.stringify(res));
process.exit(0);
