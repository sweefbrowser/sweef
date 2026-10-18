package engine

// wire.go: exact socket accounting and per-request attribution.
//
// Two separate measurements exist and are never mixed:
//
//   - Wire counters (wireCounters): every net.Conn the HTTP transport dials is
//     wrapped, and each Read and Write adds to atomic counters. The connection
//     under TLS is the one wrapped, so the numbers are what crossed the socket
//     (TLS records and handshakes included, TCP/IP headers not). Each
//     BrowserContext has its own transport and so its own counters; every
//     context also feeds the engine total.
//   - Attribution (meterRT): a RoundTripper wrapper measures, per HTTP
//     exchange, the request header and body bytes written and the response
//     header bytes plus the raw (still compressed) body bytes read, and
//     charges them to a kind (script, image, ...) and an origin. Header sizes
//     are computed from the parsed headers, so they are close to, not equal
//     to, the bytes on the wire (HTTP/2 compresses headers; TLS adds framing).
//     That is why these numbers are labelled approximate everywhere.
//
// The wrapper also decodes Content-Encoding (gzip and, when the policy asks
// for it, brotli) itself: asking for br is only safe when the engine can
// decode it, and counting the compressed body bytes needs the raw stream.

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dsnet/compress/brotli"
)

// wireCounters counts bytes read from and written to sockets. Counts also
// propagate to parent (the engine total).
type wireCounters struct {
	in, out atomic.Int64
	parent  *wireCounters
}

func (w *wireCounters) addIn(n int) {
	for c := w; c != nil; c = c.parent {
		c.in.Add(int64(n))
	}
}

func (w *wireCounters) addOut(n int) {
	for c := w; c != nil; c = c.parent {
		c.out.Add(int64(n))
	}
}

type countConn struct {
	net.Conn
	w *wireCounters
}

func (c *countConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.w.addIn(n)
	}
	return n, err
}

func (c *countConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.w.addOut(n)
	}
	return n, err
}

// proxyFunc resolves the proxy for a request. An explicit URL (ContextOptions.ProxyURL, then
// Options.ProxyURL) wins; "direct" or "none" turns proxying off; with neither, HTTPS_PROXY,
// HTTP_PROXY and NO_PROXY apply (http.ProxyFromEnvironment, which never proxies loopback). An
// explicit URL that does not parse makes every request fail instead of silently going direct.
func proxyFunc(explicit string) func(*http.Request) (*url.URL, error) {
	explicit = strings.TrimSpace(explicit)
	switch strings.ToLower(explicit) {
	case "":
		return http.ProxyFromEnvironment
	case "direct", "none", "direct://":
		return func(*http.Request) (*url.URL, error) { return nil, nil }
	}
	if !strings.Contains(explicit, "://") {
		explicit = "http://" + explicit
	}
	u, err := url.Parse(explicit)
	if err != nil || u.Host == "" {
		bad := fmt.Errorf("invalid proxy %q", explicit)
		return func(*http.Request) (*url.URL, error) { return nil, bad }
	}
	return http.ProxyURL(u)
}

// countedDial dials through the counting wrapper: whatever crosses the socket (a proxy
// CONNECT, the TLS handshake and records on top of it) lands in w.
func countedDial(w *wireCounters) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, mapDialAddr(addr))
		if err != nil {
			return nil, err
		}
		return &countConn{Conn: c, w: w}, nil
	}
}

// newTransport builds the bounded transport used for every request. Dialing
// goes through a counting wrapper; ForceAttemptHTTP2 keeps HTTP/2 enabled,
// which a custom DialContext would otherwise turn off. proxyURL is the explicit
// proxy ("" = environment), see proxyFunc.
func newTransport(w *wireCounters, proxyURL string) *http.Transport {
	return newTransportWindow(w, proxyURL, 0)
}

// cappedStreamWindow is the HTTP/2 receive window of the transport used for requests with a size cap
// (Data Saver image and script caps). Aborting such a response resets the stream, but the server has already
// been allowed to send one window of body: with the default 4 MiB window a 1.4 MB image costs its full size
// even though the cap said no (measured, see docs/data-saver-engine.md). A 64 KiB window bounds the waste per
// skipped resource; images under the cap (200 KB by default) take a few round trips instead of one.
const cappedStreamWindow = 64 << 10

// newTransportWindow is newTransport with an explicit HTTP/2 per-stream receive window (0 = the default).
func newTransportWindow(w *wireCounters, proxyURL string, streamWindow int) *http.Transport {
	tr := &http.Transport{
		Proxy:               proxyFunc(proxyURL),
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 6, // a browser's 6 connections per host: a page's stylesheets, scripts and images come in parallel
		// and an HTTP/1 origin keeps what they opened for the next navigation (HTTP/2 hosts need one)
		IdleConnTimeout:   60 * time.Second,
		ForceAttemptHTTP2: true,
		DialContext:       countedDial(w),
	}
	if streamWindow > 0 {
		tr.HTTP2 = &http.HTTP2Config{MaxReceiveBufferPerStream: streamWindow, MaxReceiveBufferPerConnection: 1 << 20}
	}
	return tr
}

// ---- per-request attribution ------------------------------------------------

type meterKeyT struct{}

var meterKey meterKeyT

// meterInfo travels in the request context from the call site to meterRT.
type meterInfo struct {
	c    *BrowserContext
	t    *Tab // nil for context-level requests
	kind DataKind
	pol  DataPolicy

	// final hop, written by meterRT
	noCreds  bool         // the request carries no cookies and keeps none (CORS without credentials)
	maxBody  int64        // transfer-size cap of the request (0 = none), set before the request is made
	declared atomic.Int64 // Content-Length as received (transfer size), -1 unknown
	hdrBytes atomic.Int64
	rawBytes atomic.Int64

	// cache outcome of the final hop, written by cacheRT (httpcache_rt.go)
	cachedXfer atomic.Int64 // what the stored response cost when it was downloaded
	fromCache  atomic.Bool  // answered from the cache (hit, revalidated or stale-if-error)
	cacheMu    sync.Mutex
	cacheNote  string
}

func (mi *meterInfo) setCache(s string) {
	mi.cacheMu.Lock()
	mi.cacheNote = s
	mi.cacheMu.Unlock()
}

// cacheState is how the cache treated the request: "hit", "revalidated", "stale-if-error", "miss: <why>",
// "bypass" or "" (no cache involved).
func (mi *meterInfo) cacheState() string {
	mi.cacheMu.Lock()
	defer mi.cacheMu.Unlock()
	return mi.cacheNote
}

// meterRT measures every exchange that carries a meterInfo and decodes
// Content-Encoding. Requests without one pass through untouched.
type meterRT struct {
	base http.RoundTripper
	// capped serves requests with a size cap (meterInfo.maxBody > 0) over connections with a small HTTP/2
	// window, see cappedStreamWindow. nil = use base.
	capped http.RoundTripper
}

func (m *meterRT) RoundTrip(req *http.Request) (*http.Response, error) {
	mi, _ := req.Context().Value(meterKey).(*meterInfo)
	if mi == nil {
		return m.base.RoundTrip(req)
	}
	// RoundTrippers must not modify the request: work on a shallow copy
	r2 := new(http.Request)
	*r2 = *req
	r2.Header = req.Header.Clone()
	if r2.Header == nil {
		r2.Header = http.Header{}
	}
	pol := mi.pol
	if pol.SaveDataHeader && r2.Header.Get("Save-Data") == "" {
		r2.Header.Set("Save-Data", "on")
	}
	// Taking charge of Accept-Encoding means decoding it ourselves. Range
	// requests stay as they are (the transport does not compress those).
	if r2.Header.Get("Accept-Encoding") == "" && r2.Header.Get("Range") == "" && r2.Method != http.MethodHead {
		r2.Header.Set("Accept-Encoding", "br, gzip") // every level: the engine decodes both itself
	}
	out := requestBytes(r2)
	rt := m.base
	if mi.maxBody > 0 && m.capped != nil {
		rt = m.capped
	}
	res, err := rt.RoundTrip(r2)
	origin := r2.URL.Scheme + "://" + r2.URL.Host
	if err != nil && retryableReset(r2, err) {
		// The peer (or a proxy in front of it) dropped a fresh connection before any response byte came back.
		// net/http already replays on a reused connection; Firefox also restarts such an idempotent request once
		// on a new one. The first attempt's bytes stay charged.
		mi.charge(r2.URL.Scheme, origin, out, 0)
		r2 = r2.Clone(r2.Context())
		res, err = rt.RoundTrip(r2)
	}
	if err != nil {
		mi.charge(r2.URL.Scheme, origin, out, 0)
		return nil, err
	}
	hdr := responseHeaderBytes(res)
	mi.declared.Store(res.ContentLength)
	mi.hdrBytes.Store(hdr)
	mi.rawBytes.Store(0)
	body := &meteredBody{rc: res.Body, mi: mi, origin: origin, scheme: r2.URL.Scheme, out: out, hdr: hdr}
	if r2.Method != http.MethodHead && res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusNotModified {
		switch strings.ToLower(strings.TrimSpace(res.Header.Get("Content-Encoding"))) {
		case "gzip", "x-gzip":
			body.enc = "gzip"
		case "br":
			body.enc = "br"
		}
	}
	if body.enc != "" {
		res.Header.Del("Content-Encoding")
		res.Header.Del("Content-Length")
		res.ContentLength = -1
		res.Uncompressed = true
	}
	res.Body = body
	return res, nil
}

// retryableReset reports whether err is a connection that went away before the response started (reset by
// peer, broken pipe or an EOF with no bytes) for a request that is safe to send again: GET or HEAD without a
// body, and a context that is still live.
func retryableReset(r *http.Request, err error) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Body != nil && r.Body != http.NoBody {
		return false
	}
	if r.Context().Err() != nil {
		return false
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// charge books one exchange against the tab (if any) and the context.
func (mi *meterInfo) charge(scheme, origin string, out, in int64) {
	if mi.t != nil {
		mi.t.ds.acc.addTransfer(mi.kind, origin, out, in)
		mi.t.noteDataChanged()
	}
	if mi.c != nil {
		d := mi.c.data()
		d.acc.addTransfer(mi.kind, origin, out, in)
		if mi.t == nil {
			mi.c.noteDataChanged()
		}
	}
}

// meteredBody counts the raw bytes of a response body and decodes it.
type meteredBody struct {
	rc     io.ReadCloser
	mi     *meterInfo
	origin string
	scheme string
	out    int64
	hdr    int64
	raw    atomic.Int64
	enc    string

	once sync.Once
	dec  io.Reader
	derr error
	done sync.Once
}

type rawCounter struct{ b *meteredBody }

func (r rawCounter) Read(p []byte) (int, error) {
	n, err := r.b.rc.Read(p)
	if n > 0 {
		r.b.raw.Add(int64(n))
		r.b.mi.rawBytes.Store(r.b.raw.Load())
	}
	return n, err
}

func (b *meteredBody) Read(p []byte) (int, error) {
	if b.enc == "" {
		return rawCounter{b}.Read(p)
	}
	b.once.Do(func() {
		switch b.enc {
		case "gzip":
			b.dec, b.derr = gzip.NewReader(rawCounter{b})
		case "br":
			b.dec, b.derr = brotli.NewReader(rawCounter{b}, nil)
		}
	})
	if b.derr != nil {
		return 0, b.derr
	}
	return b.dec.Read(p)
}

func (b *meteredBody) Close() error {
	err := b.rc.Close()
	b.done.Do(func() {
		b.mi.charge(b.scheme, b.origin, b.out, b.hdr+b.raw.Load())
	})
	return err
}

// ---- header size estimates ----------------------------------------------------

type countW struct{ n int64 }

func (c *countW) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// requestBytes estimates what the request puts on the wire as HTTP/1.1: the
// request line, the Host header, the other headers and the body.
func requestBytes(r *http.Request) int64 {
	var cw countW
	uri := r.URL.RequestURI()
	cw.n += int64(len(r.Method) + 1 + len(uri) + len(" HTTP/1.1\r\n"))
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	cw.n += int64(len("Host: ") + len(host) + 2)
	_ = r.Header.WriteSubset(&cw, map[string]bool{"Host": true, "Content-Length": true, "Transfer-Encoding": true})
	if r.ContentLength > 0 {
		cw.n += int64(len("Content-Length: ")+len(strconv.FormatInt(r.ContentLength, 10))+2) + r.ContentLength
	}
	cw.n += 2
	return cw.n
}

func responseHeaderBytes(res *http.Response) int64 {
	var cw countW
	st := res.Status
	if st == "" {
		st = strconv.Itoa(res.StatusCode) + " " + http.StatusText(res.StatusCode)
	}
	cw.n += int64(len("HTTP/1.1 ") + len(st) + 2)
	_ = res.Header.Write(&cw)
	cw.n += 2
	return cw.n
}

// withMeter attaches accounting to a request.
func withMeter(req *http.Request, c *BrowserContext, t *Tab, kind DataKind, pol DataPolicy) (*http.Request, *meterInfo) {
	mi := &meterInfo{c: c, t: t, kind: kind, pol: pol}
	mi.declared.Store(-1)
	return req.WithContext(context.WithValue(req.Context(), meterKey, mi)), mi
}

// CloseIdleConnections forwards to the wrapped transport. net/http's Client.CloseIdleConnections only
// reaches a transport that implements it, so without this the wrapper hid it and Engine.Close left
// keep-alive connections (and their goroutines) behind.
func (m *meterRT) CloseIdleConnections() {
	for _, rt := range []http.RoundTripper{m.base, m.capped} {
		if ci, ok := rt.(interface{ CloseIdleConnections() }); ok {
			ci.CloseIdleConnections()
		}
	}
}

// ---- WebSockets ---------------------------------------------------------------

// dialWebSocket opens a WebSocket for a context through the same counted dialer and the same
// proxy rules as the HTTP transport, so its bytes land in WireIn/WireOut and a proxy cannot be
// bypassed. ws and wss go through an HTTP proxy with CONNECT, as browsers do. A socks5 proxy is
// refused here (net/http supports it for HTTP, this small dialer does not).
func (e *Engine) dialWebSocket(ctx context.Context, c *BrowserContext, loc *url.URL, origin string, protocols []string, header http.Header) (*wsClient, *http.Response, error) {
	secure := loc.Scheme == "wss"
	port := loc.Port()
	if port == "" {
		port = "80"
		if secure {
			port = "443"
		}
	}
	target := net.JoinHostPort(loc.Hostname(), port)
	w := &c.data().wire
	dial := countedDial(w)

	probe := &http.Request{URL: &url.URL{Scheme: "http", Host: loc.Host}}
	if secure {
		probe.URL.Scheme = "https"
	}
	pu, perr := proxyFunc(e.proxyFor(c))(probe)
	if perr != nil {
		return nil, nil, perr
	}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var conn net.Conn
	var err error
	if pu == nil {
		conn, err = dial(dctx, "tcp", target)
		if err != nil {
			return nil, nil, err
		}
	} else {
		if pu.Scheme != "http" && pu.Scheme != "https" {
			return nil, nil, fmt.Errorf("websocket through a %s proxy is not supported", pu.Scheme)
		}
		paddr := pu.Host
		if pu.Port() == "" {
			if pu.Scheme == "https" {
				paddr = net.JoinHostPort(pu.Hostname(), "443")
			} else {
				paddr = net.JoinHostPort(pu.Hostname(), "80")
			}
		}
		conn, err = dial(dctx, "tcp", paddr)
		if err != nil {
			return nil, nil, err
		}
		if pu.Scheme == "https" {
			tc := tls.Client(conn, &tls.Config{ServerName: pu.Hostname()})
			if err = tc.HandshakeContext(dctx); err != nil {
				conn.Close()
				return nil, nil, err
			}
			conn = tc
		}
		req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
		if pu.User != nil {
			pw, _ := pu.User.Password()
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(pu.User.Username()+":"+pw)))
		}
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		if err = req.Write(conn); err != nil {
			conn.Close()
			return nil, nil, err
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, nil, fmt.Errorf("proxy refused CONNECT: %s", resp.Status)
		}
		if br.Buffered() > 0 { // nothing may follow a CONNECT 200 before we speak
			conn.Close()
			return nil, nil, errors.New("proxy sent data before the tunnel was used")
		}
	}
	if secure {
		tc := tls.Client(conn, &tls.Config{ServerName: loc.Hostname(), NextProtos: []string{"http/1.1"}})
		if err = tc.HandshakeContext(dctx); err != nil {
			conn.Close()
			return nil, nil, err
		}
		conn = tc
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	ws, resp, err := wsHandshake(conn, loc, origin, protocols, header)
	if err != nil {
		conn.Close()
		return nil, resp, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ws, resp, nil
}
