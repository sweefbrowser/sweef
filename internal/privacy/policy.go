package privacy

import (
	_ "embed"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/net/publicsuffix"
)

//go:embed seed_trackers.txt
var seedText string

var (
	seedOnce sync.Once
	seedList *Blocklist
)

// SeedList returns the embedded seed blocklist (compiled lazily on first
// lookup, so merely linking it costs nothing at start-up). It is seed data,
// not a complete filter list.
func SeedList() *Blocklist {
	seedOnce.Do(func() { seedList = FromText(seedText) })
	return seedList
}

// Settings is the privacy configuration (mirrors the stored settings).
type Settings struct {
	BlockTrackers          bool
	HTTPSUpgrade           bool
	BlockThirdPartyCookies bool
	SendGPC                bool
	TrimReferrer           bool
}

// DefaultSettings is what a fresh profile gets.
func DefaultSettings() Settings {
	return Settings{BlockTrackers: true, HTTPSUpgrade: true, BlockThirdPartyCookies: true, SendGPC: true, TrimReferrer: true}
}

// Engine decides per request. Exceptions is consulted with the top-level
// page's origin so users can turn protection off per site.
type Engine struct {
	List       *Blocklist
	S          atomic.Pointer[Settings]
	Exceptions func(topOrigin string) (noBlock bool)
	total      atomic.Int64
}

// NewEngine uses the seed list by default.
func NewEngine(s Settings) *Engine {
	e := &Engine{List: SeedList()}
	e.S.Store(&s)
	return e
}

// Update swaps the settings.
func (e *Engine) Update(s Settings) { e.S.Store(&s) }

// Settings returns the current settings.
func (e *Engine) Settings() Settings { return *e.S.Load() }

// Total is the number of requests blocked so far.
func (e *Engine) Total() int64 { return e.total.Load() }

// Decision is the verdict for one request.
type Decision struct {
	Block  bool
	Rule   string // matching blocklist domain
	Reason string // "tracker" | ""
}

// Check decides whether reqURL, requested by a page at topURL, is blocked.
// Only third-party requests are ever blocked, and never the top-level
// document itself.
func (e *Engine) Check(topURL, reqURL string) Decision {
	s := e.S.Load()
	if !s.BlockTrackers {
		return Decision{}
	}
	ru, err := url.Parse(reqURL)
	if err != nil || (ru.Scheme != "http" && ru.Scheme != "https" && ru.Scheme != "ws" && ru.Scheme != "wss") {
		return Decision{}
	}
	rh := ru.Hostname()
	if rh == "" {
		return Decision{}
	}
	if topURL != "" {
		tu, err := url.Parse(topURL)
		if err == nil && tu.Hostname() != "" {
			if SameSite(tu.Hostname(), rh) {
				return Decision{}
			}
			if e.Exceptions != nil && e.Exceptions(tu.Scheme+"://"+strings.ToLower(tu.Host)) {
				return Decision{}
			}
		}
	}
	if rule, ok := e.List.Match(rh); ok {
		e.total.Add(1)
		return Decision{Block: true, Rule: rule, Reason: "tracker"}
	}
	return Decision{}
}

// SameSite reports whether two hosts share a registrable domain
// (eTLD+1), falling back to exact match for IPs and unknown suffixes.
func SameSite(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == b {
		return true
	}
	if net.ParseIP(a) != nil || net.ParseIP(b) != nil {
		return false
	}
	ra, err1 := publicsuffix.EffectiveTLDPlusOne(a)
	rb, err2 := publicsuffix.EffectiveTLDPlusOne(b)
	return err1 == nil && err2 == nil && ra == rb
}

// IsLocalHost reports hosts that must never be upgraded or blocked.
func IsLocalHost(h string) bool {
	h = strings.ToLower(h)
	return h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") ||
		strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".test") || !strings.Contains(h, ".") || net.ParseIP(h) != nil
}

// Upgrade returns the https form of a plain http URL when HTTPS upgrade
// applies (not for localhost, IPs, single-label hosts or explicit ports
// other than 80). ok is false when the URL is unchanged.
func (e *Engine) Upgrade(raw string) (string, bool) {
	if !e.S.Load().HTTPSUpgrade {
		return raw, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || IsLocalHost(u.Hostname()) {
		return raw, false
	}
	if p := u.Port(); p != "" && p != "80" {
		return raw, false
	}
	u.Scheme = "https"
	u.Host = u.Hostname()
	return u.String(), true
}

// Headers returns request headers to add to every request.
func (e *Engine) Headers() map[string]string {
	if e.S.Load().SendGPC {
		return map[string]string{"Sec-GPC": "1", "DNT": "1"}
	}
	return nil
}

// TrimReferrer applies the strict-origin-when-cross-origin policy used by
// default in modern browsers: same-origin keeps the path, cross-origin sends
// only the origin, and https->http sends nothing. Credentials and fragments
// are always stripped. With trimming off, only credentials/fragments go.
func (e *Engine) TrimReferrer(referrer, target string) string {
	if referrer == "" {
		return ""
	}
	ru, err := url.Parse(referrer)
	if err != nil || ru.Host == "" {
		return ""
	}
	tu, err := url.Parse(target)
	if err != nil {
		return ""
	}
	ru.User, ru.Fragment = nil, ""
	if !e.S.Load().TrimReferrer {
		return ru.String()
	}
	if ru.Scheme == "https" && tu.Scheme == "http" {
		return ""
	}
	if ru.Scheme == tu.Scheme && strings.EqualFold(ru.Host, tu.Host) {
		return ru.String()
	}
	return ru.Scheme + "://" + ru.Host + "/"
}

// CookieOK reports whether a cookie set for cookieHost while the top-level
// page is topHost may be stored (third-party cookie blocking).
func (e *Engine) CookieOK(topHost, cookieHost string) bool {
	if !e.S.Load().BlockThirdPartyCookies {
		return true
	}
	cookieHost = strings.TrimPrefix(cookieHost, ".")
	return SameSite(topHost, cookieHost)
}

// Active reports whether third-party cookie blocking applies to a response
// from cookieHost while the top-level page is on topHost.
func (e *Engine) Active(topHost, cookieHost string) bool {
	return !e.CookieOK(topHost, cookieHost)
}
