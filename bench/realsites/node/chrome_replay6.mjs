// Chromium reference for the six replay pages: median time to a usable snapshot, cold and warm (second visit in the
// same browser), as chrome_replay.mjs, plus react.dev, learn.microsoft.com and nextjs.org.
//   node chrome_replay6.mjs <runs> [only-name] [proxy]
import { performance } from 'node:perf_hooks';
import { chromium } from 'playwright-core';
import { CHROMIUM, sleep } from './lib.mjs';

const N = +process.argv[2] || 5;
const only = process.argv[3] || '';
const proxy = process.argv[4] || '127.0.0.1:8898';
const PAGES = [
  ['mdnfetch', 'https://developer.mozilla.org/en-US/docs/Web/API/Window/fetch', /fetch\(\)/],
  ['mdnmap', 'https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/Array/map', /Array\.prototype\.map\(\)/],
  ['pypi', 'https://pypi.org/project/requests/', /requests/],
  ['react', 'https://react.dev/', /React/],
  ['learn', 'https://learn.microsoft.com/en-us/dotnet/', /\.NET/],
  ['nextjs', 'https://nextjs.org/docs', /Next\.js/],
];
const med = (a) => [...a].sort((x, y) => x - y)[a.length >> 1];
for (const [name, url, chk] of PAGES) {
  if (only && only !== name) continue;
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
  console.log(name, 'chromium cold', Math.round(med(cold)), 'warm', Math.round(med(warm)), cold.map(Math.round).join(','), '|', warm.map(Math.round).join(','));
}
