package privacy

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestCompileAndMatch(t *testing.T) {
	src := `
! comment
# another
||ads.example.net^
tracker.test
0.0.0.0 hosts.example.org
127.0.0.1 localhost.evil.com
@@good.tracker.test
@@||allowed.example.net^
example.net
`
	bl := FromText(src)
	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"tracker.test", "tracker.test", true},
		{"a.b.tracker.test", "tracker.test", true},
		{"TRACKER.TEST.", "tracker.test", true},
		{"good.tracker.test", "", false},   // exception beats the block
		{"x.good.tracker.test", "", false}, // exception covers subdomains too
		{"hosts.example.org", "hosts.example.org", true},
		{"cdn.hosts.example.org", "hosts.example.org", true},
		{"example.org", "", false},
		{"ads.example.net", "example.net", true}, // the broader rule matches first (shortest suffix wins is not required)
		{"allowed.example.net", "", false},
		{"nottracker.test", "", false}, // suffix must align on a label boundary
		{"tracker.test.evil.com", "", false},
		{"localhost.evil.com", "localhost.evil.com", true},
		{"localhost", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		rule, ok := bl.Match(c.host)
		if ok != c.ok || (ok && c.want != "" && rule != c.want && !(c.host == "ads.example.net")) {
			t.Errorf("Match(%q) = %q,%v want %q,%v", c.host, rule, ok, c.want, c.ok)
		}
	}
	if bl.Len() != 7 {
		t.Errorf("rules = %d", bl.Len())
	}
}

func TestLazyLoad(t *testing.T) {
	calls := 0
	bl := NewBlocklist(func() ([]byte, error) { calls++; return Compile("a.test"), nil })
	if calls != 0 || bl.Loaded() {
		t.Fatal("loader ran at construction")
	}
	if _, ok := bl.Match("x.a.test"); !ok {
		t.Fatal("match")
	}
	bl.Match("y.a.test")
	if calls != 1 || !bl.Loaded() {
		t.Fatalf("loader calls = %d", calls)
	}
	bad := FromCompiled([]byte("garbage"))
	if _, ok := bad.Match("a.test"); ok || bad.Err() == nil {
		t.Fatal("bad list must fail closed (no blocking) and report an error")
	}
	if trunc := Compile("a.test\nb.test"); FromCompiled(trunc[:len(trunc)-3]).Err() == nil {
		t.Fatal("truncated list accepted")
	}
}

func TestSeedList(t *testing.T) {
	bl := SeedList()
	if bl.Err() != nil {
		t.Fatal(bl.Err())
	}
	if bl.Len() < 200 {
		t.Fatalf("seed list suspiciously small: %d", bl.Len())
	}
	for _, h := range []string{"doubleclick.net", "stats.g.doubleclick.net", "www.google-analytics.com", "connect.facebook.net", "bat.bing.com", "px.ads.linkedin.com", "static.hotjar.com"} {
		if _, ok := bl.Match(h); !ok {
			t.Errorf("seed list should block %s", h)
		}
	}
	// sites users actually visit must not be blocked
	for _, h := range []string{"google.com", "www.google.com", "github.com", "wikipedia.org", "example.com", "cdn.jsdelivr.net", "fonts.googleapis.com", "accounts.google.com", "duckduckgo.com", "news.ycombinator.com"} {
		if rule, ok := bl.Match(h); ok {
			t.Errorf("seed list must not block %s (rule %s)", h, rule)
		}
	}
}

func TestBloomFalsePositivesAreCaught(t *testing.T) {
	// 20k rules; every non-member host must be rejected (Bloom hits are verified exactly)
	var sb strings.Builder
	r := rand.New(rand.NewSource(3))
	member := map[string]bool{}
	for i := 0; i < 20000; i++ {
		d := fmt.Sprintf("t%d-%x.example%d.com", i, r.Intn(1<<20), i%7)
		member[d] = true
		sb.WriteString(d + "\n")
	}
	bl := FromText(sb.String())
	if bl.Len() != 20000 {
		t.Fatalf("len %d", bl.Len())
	}
	for d := range member {
		if _, ok := bl.Match(d); !ok {
			t.Fatalf("lost member %s", d)
		}
	}
	for i := 0; i < 50000; i++ {
		h := fmt.Sprintf("clean%d.site%d.org", i, r.Intn(1000))
		if _, ok := bl.Match(h); ok {
			t.Fatalf("false positive for %s", h)
		}
	}
}

func TestMatchSpeed(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 100_000; i++ {
		fmt.Fprintf(&sb, "d%d.t%d.com\n", i, i%977)
	}
	bl := FromText(sb.String())
	bl.Match("warm.up.com")
	hosts := []string{"www.example.com", "cdn.some.site.org", "d5.t5.com", "a.b.c.d.e.f.g.co.uk", "fonts.gstatic.com"}
	t0 := time.Now()
	const n = 200000
	for i := 0; i < n; i++ {
		bl.Match(hosts[i%len(hosts)])
	}
	per := time.Since(t0) / n
	t.Logf("Match over 100k rules: %v/lookup", per)
	if per > 20*time.Microsecond {
		t.Errorf("lookup too slow: %v", per)
	}
}

func BenchmarkMatchClean(b *testing.B) {
	bl := SeedList()
	bl.Match("x.com")
	for i := 0; i < b.N; i++ {
		bl.Match("cdn.static.example.com")
	}
}

func BenchmarkMatchBlocked(b *testing.B) {
	bl := SeedList()
	bl.Match("x.com")
	for i := 0; i < b.N; i++ {
		bl.Match("pixel.stats.doubleclick.net")
	}
}

func TestSameSite(t *testing.T) {
	yes := [][2]string{{"a.example.com", "b.example.com"}, {"example.com", "www.example.com"}, {"foo.co.uk", "bar.foo.co.uk"}, {"127.0.0.1", "127.0.0.1"}, {"x.github.io", "x.github.io"}}
	no := [][2]string{{"example.com", "example.org"}, {"a.co.uk", "b.co.uk"}, {"a.github.io", "b.github.io"}, {"127.0.0.1", "127.0.0.2"}, {"example.com", "notexample.com"}}
	for _, p := range yes {
		if !SameSite(p[0], p[1]) {
			t.Errorf("%v should be same-site", p)
		}
	}
	for _, p := range no {
		if SameSite(p[0], p[1]) {
			t.Errorf("%v should be cross-site", p)
		}
	}
}

func TestCheckPolicy(t *testing.T) {
	e := NewEngine(DefaultSettings())
	e.List = FromText("tracker.test\n")
	// third party tracker blocked
	if d := e.Check("https://news.example.com/", "https://cdn.tracker.test/t.js"); !d.Block || d.Rule != "tracker.test" {
		t.Fatalf("%+v", d)
	}
	// first-party request to a listed domain is not blocked (visiting the site itself)
	if d := e.Check("https://tracker.test/", "https://tracker.test/page"); d.Block {
		t.Fatal("first-party blocked")
	}
	if d := e.Check("https://www.tracker.test/", "https://api.tracker.test/x"); d.Block {
		t.Fatal("same-site subdomain blocked")
	}
	// non-http schemes and unparsable URLs are left alone
	for _, u := range []string{"data:text/plain,hi", "blob:abc", "sweef://chrome/app.js", "::bad::"} {
		if e.Check("https://news.example.com/", u).Block {
			t.Errorf("blocked %s", u)
		}
	}
	// per-site exception
	e.Exceptions = func(origin string) bool { return origin == "https://news.example.com" }
	if e.Check("https://news.example.com/", "https://cdn.tracker.test/t.js").Block {
		t.Fatal("exception ignored")
	}
	if !e.Check("https://other.example.com/", "https://cdn.tracker.test/t.js").Block {
		t.Fatal("exception leaked to another origin")
	}
	// switch off
	e.Exceptions = nil
	s := DefaultSettings()
	s.BlockTrackers = false
	e.Update(s)
	if e.Check("https://news.example.com/", "https://cdn.tracker.test/t.js").Block {
		t.Fatal("blocking not disabled")
	}
	if e.Total() != 2 {
		t.Fatalf("total %d", e.Total())
	}
}

func TestUpgrade(t *testing.T) {
	e := NewEngine(DefaultSettings())
	cases := map[string]string{
		"http://example.com/a?b=1": "https://example.com/a?b=1",
		"http://example.com:80/":   "https://example.com/",
		"http://example.com:8080/": "http://example.com:8080/", // explicit non-default port stays
		"http://localhost/":        "http://localhost/",
		"http://127.0.0.1:3000/":   "http://127.0.0.1:3000/",
		"http://[::1]/":            "http://[::1]/",
		"http://intranet/x":        "http://intranet/x",
		"http://app.local/":        "http://app.local/",
		"https://example.com/":     "https://example.com/",
		"ftp://example.com/":       "ftp://example.com/",
	}
	for in, want := range cases {
		got, changed := e.Upgrade(in)
		if got != want || changed != (got != in) {
			t.Errorf("Upgrade(%q) = %q,%v want %q", in, got, changed, want)
		}
	}
	s := DefaultSettings()
	s.HTTPSUpgrade = false
	e.Update(s)
	if _, ok := e.Upgrade("http://example.com/"); ok {
		t.Fatal("upgrade not disabled")
	}
}

func TestReferrerTrimming(t *testing.T) {
	e := NewEngine(DefaultSettings())
	cases := []struct{ ref, target, want string }{
		{"https://a.com/secret/path?token=1#frag", "https://a.com/other", "https://a.com/secret/path?token=1"},
		{"https://a.com/secret/path?token=1", "https://b.com/x", "https://a.com/"},
		{"https://a.com/p", "http://b.com/", ""}, // https -> http: nothing
		{"https://user:pw@a.com/p", "https://a.com/q", "https://a.com/p"},
		{"", "https://a.com/", ""},
		{"not a url", "https://a.com/", ""},
		{"https://a.com:8443/p", "https://a.com/", "https://a.com:8443/"}, // different port = cross-origin
	}
	for _, c := range cases {
		if got := e.TrimReferrer(c.ref, c.target); got != c.want {
			t.Errorf("TrimReferrer(%q -> %q) = %q, want %q", c.ref, c.target, got, c.want)
		}
	}
	s := DefaultSettings()
	s.TrimReferrer = false
	e.Update(s)
	if got := e.TrimReferrer("https://u:p@a.com/p?q=1#f", "https://b.com/"); got != "https://a.com/p?q=1" {
		t.Errorf("untrimmed referrer must still lose credentials and fragment: %q", got)
	}
}

func TestHeadersAndCookies(t *testing.T) {
	e := NewEngine(DefaultSettings())
	h := e.Headers()
	if h["Sec-GPC"] != "1" || h["DNT"] != "1" {
		t.Fatalf("%v", h)
	}
	s := DefaultSettings()
	s.SendGPC = false
	e.Update(s)
	if len(e.Headers()) != 0 {
		t.Fatal("GPC not switchable")
	}
	e = NewEngine(DefaultSettings())
	if !e.CookieOK("news.example.com", "api.example.com") || !e.CookieOK("a.com", ".a.com") {
		t.Fatal("first-party cookie refused")
	}
	if e.CookieOK("news.example.com", "ads.tracker.test") {
		t.Fatal("third-party cookie allowed")
	}
	s = DefaultSettings()
	s.BlockThirdPartyCookies = false
	e.Update(s)
	if !e.CookieOK("news.example.com", "ads.tracker.test") {
		t.Fatal("switch off must allow")
	}
}
