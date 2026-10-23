package engine

import "testing"

func TestCompressionStreams(t *testing.T) {
	tab := newTabForLoad(t)
	var r map[string]any
	evalJSON(t, tab, `(async function () {
	  var o = {};
	  var text = 'hello hello hello hello hello streaming world '.repeat(40);
	  var enc = new TextEncoder().encode(text);
	  async function run(stream, bytes) {
	    var w = stream.writable.getWriter();
	    w.write(bytes); w.close();
	    return new Uint8Array(await new Response(stream.readable).arrayBuffer());
	  }
	  for (var f of ['gzip', 'deflate', 'deflate-raw']) {
	    var c = await run(new CompressionStream(f), enc);
	    var d = await run(new DecompressionStream(f), c);
	    o[f] = [c.length < enc.length, new TextDecoder().decode(d) === text, c[0], c[1]];
	  }
	  try { new CompressionStream('brotli'); } catch (e) { o.badFormat = e.name; }
	  try { await run(new DecompressionStream('gzip'), new Uint8Array([1, 2, 3, 4])); o.badData = 'no error'; } catch (e) { o.badData = e.name; }
	  return o;
	})()`, &r)
	for _, f := range []string{"gzip", "deflate", "deflate-raw"} {
		v, _ := r[f].([]any)
		if len(v) < 2 || v[0] != true || v[1] != true {
			t.Errorf("%s round trip: %v", f, r[f])
		}
	}
	if g, _ := r["gzip"].([]any); len(g) == 4 && (g[2] != float64(0x1f) || g[3] != float64(0x8b)) {
		t.Errorf("gzip output does not start with the gzip magic: %v", g)
	}
	if r["badFormat"] != "TypeError" || r["badData"] != "TypeError" {
		t.Errorf("errors: %v %v", r["badFormat"], r["badData"])
	}
}
