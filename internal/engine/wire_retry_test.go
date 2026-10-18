package engine

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A fresh connection that is reset before any response byte is retried once for a GET (the origin or a proxy
// dropped it), and the retry is bounded: a second reset surfaces as the error. Requests with a body are never
// replayed.
func TestMeterRTRetriesResetBeforeResponse(t *testing.T) {
	for _, tc := range []struct {
		resets  int
		method  string
		wantErr bool
		wantN   int32
	}{
		{1, "GET", false, 2},
		{2, "GET", true, 2},
		{1, "POST", true, 1},
	} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if int(n.Add(1)) <= tc.resets {
				c, _, _ := w.(http.Hijacker).Hijack()
				c.(*net.TCPConn).SetLinger(0) // close sends RST
				c.Close()
				return
			}
			io.WriteString(w, "ok")
		}))
		var w wireCounters
		tr := newTransport(&w, "direct")
		cl := &http.Client{Transport: &meterRT{base: tr}}
		e := New(DefaultOptions())
		req, _ := http.NewRequest(tc.method, srv.URL+"/x", nil)
		req, _ = withMeter(req, e.DefaultContext(), nil, KindDocument, PolicyFor(SaverStandard))
		res, err := cl.Do(req)
		if (err != nil) != tc.wantErr || n.Load() != tc.wantN {
			t.Fatalf("%+v: err=%v attempts=%d", tc, err, n.Load())
		}
		if err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if string(b) != "ok" {
				t.Fatalf("body %q", b)
			}
		}
		tr.CloseIdleConnections()
		e.Close()
		srv.Close()
	}
}
