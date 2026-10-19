package engine

import (
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"time"
)

// The memory budget (Options.MaxRSSMiB, --max-rss-mib) is a soft limit on the
// process's resident memory. Two mechanisms hold it:
//
//   - SetProcessMemoryBudget gives the Go runtime a soft memory limit
//     (debug.SetMemoryLimit) at the budget, never below memLimitFloorMiB so a
//     small budget makes the collector work harder instead of thrashing.
//     A GOMEMLIMIT set by the user wins.
//   - the engine's governor samples the resident set every few seconds and,
//     above the budget, collects, returns freed memory to the OS and trims the
//     tabs' event rings. QuickJS heaps live outside the Go heap (each capped
//     by JSMemoryMiB); closing tabs or navigating away frees them.
//
// The budget is not a hard cap: one heavy page can take more while it is open.

// DefaultJSMemoryMiB is the default per-tab QuickJS heap cap (Options.JSMemoryMiB, --js-mem-mib). It is a
// safety net against runaway scripts, not a working-set budget: pages that parse large JSON payloads need
// a few hundred MiB (MDN's browser-compat tables peak near 340 MiB, YouTube's BotGuard needs more than
// 64), and a page that hits the cap renders "InternalError: out of memory" where its content should be.
// 1 GiB leaves about 3x headroom over the largest page measured and still stops an allocation loop in
// about three seconds (measured), long before the host runs out of memory. The soft process budget below does not
// bound it (QuickJS allocates outside the Go heap); the script timeout and this cap do.
const DefaultJSMemoryMiB = 1024

const (
	// DefaultMaxRSSMiB is the default soft budget: a daemon driving typical
	// pages one after another stays well under it (see README "Memory").
	DefaultMaxRSSMiB = 384
	memLimitFloorMiB = 192
	govInterval      = 2 * time.Second
	govFreeEvery     = 10 * time.Second // FreeOSMemory forces a full GC: not more often than this
)

var memLimitOnce sync.Once

// SetProcessMemoryBudget applies the budget to the Go runtime as a soft
// memory limit (once per process; the long-lived commands call it).
func SetProcessMemoryBudget(mib int) {
	memLimitOnce.Do(func() {
		if _, set := os.LookupEnv("GOMEMLIMIT"); set {
			return
		}
		if mib <= 0 {
			mib = DefaultMaxRSSMiB
		}
		debug.SetMemoryLimit(int64(max(mib, memLimitFloorMiB)) << 20)
	})
}

// processMiB is the resident set in MiB, or the Go runtime's own footprint
// where the OS figure is not available.
func processMiB() float64 {
	if rss, ok := ProcessRSS(); ok {
		return float64(rss) / (1 << 20)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.Sys-ms.HeapReleased) / (1 << 20)
}

// startMemGovernor runs the governor until the engine closes.
func (e *Engine) startMemGovernor() {
	go func() {
		tick := time.NewTicker(govInterval)
		defer tick.Stop()
		var lastFree time.Time
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-tick.C:
			}
			if !e.metrics.Sample() {
				continue
			}
			e.relieveMemory(&lastFree)
		}
	}()
}

// relieveMemory runs when the resident set is over budget.
func (e *Engine) relieveMemory(lastFree *time.Time) {
	for _, t := range e.ListTabs() {
		for _, r := range []*Ring{t.consoleRing, t.netRing, t.stateRing} {
			if r != nil {
				r.Trim()
			}
		}
	}
	if time.Since(*lastFree) >= govFreeEvery {
		*lastFree = time.Now()
		debug.FreeOSMemory() // collect and hand the freed spans back to the OS now
	}
}
