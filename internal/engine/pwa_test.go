package engine

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a PWAClock the test moves by hand.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at   time.Time
	f    func()
	dead bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_900_000_000, 0)} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool { c.mu.Lock(); defer c.mu.Unlock(); was := !t.dead; t.dead = true; return was }
}

// Advance moves time forward and runs the timers that become due, earliest first.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		sort.SliceStable(c.timers, func(i, j int) bool { return c.timers[i].at.Before(c.timers[j].at) })
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.dead && !t.at.After(target) {
				next = t
				break
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		next.dead = true
		if next.at.After(c.now) {
			c.now = next.at
		}
		c.mu.Unlock()
		next.f()
	}
}

func (c *fakeClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.dead {
			n++
		}
	}
	return n
}

// pwaEnv is an engine whose default context is wired to a recording notification sink and a scripted user.
type pwaEnv struct {
	t     *testing.T
	e     *Engine
	ctx   *BrowserContext
	clock *fakeClock

	mu       sync.Mutex
	perms    map[string]string // origin|name -> state; absent = prompt
	answer   string            // what the user says when asked
	asked    []PermissionRequest
	opened   []string
	focused  []string
	badges   []string
	dnd      atomic.Bool
	offline  atomic.Bool
	openTabs []*Tab
}

func (p *pwaEnv) get(origin, name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.perms[origin+"|"+name]; ok {
		return s
	}
	return "prompt"
}

func (p *pwaEnv) set(origin, name, state string) {
	p.mu.Lock()
	p.perms[origin+"|"+name] = state
	p.mu.Unlock()
}

func (p *pwaEnv) askedN() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.asked) }

// newPWAEnv builds the engine on dir ("" = memory only) and configures its default context.
func newPWAEnv(t *testing.T, dir string, tweak func(*PWAOptions)) *pwaEnv {
	t.Helper()
	return newPWAEnvPre(t, dir, tweak, nil)
}

// newPWAEnvPre is newPWAEnv with a step that runs before the context is configured (the schedulers start then).
func newPWAEnvPre(t *testing.T, dir string, tweak func(*PWAOptions), pre func(*pwaEnv)) *pwaEnv {
	t.Helper()
	p := &pwaEnv{t: t, clock: newFakeClock(), perms: map[string]string{}, answer: "granted"}
	p.e = newStorageEngine(t, dir)
	p.ctx = p.e.DefaultContext()
	if pre != nil {
		pre(p)
	}
	o := PWAOptions{
		Permission: p.get,
		Request: func(ctx context.Context, r PermissionRequest) string {
			p.mu.Lock()
			p.asked = append(p.asked, r)
			ans := p.answer
			p.mu.Unlock()
			if ans == "granted" || ans == "denied" {
				p.set(r.Origin, r.Permission, ans)
			}
			return ans
		},
		OpenWindow: func(u string) (*Tab, error) {
			p.mu.Lock()
			p.opened = append(p.opened, u)
			p.mu.Unlock()
			tab, err := p.e.NewTab(u)
			if err == nil {
				p.mu.Lock()
				p.openTabs = append(p.openTabs, tab)
				p.mu.Unlock()
			}
			return tab, err
		},
		FocusTab: func(tab *Tab) {
			p.mu.Lock()
			p.focused = append(p.focused, tab.URL())
			p.mu.Unlock()
		},
		DoNotDisturb: p.dnd.Load,
		OnBadge: func(origin string, b Badge) {
			p.mu.Lock()
			p.badges = append(p.badges, origin+" "+badgeString(b))
			p.mu.Unlock()
		},
		Online:       func() bool { return !p.offline.Load() },
		Clock:        p.clock,
		EventTimeout: 10 * time.Second,
		Sync:         SyncPolicy{EventTimeout: 10 * time.Second},
	}
	if tweak != nil {
		tweak(&o)
	}
	p.ctx.SetPWA(o)
	return p
}

func badgeString(b Badge) string {
	switch {
	case !b.Set:
		return "none"
	case b.Count == 0:
		return "flag"
	}
	return itoa(int(b.Count))
}

func (p *pwaEnv) badgeLog() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.badges, "|")
}

func waitUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func originOf(u string) string { return siteOriginOf(u) }
