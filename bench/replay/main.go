// Command replay records real pages with their subresources and serves them again from localhost, so page-load
// numbers can be compared without the noise of the live network (DNS, TLS, server and CDN latency, A/B tests,
// content that changes between runs).
//
// It is an HTTP proxy that intercepts TLS with its own CA (written to <dir>/ca.pem on first use):
//
//	go run ./bench/replay -dir bench/replay/data -record -addr 127.0.0.1:8899   # forwards and stores
//	go run ./bench/replay -dir bench/replay/data -addr 127.0.0.1:8899           # answers from the store
//
// Point a browser at it: Sweef with HTTPS_PROXY=http://127.0.0.1:8899 SSL_CERT_FILE=<dir>/ca.pem, Chromium with
// --proxy-server=http://127.0.0.1:8899 --ignore-certificate-errors. Recording forwards to the network through
// $HTTPS_PROXY when set. A request the store does not have is matched, in order, by method and URL, then by
// method and URL without its query (cache busters, timestamps); anything else gets 404. -rtt adds a fixed delay
// to every answer (a stand-in for network latency; 0 = loopback speed).
package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	dir    = flag.String("dir", "bench/replay/data", "store directory")
	addr   = flag.String("addr", "127.0.0.1:8899", "listen address")
	record = flag.Bool("record", false, "forward to the network and store the answers (default: replay only)")
	rtt    = flag.Duration("rtt", 0, "fixed delay added to every replayed answer")
	verb   = flag.Bool("v", false, "log every request")
)

type entry struct {
	Method string      `json:"method"`
	URL    string      `json:"url"`
	Status int         `json:"status"`
	Header http.Header `json:"header"`
	Body   string      `json:"body"` // file name of the body under <dir>/bodies
}

type store struct {
	mu      sync.Mutex
	byKey   map[string]*entry
	byNoQry map[string]*entry
	hits    map[string]int
	misses  []string
}

func key(method, u string) string { return method + " " + u }

func noQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

func (s *store) load() {
	s.byKey, s.byNoQry, s.hits = map[string]*entry{}, map[string]*entry{}, map[string]int{}
	f, err := os.Open(filepath.Join(*dir, "index.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			s.put(&e)
		}
	}
}

func (s *store) put(e *entry) {
	s.byKey[key(e.Method, e.URL)] = e
	s.byNoQry[key(e.Method, noQuery(e.URL))] = e
}

func (s *store) find(method, u string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.byKey[key(method, u)]; e != nil {
		s.hits["exact"]++
		return e
	}
	if e := s.byNoQry[key(method, noQuery(u))]; e != nil {
		s.hits["path"]++
		return e
	}
	s.misses = append(s.misses, key(method, u))
	return nil
}

func (s *store) save(e *entry, body []byte) error {
	sum := sha256.Sum256(body)
	name := hex.EncodeToString(sum[:16])
	p := filepath.Join(*dir, "bodies", name)
	if _, err := os.Stat(p); err != nil {
		if err := os.WriteFile(p, body, 0o644); err != nil {
			return err
		}
	}
	e.Body = name
	b, _ := json.Marshal(e)
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.byKey[key(e.Method, e.URL)]; old != nil && old.Status < 400 && e.Status >= 400 {
		return nil // keep a good answer over a later failure
	}
	f, err := os.OpenFile(filepath.Join(*dir, "index.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	s.put(e)
	return nil
}

// ---- CA and leaf certificates ---------------------------------------------------------

type certs struct {
	ca    *x509.Certificate
	key   *ecdsa.PrivateKey
	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

func loadCA() *certs {
	c := &certs{leafs: map[string]*tls.Certificate{}}
	cp, kp := filepath.Join(*dir, "ca.pem"), filepath.Join(*dir, "ca-key.pem")
	if cb, err := os.ReadFile(cp); err == nil {
		kb, err := os.ReadFile(kp)
		if err != nil {
			log.Fatal(err)
		}
		b, _ := pem.Decode(cb)
		c.ca, err = x509.ParseCertificate(b.Bytes)
		if err != nil {
			log.Fatal(err)
		}
		k, _ := pem.Decode(kb)
		c.key, err = x509.ParseECPrivateKey(k.Bytes)
		if err != nil {
			log.Fatal(err)
		}
		return c
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sweef bench replay CA"},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		log.Fatal(err)
	}
	c.ca, _ = x509.ParseCertificate(der)
	c.key = k
	kd, _ := x509.MarshalECPrivateKey(k)
	_ = os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	_ = os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600)
	return c
}

func (c *certs) leaf(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.leafs[host]; l != nil {
		return l, nil
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sn, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: host},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tpl.IPAddresses = []net.IP{ip}
	} else {
		tpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.ca, &k.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	l := &tls.Certificate{Certificate: [][]byte{der, c.ca.Raw}, PrivateKey: k}
	c.leafs[host] = l
	return l, nil
}

// ---- proxy ------------------------------------------------------------------------------

type proxy struct {
	st  *store
	ca  *certs
	up  *http.Transport
	ok  sync.Map
	ln  net.Listener
	srv *http.Server
}

// hop-by-hop headers are not forwarded or stored
var hop = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL.IsAbs() {
		p.answer(w, r, r.URL.String())
		return
	}
	if r.URL.Path == "/__replay/stats" {
		p.st.mu.Lock()
		defer p.st.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": len(p.st.byKey), "hits": p.st.hits, "misses": p.st.misses})
		return
	}
	http.Error(w, "replay proxy: absolute URL or CONNECT expected", http.StatusBadRequest)
}

func (p *proxy) connect(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()
	if host == "" {
		host, _, _ = net.SplitHostPort(r.Host)
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	tc := tls.Server(conn, &tls.Config{
		NextProtos: []string{"h2", "http/1.1"}, // the real hosts of the recorded pages speak HTTP/2: one multiplexed connection, not six
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			n := h.ServerName
			if n == "" {
				n = host
			}
			return p.ca.leaf(n)
		},
	})
	if err := tc.Handshake(); err != nil {
		conn.Close()
		return
	}
	authority := r.Host
	if !strings.Contains(authority, ":") {
		authority += ":443"
	}
	authority = strings.TrimSuffix(authority, ":443")
	ln := &oneConnListener{c: tc, done: make(chan struct{})}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
		p.answer(w, rr, "https://"+authority+rr.URL.RequestURI())
	}), ReadHeaderTimeout: 30 * time.Second,
		// the *tls.Conn goes to the server as it is (a wrapper would hide it from the HTTP/2 upgrade), so the end
		// of the connection is learned from its state
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateClosed {
				ln.once.Do(func() { close(ln.done) })
			}
		}}
	go func() { _ = srv.Serve(ln) }()
	<-ln.done
}

// oneConnListener serves the requests of one intercepted connection.
type oneConnListener struct {
	c    net.Conn
	once sync.Once
	done chan struct{}
	used bool
	mu   sync.Mutex
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used {
		<-l.done
		return nil, io.EOF
	}
	l.used = true
	return l.c, nil
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

func (p *proxy) answer(w http.ResponseWriter, r *http.Request, u string) {
	var reqBody []byte
	if r.Body != nil {
		reqBody, _ = io.ReadAll(r.Body)
	}
	if *verb {
		log.Println(r.Method, u)
	}
	if !*record {
		e := p.st.find(r.Method, u)
		if e == nil {
			http.Error(w, "not recorded", http.StatusNotFound)
			return
		}
		body, err := os.ReadFile(filepath.Join(*dir, "bodies", e.Body))
		if err != nil {
			http.Error(w, "body missing", http.StatusBadGateway)
			return
		}
		if *rtt > 0 {
			time.Sleep(*rtt)
		}
		for k, vs := range e.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(e.Status)
		_, _ = w.Write(body)
		return
	}
	req, err := http.NewRequest(r.Method, u, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Header = r.Header.Clone()
	for _, h := range hop {
		req.Header.Del(h)
	}
	req.Header.Del("Accept-Encoding") // store identity bodies; the transport negotiates gzip and decodes it
	resp, err := p.up.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	h := resp.Header.Clone()
	for _, k := range hop {
		h.Del(k)
	}
	h.Del("Content-Encoding")
	h.Del("Content-Length")
	h.Del("Alt-Svc")
	h.Del("Strict-Transport-Security")
	if resp.StatusCode != http.StatusNotModified && resp.StatusCode != http.StatusPartialContent {
		if err := p.st.save(&entry{Method: r.Method, URL: u, Status: resp.StatusCode, Header: h}, body); err != nil {
			log.Println("save:", err)
		}
	}
	for k, vs := range h {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func main() {
	flag.Parse()
	if err := os.MkdirAll(filepath.Join(*dir, "bodies"), 0o755); err != nil {
		log.Fatal(err)
	}
	st := &store{}
	st.load()
	up := &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 16, ForceAttemptHTTP2: true,
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	if u := os.Getenv("REPLAY_UPSTREAM_PROXY"); u != "" {
		pu, err := url.Parse(u)
		if err != nil {
			log.Fatal(err)
		}
		up.Proxy = http.ProxyURL(pu)
	}
	p := &proxy{st: st, ca: loadCA(), up: up}
	mode := "replay"
	if *record {
		mode = "record"
	}
	log.Printf("%s proxy on %s, %d entries, CA %s", mode, *addr, len(st.byKey), filepath.Join(*dir, "ca.pem"))
	log.Fatal(http.ListenAndServe(*addr, p))
}
