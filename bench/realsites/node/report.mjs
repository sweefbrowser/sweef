#!/usr/bin/env node
// Turn a results/modea-*.json file into a markdown report (all tasks, failures included).
//   node report.mjs results/modea-X.json > results/modea-X.md
import fs from 'node:fs';
import { median } from './lib.mjs';

const file = process.argv[2];
if (!file) { console.error('usage: report.mjs results.json'); process.exit(2); }
const { meta, runs } = JSON.parse(fs.readFileSync(file, 'utf8'));
const tools = meta.config.tools;
const taskIds = [...new Set(runs.map((r) => r.task))];
const f0 = (x) => (x == null ? '-' : String(Math.round(x)));
const kb = (x) => (x == null ? '-' : (x / 1024).toFixed(1));

// per task+tool aggregate over OK runs only; failures counted separately
const agg = {};
for (const id of taskIds) for (const tool of tools) {
  const rs = runs.filter((r) => r.task === id && r.tool === tool);
  const ok = rs.filter((r) => r.ok);
  const fails = rs.filter((r) => !r.ok);
  const pick = (g) => median(ok.map(g));
  agg[id + '|' + tool] = {
    n: rs.length, ok: ok.length, fails,
    cold: pick((r) => (r.tool === 'playwright' || r.tool === 'lightpanda' ? r.cold?.ttfs_incl_boot_ms : r.cold?.ttfs_ms)),
    cold_open: pick((r) => r.cold?.open_ms), cold_snap: pick((r) => r.cold?.snapshot_ms),
    warm: pick((r) => r.warm?.ttfs_ms), bytes: pick((r) => r.cold?.snapshot_bytes), tokens: pick((r) => r.cold?.snapshot_tokens_est),
    step: ok.length ? median(ok.flatMap((r) => r.steps.map((s) => s.ms))) : null,
    post_snap: pick((r) => r.post_snapshot_ms), snap_i: pick((r) => r.cold?.snapshot_i_bytes),
  };
}
const A = (id, t) => agg[id + '|' + t];
const cell = (a, k) => (!a ? 'n/a' : a.ok === 0 ? 'FAIL' : f0(a[k]));

const L = [];
L.push(`# Real-site benchmark, Mode A (no LLM)\n`);
L.push(`Generated from \`${file.split('/').pop()}\` on ${meta.started}.\n`);
L.push(`- host: ${meta.host.cpus} CPUs (${meta.host.cpu_model}), ${meta.host.mem_gb} GB RAM, ${meta.host.kernel}`);
L.push(`- versions: sweef \`${meta.versions.sweef}\`, ${meta.versions.chromium}, playwright-core ${meta.versions.playwright_core}, node ${meta.versions.node}, agent-browser ${meta.versions.agent_browser || 'NOT AVAILABLE (not on PATH; not measured)'}`);
L.push(`- ${meta.config.runs} fresh-process runs per task and tool; medians over successful runs only; per-navigation timeout ${meta.config.timeout_s}s; tools: ${tools.join(', ')}\n`);

L.push(`## Pass rate (correctness first)\n`);
L.push(`A run passes only if the page loaded, the snapshot contained the expected text, all scripted steps succeeded and the final page state matched the check.\n`);
L.push(`| tool | passed runs | tasks with all runs passing | tasks with no passing run |`);
L.push(`|---|---|---|---|`);
for (const tool of tools) {
  const rs = runs.filter((r) => r.tool === tool);
  const ts = taskIds.map((id) => A(id, tool));
  L.push(`| ${tool} | ${rs.filter((r) => r.ok).length}/${rs.length} | ${ts.filter((a) => a.ok === a.n).length}/${ts.length} | ${ts.filter((a) => a.ok === 0).length} |`);
}

const hasPw = tools.includes('playwright'), hasSw = tools.includes('sweef');
L.push(`\n## Cold: time to a usable snapshot (ms, median)\n`);
L.push(`From invoking the tool in a fresh process (no daemon / browser running) until a snapshot containing the expected text is in hand. For Sweef this is \`open\` (starts the daemon) plus \`snapshot\`, two CLI spawns. For Playwright it is process start (node boot) + Chromium launch + goto(load) + \`_snapshotForAI()\`. Lower is better.\n`);
L.push(`| task | ${tools.join(' | ')}${hasPw && hasSw ? ' | sweef vs playwright' : ''} |`);
L.push(`|---|${tools.map(() => '---').join('|')}${hasPw && hasSw ? '|---' : ''}|`);
let wins = 0, losses = 0, both = 0;
for (const id of taskIds) {
  let ratio = '';
  if (hasPw && hasSw) {
    const s = A(id, 'sweef'), p = A(id, 'playwright');
    if (s.ok && p.ok) { both++; const x = p.cold / s.cold; ratio = x >= 1 ? `**${x.toFixed(2)}x faster**` : `${(1 / x).toFixed(2)}x slower`; if (x >= 1) wins++; else losses++; }
    else ratio = s.ok && !p.ok ? 'sweef only passes' : !s.ok && p.ok ? 'playwright only passes' : 'both fail';
  }
  L.push(`| ${id} | ${tools.map((t) => cell(A(id, t), 'cold')).join(' | ')}${ratio ? ' | ' + ratio : ''} |`);
}
if (hasPw && hasSw) {
  L.push(`\nOn the ${both} tasks where both passed, Sweef was faster cold on ${wins} and slower on ${losses}.`);
  const sc = taskIds.filter((id) => A(id, 'sweef').ok && A(id, 'playwright').ok).map((id) => [A(id, 'sweef').cold, A(id, 'playwright').cold]);
  if (sc.length) L.push(`Median cold: Sweef ${f0(median(sc.map((x) => x[0])))} ms, Playwright ${f0(median(sc.map((x) => x[1])))} ms.`);
}

L.push(`\n## Warm: repeat visit in the same session (ms, median)\n`);
L.push(`Same URL opened again in the live session (HTTP cache populated, browser / daemon already running): navigation plus usable snapshot.\n`);
L.push(`| task | ${tools.join(' | ')} |`);
L.push(`|---|${tools.map(() => '---').join('|')}|`);
for (const id of taskIds) L.push(`| ${id} | ${tools.map((t) => cell(A(id, t), 'warm')).join(' | ')} |`);

L.push(`\n## Snapshot size (cold, full snapshot)\n`);
L.push(`Bytes and estimated tokens (bytes/4; an estimate, not a tokenizer count). The formats differ: Sweef emits a YAML accessibility tree with \`/url:\` lines, Playwright \`_snapshotForAI\` emits its own aria tree. Smaller is cheaper to put in an agent context, provided the needed text survives (the check above).\n`);
L.push(`| task | ${tools.map((t) => `${t} KB`).join(' | ')} | ${tools.map((t) => `${t} tok`).join(' | ')}${hasSw ? ' | sweef -i KB' : ''} |`);
L.push(`|---|${tools.map(() => '---').join('|')}|${tools.map(() => '---').join('|')}${hasSw ? '|---' : ''}|`);
for (const id of taskIds) L.push(`| ${id} | ${tools.map((t) => { const a = A(id, t); return a.ok ? kb(a.bytes) : 'FAIL'; }).join(' | ')} | ${tools.map((t) => { const a = A(id, t); return a.ok ? f0(a.tokens) : 'FAIL'; }).join(' | ')}${hasSw ? ' | ' + kb(A(id, 'sweef').snap_i) : ''} |`);

L.push(`\n## Step latency (ms, median over scripted steps; CLI tools include process spawn)\n`);
L.push(`Sweef and agent-browser are driven as CLIs (one process spawn per command, as an agent shell tool would). Playwright steps are in-process calls with no spawn cost, which favours Playwright. Click/fill return semantics also differ (auto-wait vs settle), so treat this column as indicative only.\n`);
L.push(`| task | steps | ${tools.join(' | ')} |`);
L.push(`|---|---|${tools.map(() => '---').join('|')}|`);
const tm = JSON.parse(fs.readFileSync(new URL('../tasks.json', import.meta.url))).tasks;
for (const id of taskIds) { const n = tm.find((t) => t.id === id)?.steps.length || 0; if (n) L.push(`| ${id} | ${n} | ${tools.map((t) => cell(A(id, t), 'step')).join(' | ')} |`); }
L.push(`\nFollow-up \`snapshot\` latency on the current page (ms, median), a pure step: ` + tools.map((t) => `${t} ${f0(median(taskIds.map((id) => A(id, t).post_snap).filter((x) => x != null)))}`).join(', ') + '.');

L.push(`\n## Failures (not hidden)\n`);
L.push(`| task | tool | failed runs | classes | first error |`);
L.push(`|---|---|---|---|---|`);
let anyFail = false;
for (const id of taskIds) for (const tool of tools) {
  const a = A(id, tool);
  if (a.fails.length) { anyFail = true; const cls = [...new Set(a.fails.map((r) => r.fail_class))].join(', '); L.push(`| ${id} | ${tool} | ${a.fails.length}/${a.n} | ${cls} | ${(a.fails[0].error || '').replace(/\|/g, '/').slice(0, 140)} |`); }
}
if (!anyFail) L.push(`| - | - | none | - | - |`);

if (hasSw) {
  L.push(`\n## Slowest 5 Sweef cases (cold, passing runs)\n`);
  L.push(`| rank | task | sweef ms | playwright ms | open | snapshot | KB |`);
  L.push(`|---|---|---|---|---|---|---|`);
  const sl = taskIds.map((id) => [id, A(id, 'sweef')]).filter(([, a]) => a.ok).sort((x, y) => y[1].cold - x[1].cold).slice(0, 5);
  sl.forEach(([id, a], i) => L.push(`| ${i + 1} | ${id} | ${f0(a.cold)} | ${hasPw ? cell(A(id, 'playwright'), 'cold') : '-'} | ${f0(a.cold_open)} | ${f0(a.cold_snap)} | ${kb(a.bytes)} |`));
}
L.push(`\n## Method notes\n`);
for (const n of meta.notes) L.push(`- ${n}`);
console.log(L.join('\n'));
