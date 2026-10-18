package engine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// httpDo joins the body from chunks: sizes around the chunk boundaries (empty, one read, several MiB,
// unknown length) must come back byte for byte.
func TestHTTPBodyChunkedRead(t *testing.T) {
	mk := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('a' + i*7%26)
		}
		return b
	}
	sizes := []int{0, 1, 32 << 10, 32<<10 + 1, 1<<20 - 1, 1 << 20, 5<<20 + 12345, 9 << 20}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n int
		for _, c := range r.URL.Query().Get("n") {
			n = n*10 + int(c-'0')
		}
		w.Header().Set("Content-Type", "text/plain")
		if r.URL.Query().Get("chunked") == "1" {
			w.(http.Flusher).Flush() // no Content-Length
		} else {
			w.Header().Set("Content-Length", itoa(n))
		}
		_, _ = w.Write(mk(n))
	}))
	defer srv.Close()
	e := New(DefaultOptions())
	defer e.Close()
	tab, err := e.NewTab("about:blank")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range sizes {
		for _, q := range []string{"", "&chunked=1"} {
			body, status, err := tab.fetchRes(srv.URL+"/x?n="+itoa(n)+q, fetchOpts{kind: KindOther})
			if err != nil || status != 200 {
				t.Fatalf("n=%d%s: status %d err %v", n, q, status, err)
			}
			if !bytes.Equal([]byte(body), mk(n)) {
				t.Fatalf("n=%d%s: body differs (got %d bytes)", n, q, len(body))
			}
		}
	}
}
