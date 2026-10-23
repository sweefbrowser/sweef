package engine

import (
	"time"
)

// Defaults for a screenshot's wait for visual stability (SettleVisual).
const (
	// ShotStableTimeout is the longest a screenshot waits for the page to stop changing.
	ShotStableTimeout = 2 * time.Second
	// ShotStableQuiet is how long the document must stay unchanged, style writes included, to count as still.
	ShotStableQuiet = 300 * time.Millisecond

	visualStep = 50 * time.Millisecond

	// A page that never goes quiet is still taken as settled once the same few elements have been animating on
	// their own for ambientWindow and nothing else has moved: a ticker, a spinner, a pulsing dot.
	ambientWindow   = 1200 * time.Millisecond
	ambientMaxNodes = 12
)

// styleStep is what one slice of SettleVisual saw: when, which elements had their style written, and whether
// anything other than style changed.
type styleStep struct {
	at      time.Time
	nodes   map[int64]struct{}
	content bool
}

// SettleVisual pumps the page's event loop until what it would paint has stopped changing, for at most timeout,
// and reports whether it did (false: still changing at the deadline, for example a page rewriting many elements).
//
// A headless load and the settle after an action stop as soon as the document's content stops changing
// (fnet.Doc.ContentGen), which deliberately ignores style attribute writes so that an endless animation does not
// hold every command for its whole budget. That is right for reading a page and wrong for photographing it: sites
// reveal their content with animation libraries that rewrite opacity and transform every frame, so a capture taken
// at the end of a settle shows the first frames of the intro (a blank page, faded headings). This waits on
// fnet.Doc.MutationGen instead, which counts style writes too, until it has been still for quiet.
//
// Marketing pages also carry animations that never end (a ticker, a spinner), which would keep that from ever
// going still. When, for ambientWindow, only a handful of elements have been written, all of them already written
// in the first half of the window, and nothing else changed, the page is taken as settled: what moves is the
// ambient animation, and the reveal has finished.
//
// A page without script returns at once: nothing moves in it (CSS animations and transitions are not run).
// timeout <= 0 disables the wait; quiet <= 0 means ShotStableQuiet.
func (t *Tab) SettleVisual(timeout, quiet time.Duration) (stable bool) {
	if timeout <= 0 {
		return true
	}
	if quiet <= 0 {
		quiet = ShotStableQuiet
	}
	if quiet > timeout {
		quiet = timeout
	}
	t.mu.RLock()
	j, d := t.js, t.dom
	t.mu.RUnlock()
	if j == nil || d == nil {
		return true
	}
	var (
		start      = time.Now()
		lastChange = start
		last       = d.MutationGen()
		lastC      = d.ContentGen()
		log, stop  = d.RecordStyleWrites()
		hist       []styleStep
	)
	defer func() { stop() }()
	for {
		sliceStart := time.Now()
		t.Settle(visualStep)
		t.mu.RLock()
		nd := t.dom
		t.mu.RUnlock()
		if nd == nil {
			return true
		}
		now := time.Now()
		if nd != d { // the page navigated: start over on the new document
			stop()
			d = nd
			log, stop = d.RecordStyleWrites()
			last, lastC, lastChange, hist = d.MutationGen(), d.ContentGen(), now, nil
		}
		if g := d.MutationGen(); g != last {
			last, lastChange = g, now
			c := d.ContentGen()
			hist = append(hist, styleStep{at: now, nodes: log.Take(), content: c != lastC})
			lastC = c
			// keep two windows' worth: older steps cannot matter again
			for len(hist) > 0 && now.Sub(hist[0].at) > 2*ambientWindow {
				hist = hist[1:]
			}
		}
		if now.Sub(lastChange) >= quiet {
			return true
		}
		if now.Sub(start) >= ambientWindow+quiet && ambientOnly(hist, now) {
			return true
		}
		if now.Sub(start) >= timeout {
			return false
		}
		// the pump returns early when nothing is due; real time must still pass for timers and frames
		if rest := visualStep - now.Sub(sliceStart); rest > 0 {
			time.Sleep(rest)
		}
	}
}

// ambientOnly reports whether, over the last ambientWindow, nothing but style changed, at most ambientMaxNodes
// elements were written, and every element written in the second half was already being written in the first:
// no new element started moving, so what is left is an animation that goes on for ever.
func ambientOnly(hist []styleStep, now time.Time) bool {
	older, newer := map[int64]struct{}{}, map[int64]struct{}{}
	mid, from := now.Add(-ambientWindow/2), now.Add(-ambientWindow)
	var sawOlder bool
	for _, s := range hist {
		if s.at.Before(from) {
			continue
		}
		if s.content {
			return false
		}
		dst := older
		if s.at.After(mid) {
			dst = newer
		} else {
			sawOlder = true
		}
		for id := range s.nodes {
			dst[id] = struct{}{}
		}
	}
	if !sawOlder || len(newer) == 0 || len(newer) > ambientMaxNodes {
		return false
	}
	for id := range newer {
		if _, ok := older[id]; !ok {
			return false
		}
	}
	return true
}
