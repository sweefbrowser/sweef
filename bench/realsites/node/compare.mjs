#!/usr/bin/env node
// Before/after table for two Mode A result files (all tasks, failures included).
//   node compare.mjs results/BEFORE.json results/AFTER.json [--before-tool sweef] [--after-tool sweef] > results/X.md
// BEFORE is usually the old Sweef binary run under the same harness; AFTER holds the new binary and, when it was
// measured, Playwright. Times are cold time to a usable snapshot (fresh process, fresh cache), medians over passing runs.
import fs from 'node:fs';
import { median } from './lib.mjs';

const args = process.argv.slice(2);
const files = [];
let bt = 'sweef', at = 'sweef';
for (let i = 0; i < args.length; i++) {
  if (args[i] === '--before-tool') bt = args[++i];
  else if (args[i] === '--after-tool') at = args[++i];
  else files.push(args[i]);
}
if (files.length !== 2) { console.error('usage: compare.mjs BEFORE.json AFTER.json'); process.exit(2); }
const B = JSON.parse(fs.readFileSync(files[0], 'utf8'));
const A = JSON.parse(fs.readFileSync(files[1], 'utf8'));
let cats = {};
try {
  const t = JSON.parse(fs.readFileSync(new URL('../tasks.json', import.meta.url), 'utf8'));
  for (const x of t.tasks || t) cats[x.id] = x.category;
} catch {}

const cold = (r) => (r.tool === 'playwright' || r.tool === 'lightpanda' ? r.cold?.ttfs_incl_boot_ms : r.cold?.ttfs_ms);
function agg(runs, tool, id) {
  const rs = runs.filter((r) => r.task === id && r.tool === tool);
  const ok = rs.filter((r) => r.ok);
  return { n: rs.length, ok: ok.length, cold: median(ok.map(cold)), tokens: median(ok.map((r) => r.cold?.snapshot_tokens_est)) };
}
const ids = [...new Set([...A.runs.map((r) => r.task), ...B.runs.map((r) => r.task)])];
const hasPW = A.runs.some((r) => r.tool === 'playwright');
const f0 = (x) => (x == null ? 'FAIL' : String(Math.round(x)));
const ratio = (x, y) => (x == null || y == null ? '-' : x <= y ? `**${(y / x).toFixed(2)}x faster**` : `${(x / y).toFixed(2)}x slower`);

const rows = [];
for (const id of ids) {
  const b = agg(B.runs, bt, id), a = agg(A.runs, at, id), p = hasPW ? agg(A.runs, 'playwright', id) : null;
  rows.push({ id, cat: cats[id] || '-', b, a, p });
}
const sum = (xs) => xs.reduce((s, x) => s + x, 0);
const out = [];
out.push(`# Mode A before/after: ${files[0].split('/').pop()} (${bt}) vs ${files[1].split('/').pop()} (${at})`, '');
out.push(`- before: ${B.meta?.versions?.sweef || ''} ${B.meta?.started || ''}; after: ${A.meta?.versions?.sweef || ''} ${A.meta?.started || ''}`);
out.push(`- host: ${A.meta?.host?.cpus || '?'} CPUs ${A.meta?.host?.cpu_model || ''}, ${A.meta?.versions?.chromium || ''}`);
out.push('- cold = fresh process and fresh HTTP/bytecode cache per run, until a snapshot with the expected text is in hand; medians over passing runs', '');
out.push('## Summary', '');
const both = rows.filter((r) => r.b.cold != null && r.a.cold != null);
const vsPW = rows.filter((r) => r.p && r.p.cold != null && r.a.cold != null);
out.push('| | before | after' + (hasPW ? ' | playwright |' : ' |'));
out.push('|---|---|---' + (hasPW ? '|---|' : '|'));
out.push(`| passed runs | ${sum(rows.map((r) => r.b.ok))}/${sum(rows.map((r) => r.b.n))} | ${sum(rows.map((r) => r.a.ok))}/${sum(rows.map((r) => r.a.n))}` + (hasPW ? ` | ${sum(rows.map((r) => r.p.ok))}/${sum(rows.map((r) => r.p.n))} |` : ' |'));
out.push(`| tasks with every run passing | ${rows.filter((r) => r.b.n && r.b.ok === r.b.n).length}/${rows.length} | ${rows.filter((r) => r.a.n && r.a.ok === r.a.n).length}/${rows.length}` + (hasPW ? ` | ${rows.filter((r) => r.p.n && r.p.ok === r.p.n).length}/${rows.length} |` : ' |'));
out.push(`| median cold ms, tasks passing before and after (${both.length}) | ${f0(median(both.map((r) => r.b.cold)))} | ${f0(median(both.map((r) => r.a.cold)))}` + (hasPW ? ' | |' : ' |'));
if (hasPW) {
  out.push(`| median cold ms, tasks passing for after and playwright (${vsPW.length}) | | ${f0(median(vsPW.map((r) => r.a.cold)))} | ${f0(median(vsPW.map((r) => r.p.cold)))} |`);
  out.push(`| tasks where after is faster / slower than playwright | | ${vsPW.filter((r) => r.a.cold < r.p.cold).length} / ${vsPW.filter((r) => r.a.cold >= r.p.cold).length} | |`);
  const tk = rows.filter((r) => r.a.tokens != null && r.p.tokens != null);
  out.push(`| median snapshot tokens (est.), ${tk.length} tasks | ${f0(median(rows.filter((r) => r.b.tokens != null).map((r) => r.b.tokens)))} | ${f0(median(tk.map((r) => r.a.tokens)))} | ${f0(median(tk.map((r) => r.p.tokens)))} |`);
}
out.push('');
if (hasPW) {
  out.push('## By category (after vs playwright, tasks both pass)', '', '| category | tasks | after faster | median after ms | median playwright ms |', '|---|---|---|---|---|');
  for (const c of [...new Set(rows.map((r) => r.cat))].sort()) {
    const rs = vsPW.filter((r) => r.cat === c);
    if (!rs.length) continue;
    out.push(`| ${c} | ${rs.length} | ${rs.filter((r) => r.a.cold < r.p.cold).length} | ${f0(median(rs.map((r) => r.a.cold)))} | ${f0(median(rs.map((r) => r.p.cold)))} |`);
  }
  out.push('');
}
out.push('## Every task', '', '| task | category | before ms | after ms | after vs before | passes before/after' + (hasPW ? '/pw | playwright ms | after vs playwright | tokens after/pw |' : ' |'));
out.push('|---|---|---|---|---|---' + (hasPW ? '|---|---|---|' : '|'));
for (const r of rows) {
  out.push(`| ${r.id} | ${r.cat} | ${f0(r.b.cold)} | ${f0(r.a.cold)} | ${ratio(r.a.cold, r.b.cold)} | ${r.b.ok}/${r.b.n} ${r.a.ok}/${r.a.n}` +
    (hasPW ? ` ${r.p.ok}/${r.p.n} | ${f0(r.p.cold)} | ${ratio(r.a.cold, r.p.cold)} | ${f0(r.a.tokens)}/${f0(r.p.tokens)} |` : ' |'));
}
out.push('', '## Slowest 5 (after)', '');
for (const r of rows.filter((r) => r.a.cold != null).sort((x, y) => y.a.cold - x.a.cold).slice(0, 5)) out.push(`- ${r.id}: ${f0(r.a.cold)} ms` + (r.p?.cold != null ? ` (playwright ${f0(r.p.cold)} ms)` : ''));
const fails = rows.filter((r) => r.a.ok < r.a.n);
if (fails.length) {
  out.push('', '## Failing runs (after)', '');
  for (const r of fails) {
    const errs = [...new Set(A.runs.filter((x) => x.task === r.id && x.tool === at && !x.ok).map((x) => (x.fail_class ? x.fail_class + ': ' : '') + String(x.error || '').replace(/\s+/g, ' ').slice(0, 120)))];
    out.push(`- ${r.id} ${r.a.ok}/${r.a.n}: ${errs.join(' | ')}`);
  }
}
console.log(out.join('\n'));
