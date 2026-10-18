// Chromium reference for the replay-proxy page set (bench/replay): median time to a usable snapshot, cold and warm
// (second visit in the same browser). node chrome_replay.mjs <runs> [proxy]   (default proxy 127.0.0.1:8898)
import { performance } from 'node:perf_hooks';
import { chromium } from 'playwright-core';
import { CHROMIUM, sleep } from './lib.mjs';

const N = +process.argv[2] || 5;
const proxy = process.argv[3] || '127.0.0.1:8898';
const PAGES = [
  ['gh-golang', 'https://github.com/golang/go', /golang/],
  ['gh-issue9', 'https://github.com/golang/go/issues/9', /already used the name/],
  ['mdn-fetch', 'https://developer.mozilla.org/en-US/docs/Web/API/Window/fetch', /fetch\(\)/],
  ['mdn-map', 'https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/Array/map', /Array\.prototype\.map\(\)/],
  ['pypi', 'https://pypi.org/project/requests/', /requests/],
];
const med = (a) => [...a].sort((x, y) => x - y)[a.length >> 1];
for (const [name, url, chk] of PAGES) {
  const cold = [], warm = [];
  for (let i = 0; i < N; i++) {
    const b = await chromium.launch({ executablePath: CHROMIUM, headless: true, args: ['--no-sandbox', `--proxy-server=http://${proxy}`, '--proxy-bypass-list=<-loopback>'] });
    const ctx = await b.newContext({ ignoreHTTPSErrors: true });
    const page = await ctx.newPage();
    const visit = async () => {
      const t = performance.now();
      await page.goto(url, { waitUntil: 'load', timeout: 30000 }).catch(() => {});
      for (;;) {
        const s = await page._snapshotForAI().catch(() => '');
        const txt = typeof s === 'string' ? s : (s.full || '');
        if (chk.test(txt) || performance.now() - t > 20000) break;
        await sleep(100);
      }
      return performance.now() - t;
    };
    cold.push(await visit());
    warm.push(await visit());
    await b.close();
  }
  console.log(name, 'cold', Math.round(med(cold)), 'warm', Math.round(med(warm)), cold.map(Math.round), warm.map(Math.round));
}
