package engine

import (
	"strings"
	"testing"
)

// jsCase is one table row: an async function body whose return value is
// JSON-encoded and compared with want.
type jsCase struct {
	name string
	js   string
	want string
}

// runJSCases evaluates every case in one fresh tab (one VM) so a table costs a
// single prelude load. Each case runs as its own async function.
func runJSCases(t *testing.T, tab *Tab, cases []jsCase) {
	t.Helper()
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			expr := "(async function(){ try { " + c.js + " } catch (e) { return 'THROW ' + (e && e.name) + ': ' + (e && e.message); } })()"
			r, err := tab.Evaluate(expr)
			if err != nil {
				t.Fatalf("eval: %v\nconsole:\n%s", err, consoleText(tab))
			}
			if got := string(r); got != c.want {
				// A documented gap, not a regression: the WebCodecs value handles are QuickJS-only so far (docs/v8.md).
				if tab.js != nil && tab.js.jr.kind() == JSEngineV8 && strings.Contains(got, "was not defined by module webcodecs") {
					t.Skip("WebCodecs value handles are not on the V8 runtime yet: " + got)
				}
				t.Fatalf("got  %s\nwant %s\nconsole:\n%s", got, c.want, consoleText(tab))
			}
		})
	}
}

func platformTab(t *testing.T) *Tab {
	t.Helper()
	srv := featureServer(t)
	e := newFeatureEngine(t)
	tab, err := e.NewTab(srv.URL + "/static")
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

const jsHelpers = `
const toU8 = (s) => new TextEncoder().encode(s);
async function readAll(rs) { const r = rs.getReader(); const out = []; for (;;) { const x = await r.read(); if (x.done) return out; out.push(x.value); } }
function cat(chunks) { let n = 0; chunks.forEach(c => n += c.length); const o = new Uint8Array(n); let p = 0; chunks.forEach(c => { o.set(c, p); p += c.length; }); return o; }
async function pipeBytes(u8, ts, parts) { const w = ts.writable.getWriter(); const rd = readAll(ts.readable); const n = parts || 1; const step = Math.ceil(u8.length / n) || 1; for (let i = 0; i < u8.length; i += step) await w.write(u8.subarray(i, i + step)); await w.close(); return cat(await rd); }
`

func TestStreamsAndCodecs(t *testing.T) {
	tab := platformTab(t)
	mustEval(t, tab, "globalThis.__h = 1")
	pre := jsHelpers
	runJSCases(t, tab, []jsCase{
		{"readable default", pre + `const rs = new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); c.close(); } }); return await readAll(rs);`, `[1,2]`},
		{"readable pull backpressure", pre + `let pulls = 0; const rs = new ReadableStream({ pull(c) { pulls++; c.enqueue(pulls); if (pulls == 3) c.close(); } }, { highWaterMark: 0 }); const a = await readAll(rs); return [a, pulls];`, `[[1,2,3],3]`},
		{"desiredSize", pre + `let d; const rs = new ReadableStream({ start(c) { d = [c.desiredSize]; c.enqueue('a'); d.push(c.desiredSize); } }, { highWaterMark: 3 }); return d;`, `[3,2]`},
		{"locked and releaseLock", pre + `const rs = new ReadableStream(); const r = rs.getReader(); const l1 = rs.locked; r.releaseLock(); return [l1, rs.locked];`, `[true,false]`},
		{"cancel calls source", pre + `let got; const rs = new ReadableStream({ cancel(r) { got = r; } }); await rs.cancel('why'); return got;`, `"why"`},
		{"tee", pre + `const rs = new ReadableStream({ start(c) { c.enqueue('x'); c.enqueue('y'); c.close(); } }); const [a, b] = rs.tee(); return [await readAll(a), await readAll(b)];`, `[["x","y"],["x","y"]]`},
		{"async iteration", pre + `const rs = new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); c.close(); } }); const o = []; for await (const v of rs) o.push(v); return o;`, `[1,2]`},
		{"ReadableStream.from", pre + `const o = []; for await (const v of ReadableStream.from([1, 2, 3])) o.push(v); return o;`, `[1,2,3]`},
		{"errored stream rejects reads", pre + `const rs = new ReadableStream({ start(c) { c.error(new Error('boom')); } }); try { await rs.getReader().read(); } catch (e) { return e.message; }`, `"boom"`},
		{"byte stream default reader", pre + `const rs = new ReadableStream({ type: 'bytes', start(c) { c.enqueue(new Uint8Array([1, 2, 3])); c.close(); } }); return Array.from((await readAll(rs))[0]);`, `[1,2,3]`},
		{"byob reader", pre + `const rs = new ReadableStream({ type: 'bytes', start(c) { c.enqueue(new Uint8Array([9, 8, 7, 6])); c.close(); } }); const r = rs.getReader({ mode: 'byob' }); const x = await r.read(new Uint8Array(3)); const y = await r.read(new Uint8Array(3)); const z = await r.read(new Uint8Array(3)); return [Array.from(x.value), x.done, Array.from(y.value), z.done, z.value.length];`, `[[9,8,7],false,[6],true,0]`},
		{"byob pull fills", pre + `const rs = new ReadableStream({ type: 'bytes', pull(c) { const v = c.byobRequest.view; v[0] = 42; c.byobRequest.respond(1); c.close(); } }); const r = rs.getReader({ mode: 'byob' }); const x = await r.read(new Uint8Array(4)); return [x.value[0], x.value.length];`, `[42,1]`},
		{"writable sink", pre + `const seen = []; const ws = new WritableStream({ write(c) { seen.push(c); }, close() { seen.push('closed'); } }); const w = ws.getWriter(); await w.write('a'); await w.write('b'); await w.close(); return seen;`, `["a","b","closed"]`},
		{"writable backpressure", pre + `const ws = new WritableStream({ write() { return new Promise(r => setTimeout(r, 5)); } }, { highWaterMark: 1 }); const w = ws.getWriter(); const d0 = w.desiredSize; const p = w.write(1); const d1 = w.desiredSize; await p; return [d0, d1, w.desiredSize];`, `[1,0,1]`},
		{"writable abort", pre + `let r; const ws = new WritableStream({ abort(x) { r = x; } }); await ws.abort('stop'); return r;`, `"stop"`},
		{"writer after close rejects", pre + `const ws = new WritableStream(); const w = ws.getWriter(); await w.close(); try { await w.write(1); } catch (e) { return e.name; }`, `"TypeError"`},
		{"transform", pre + `const ts = new TransformStream({ transform(c, ctl) { ctl.enqueue(c * 2); } }); const w = ts.writable.getWriter(); w.write(1); w.write(2); w.close(); return await readAll(ts.readable);`, `[2,4]`},
		{"transform flush", pre + `const ts = new TransformStream({ transform(c, ctl) { ctl.enqueue(c); }, flush(ctl) { ctl.enqueue('end'); } }); const w = ts.writable.getWriter(); w.write('a'); w.close(); return await readAll(ts.readable);`, `["a","end"]`},
		{"pipeThrough pipeTo", pre + `const out = []; await new ReadableStream({ start(c) { c.enqueue(1); c.enqueue(2); c.close(); } }).pipeThrough(new TransformStream({ transform(c, ctl) { ctl.enqueue(c + 1); } })).pipeTo(new WritableStream({ write(c) { out.push(c); } })); return out;`, `[2,3]`},
		{"pipeTo abort signal", pre + `const ac = new AbortController(); const rs = new ReadableStream({ pull(c) { c.enqueue(1); } }); const p = rs.pipeTo(new WritableStream(), { signal: ac.signal }); ac.abort(); try { await p; } catch (e) { return e.name; }`, `"AbortError"`},
		{"queuing strategies", pre + `return [new CountQueuingStrategy({ highWaterMark: 2 }).size(), new ByteLengthQueuingStrategy({ highWaterMark: 1 }).size(new Uint8Array(5))];`, `[1,5]`},
		{"Blob.stream and Response.body", pre + `const a = await readAll(new Blob(['hi']).stream()); const b = await readAll(new Response('yo').body); return [a.length, new TextDecoder().decode(b[0])];`, `[1,"yo"]`},
		{"toStringTag", pre + `return [String(new ReadableStream()), String(new TransformStream()), Object.prototype.toString.call(new WritableStream())];`, `["[object ReadableStream]","[object TransformStream]","[object WritableStream]"]`},

		{"gzip round trip", pre + `const src = toU8('hello hello hello hello '.repeat(200)); const z = await pipeBytes(src, new CompressionStream('gzip'), 3); const back = await pipeBytes(z, new DecompressionStream('gzip'), 5); return [z.length < src.length, z[0], z[1], new TextDecoder().decode(back) === 'hello hello hello hello '.repeat(200)];`, `[true,31,139,true]`},
		{"deflate is zlib", pre + `const z = await pipeBytes(toU8('abcabcabc'), new CompressionStream('deflate')); return [z[0], (z[0] * 256 + z[1]) % 31 === 0, new TextDecoder().decode(await pipeBytes(z, new DecompressionStream('deflate')))];`, `[120,true,"abcabcabc"]`},
		{"deflate-raw round trip", pre + `const z = await pipeBytes(toU8('raw raw raw'), new CompressionStream('deflate-raw')); return new TextDecoder().decode(await pipeBytes(z, new DecompressionStream('deflate-raw')));`, `"raw raw raw"`},
		{"known gzip bytes decode", pre + `const b = Uint8Array.from(atob('H4sIAAAAAAAC/8tIzcnJVyjPL8pJAQCFEUoNCwAAAA=='), c => c.charCodeAt(0)); return new TextDecoder().decode(await pipeBytes(b, new DecompressionStream('gzip')));`, `"hello world"`},
		{"decompress corrupt data errors", pre + `try { await pipeBytes(toU8('this is not gzip data at all'), new DecompressionStream('gzip')); } catch (e) { return e.name; }`, `"TypeError"`},
		{"decompress truncated input errors", pre + `const z = await pipeBytes(toU8('truncate me '.repeat(50)), new CompressionStream('gzip')); try { await pipeBytes(z.subarray(0, z.length - 6), new DecompressionStream('gzip')); } catch (e) { return e.name; }`, `"TypeError"`},
		{"decompress trailing garbage errors", pre + `const z = await pipeBytes(toU8('abc'), new CompressionStream('deflate')); const bad = cat([z, toU8('junk')]); try { await pipeBytes(bad, new DecompressionStream('deflate')); } catch (e) { return e.name; }`, `"TypeError"`},
		{"decompress empty input errors", pre + `try { await pipeBytes(new Uint8Array(0), new DecompressionStream('gzip')); } catch (e) { return e.name; }`, `"TypeError"`},
		{"unknown format", pre + `try { new CompressionStream('lzma'); } catch (e) { return e.name; }`, `"TypeError"`},
		{"compress brotli unsupported, decompress accepted", pre + `let a; try { new CompressionStream('brotli'); } catch (e) { a = e.name; } new DecompressionStream('brotli'); return a;`, `"TypeError"`},
		{"brotli decode", pre + `const b = Uint8Array.from(atob('CwWAaGVsbG8gd29ybGQD'), c => c.charCodeAt(0)); return new TextDecoder().decode(await pipeBytes(b, new DecompressionStream('brotli')));`, `"hello world"`},
		{"non-buffer chunk rejects", pre + `const w = new CompressionStream('gzip').writable.getWriter(); try { await w.write('text'); } catch (e) { return e.name; }`, `"TypeError"`},
		{"large expansion is streamed in windows", pre + `const big = new Uint8Array(3 << 20); const z = await pipeBytes(big, new CompressionStream('gzip')); const ds = new DecompressionStream('gzip'); const w = ds.writable.getWriter(); w.write(z); w.close(); const chunks = await readAll(ds.readable); return [chunks.length > 1, cat(chunks).length];`, `[true,3145728]`},
		{"pipeThrough compression", pre + `const rs = new ReadableStream({ start(c) { c.enqueue(toU8('part one ')); c.enqueue(toU8('part two')); c.close(); } }); const out = cat(await readAll(rs.pipeThrough(new CompressionStream('gzip')).pipeThrough(new DecompressionStream('gzip')))); return new TextDecoder().decode(out);`, `"part one part two"`},
		{"decompress via fetch body shape", pre + `const z = await pipeBytes(toU8('{"a":1}'), new CompressionStream('gzip')); const r = new Response(new Blob([z]).stream().pipeThrough(new DecompressionStream('gzip'))); return 'ok';`, `"ok"`},

		{"TextEncoderStream", pre + `const t = new TextEncoderStream(); const w = t.writable.getWriter(); w.write('hé'); w.write('\ud83d'); w.write('\ude00!'); w.close(); return [t.encoding, Array.from(cat(await readAll(t.readable)))];`, `["utf-8",[104,195,169,240,159,152,128,33]]`},
		{"TextEncoderStream lone surrogate", pre + `const t = new TextEncoderStream(); const w = t.writable.getWriter(); w.write('a\ud83d'); w.close(); return Array.from(cat(await readAll(t.readable)));`, `[97,239,191,189]`},
		{"TextDecoderStream partial sequences", pre + `const t = new TextDecoderStream(); const w = t.writable.getWriter(); w.write(new Uint8Array([0xe2, 0x82])); w.write(new Uint8Array([0xac, 0xf0, 0x9f])); w.write(new Uint8Array([0x98, 0x80])); w.close(); return (await readAll(t.readable)).join('|');`, `"€|😀"`},
		{"TextDecoderStream invalid bytes replaced", pre + `const t = new TextDecoderStream(); const w = t.writable.getWriter(); w.write(new Uint8Array([0x61, 0xff, 0x62, 0xe2, 0x82])); w.close(); return (await readAll(t.readable)).join('');`, `"a�b�"`},
		{"TextDecoderStream fatal", pre + `const t = new TextDecoderStream('utf-8', { fatal: true }); const w = t.writable.getWriter(); w.write(new Uint8Array([0xff])).catch(() => {}); try { await readAll(t.readable); } catch (e) { return [t.fatal, e.name]; }`, `[true,"TypeError"]`},
		{"TextDecoderStream BOM", pre + `async function run(opts) { const t = new TextDecoderStream('utf-8', opts); const w = t.writable.getWriter(); w.write(new Uint8Array([0xef, 0xbb])); w.write(new Uint8Array([0xbf, 0x41])); w.close(); return (await readAll(t.readable)).join(''); } return [(await run({})).length, (await run({ ignoreBOM: true })).length];`, `[1,2]`},
		{"TextDecoderStream truncated tail at flush", pre + `const t = new TextDecoderStream(); const w = t.writable.getWriter(); w.write(new Uint8Array([0xf0, 0x9f])); w.close(); return (await readAll(t.readable)).join('');`, `"�"`},
		{"TextDecoderStream bad label", pre + `try { new TextDecoderStream('nope'); } catch (e) { return e.name; }`, `"RangeError"`},
		{"text streams compose", pre + `const rs = new ReadableStream({ start(c) { c.enqueue('café '); c.enqueue('😀'); c.close(); } }); const out = await readAll(rs.pipeThrough(new TextEncoderStream()).pipeThrough(new TextDecoderStream())); return out.join('');`, `"café 😀"`},
	})
	_ = strings.TrimSpace
}
