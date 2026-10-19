// Shared helpers for the real-site benchmark (Mode A and Mode B tooling).
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export const HERE = path.dirname(fileURLToPath(import.meta.url));
export const CHROMIUM = process.env.CHROMIUM_PATH || '/opt/pw-browsers/chromium';

export function loadTasks(file = path.join(HERE, '..', 'tasks.json'), only = null) {
  const all = JSON.parse(fs.readFileSync(file, 'utf8')).tasks;
  if (!only || only.length === 0) return all;
  const set = new Set(only);
  return all.filter((t) => set.has(t.id));
}

export const median = (xs) => {
  const a = xs.filter((x) => Number.isFinite(x)).sort((x, y) => x - y);
  if (!a.length) return null;
  const m = a.length >> 1;
  return a.length % 2 ? a[m] : (a[m - 1] + a[m]) / 2;
};

// A snapshot token estimate. There is no tokenizer in this harness: bytes/4 is
// the same rule of thumb the Sweef agent uses internally. Report it as an
// estimate, never as a billed token count.
export const estTokens = (s) => Math.ceil(Buffer.byteLength(s, 'utf8') / 4);

// Engine error banners rendered into a page. When a script dies inside the engine, frameworks often print the
// error where the content should be (QuickJS: "InternalError: out of memory" for the JS heap cap, "InternalError:
// stack overflow", "InternalError: interrupted" for the script timeout). A page text that contains one of these
// is a failed run even when the expected keywords are elsewhere on the page, so every check reports it as
// fail_class "engine_error". The patterns are engine-generated strings, not site wording, and apply to every tool.
export const ENGINE_ERROR_RE = /InternalError:\s*(out of memory|stack overflow|interrupted)/i;
export function engineBanner(text) {
  const m = ENGINE_ERROR_RE.exec(text || '');
  return m ? m[0].slice(0, 80) : null;
}

export const re = (pattern) => new RegExp(pattern, 'i');
export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// USABLE_POLL_MS / USABLE_MAX_MS: after navigation, a snapshot counts as
// usable only when it contains the task's expected text. Both tools use the
// same polling policy so SPA pages that render late are measured, not skipped.
export const USABLE_POLL_MS = 250;
export const USABLE_MAX_MS = 10000;

// rmTree removes a per-run directory. A daemon that is still shutting down may write into its cache while the
// tree is removed (ENOTEMPTY); retry, and never let cleanup end a benchmark run.
export function rmTree(dir) {
  try { fs.rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 200 }); }
  catch (e) { console.error(`warning: could not remove ${dir}: ${e.message}`); }
}
