package engine

// pwa_sync.go: Background Sync and Periodic Background Sync.
//
// Background Sync: registration.sync.register(tag) records a one-shot task. While the browser runs and is online
// the registration's worker receives a `sync` event for it. A task whose event fails is retried with exponential
// backoff (SyncPolicy: 3 attempts, the wait after the first failure 5 minutes, growing by a factor of 3, as in
// Chromium); the last attempt has event.lastChance set, and after it the task is dropped. A tag registered again
// while its event runs fires once more afterwards.
//
// Periodic Background Sync: registration.periodicSync.register(tag, {minInterval}) fires `periodicsync` no more
// often than the larger of minInterval and the platform floor (12 hours), and only for origins whose
// "periodic-background-sync" permission is granted (the app grants it to bookmarked or installed sites; the
// default is denied). Nothing fires while the browser is not running; registrations are persisted and resume at
// the next start.

import (
	"math"
	"time"
)

type syncReg struct {
	Scope    string `json:"scope"`
	Tag      string `json:"tag"`
	Attempts int    `json:"attempts"`
	NextAt   int64  `json:"nextAt"` // unix milliseconds, not before

	origin  string
	firing  bool
	again   bool // registered again while firing
	waiting bool // due, but the browser is offline
	stop    func() bool
	dead    bool
}

type periodicReg struct {
	Scope       string `json:"scope"`
	Tag         string `json:"tag"`
	MinInterval int64  `json:"minInterval"` // milliseconds, as requested
	LastFired   int64  `json:"lastFired"`   // unix milliseconds; the registration time until the first event
	origin      string
	waiting     bool
	stop        func() bool
	dead        bool
}

func unixMS(t time.Time) int64 { return t.UnixNano() / int64(time.Millisecond) }

func (st *pwaState) startSync() {
	st.goJob(func() {
		for _, o := range st.allOrigins() {
			st.originData(o) // loading arms the registrations (armLocked)
		}
	})
}

// armLocked schedules the registrations just read from disk. Before SetPWA there is no clock to arm them with;
// SetPWA runs startSync, whose scan finds them then.
func (st *pwaState) armLocked(origin string, d *pwaOrigin) {
	if !st.enabled.Load() {
		delete(st.data, origin)
		return
	}
	for _, r := range d.Sync {
		r.origin = origin
		st.scheduleSyncLocked(r, time.Duration(r.NextAt-unixMS(st.clock.Now()))*time.Millisecond)
	}
	for _, r := range d.Periodic {
		r.origin = origin
		st.schedulePeriodicLocked(r)
	}
}

func (st *pwaState) stopSync() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, d := range st.data {
		for _, r := range d.Sync {
			if r.stop != nil {
				r.stop()
			}
		}
		for _, r := range d.Periodic {
			if r.stop != nil {
				r.stop()
			}
		}
	}
}

// scheduleSyncLocked arms a registration to fire after d (immediately when d <= 0).
func (st *pwaState) scheduleSyncLocked(r *syncReg, d time.Duration) {
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
	if d <= 0 {
		st.goJobLocked(func() { st.fireSync(r) })
		return
	}
	r.stop = st.clock.AfterFunc(d, func() { st.goJob(func() { st.fireSync(r) }) })
}

// goJobLocked is goJob for callers that hold st.mu.
func (st *pwaState) goJobLocked(f func()) {
	if st.closed {
		return
	}
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()
		f()
	}()
}

func (st *pwaState) retryDelay(attempts int) time.Duration {
	return time.Duration(float64(st.pol.InitialDelay) * math.Pow(st.pol.DelayFactor, float64(attempts-1)))
}

func (st *pwaState) findReg(origin, scope string) *swReg {
	m := st.c.sw()
	m.ensure(origin)
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.regs[regKey(origin, scope)]; r != nil && !r.unregistered {
		return r
	}
	return nil
}

func (st *pwaState) fireSync(r *syncReg) {
	st.mu.Lock()
	if r.dead || r.firing || st.closed {
		st.mu.Unlock()
		return
	}
	if !st.isOnline() {
		r.waiting = true
		st.mu.Unlock()
		return
	}
	r.waiting, r.firing = false, true
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
	last := r.Attempts+1 >= st.pol.MaxAttempts
	started := st.clock.Now() // waits are counted from when the attempt began, not from when it ended
	origin, scope, tag := r.origin, r.Scope, r.Tag
	st.mu.Unlock()

	var err error
	if reg := st.findReg(origin, scope); reg == nil {
		err = errNoWorker
	} else {
		_, err = st.c.sw().dispatchReg(reg, "sync", map[string]any{"tag": tag, "lastChance": last}, st.pol.EventTimeout)
	}

	st.mu.Lock()
	r.firing = false
	again := r.again
	r.again = false
	d := st.originData0(origin)
	switch {
	case err == errNoWorker:
		st.removeSyncLocked(d, r)
	case err == nil:
		st.removeSyncLocked(d, r)
		if again {
			st.addSyncLocked(origin, d, scope, tag)
		}
	default:
		r.Attempts++
		if r.Attempts >= st.pol.MaxAttempts {
			st.removeSyncLocked(d, r)
			if again {
				st.addSyncLocked(origin, d, scope, tag)
			}
		} else {
			due := started.Add(st.retryDelay(r.Attempts))
			delay := due.Sub(st.clock.Now())
			r.NextAt = unixMS(due)
			if again {
				r.Attempts, r.NextAt = 0, unixMS(st.clock.Now())
				delay = 0
			}
			st.scheduleSyncLocked(r, delay)
			st.saveOriginLocked(origin)
		}
	}
	st.mu.Unlock()
}

type pwaErr string

func (e pwaErr) Error() string { return string(e) }

const errNoWorker = pwaErr("no active worker")

// originData0 returns the cached data of an origin that is known to be loaded (st.mu held).
func (st *pwaState) originData0(origin string) *pwaOrigin {
	d := st.data[origin]
	if d == nil {
		d = &pwaOrigin{}
		st.data[origin] = d
	}
	return d
}

func (st *pwaState) removeSyncLocked(d *pwaOrigin, r *syncReg) {
	r.dead = true
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
	out := d.Sync[:0]
	for _, o := range d.Sync {
		if o != r {
			out = append(out, o)
		}
	}
	d.Sync = out
	st.saveOriginLocked(r.origin)
}

func (st *pwaState) addSyncLocked(origin string, d *pwaOrigin, scope, tag string) *syncReg {
	r := &syncReg{Scope: scope, Tag: tag, NextAt: unixMS(st.clock.Now()), origin: origin}
	d.Sync = append(d.Sync, r)
	st.saveOriginLocked(origin)
	st.scheduleSyncLocked(r, 0)
	return r
}

func (st *pwaState) registerSync(origin string, reg *swReg, tag string) {
	d := st.originData(origin)
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, r := range d.Sync {
		if r.Scope == reg.scope && r.Tag == tag && !r.dead {
			if r.firing {
				r.again = true
			}
			return
		}
	}
	st.addSyncLocked(origin, d, reg.scope, tag)
}

// syncOnline fires what waited for connectivity.
func (st *pwaState) syncOnline() {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := unixMS(st.clock.Now())
	for _, d := range st.data {
		for _, r := range d.Sync {
			if r.waiting && !r.dead {
				r.waiting = false
				st.scheduleSyncLocked(r, time.Duration(r.NextAt-now)*time.Millisecond)
			}
		}
		for _, r := range d.Periodic {
			if r.waiting && !r.dead {
				r.waiting = false
				st.schedulePeriodicLocked(r)
			}
		}
	}
}

func (st *pwaState) syncTags(origin string, reg *swReg) []string {
	d := st.originData(origin)
	st.mu.Lock()
	defer st.mu.Unlock()
	out := []string{}
	for _, r := range d.Sync {
		if r.Scope == reg.scope && !r.dead {
			out = append(out, r.Tag)
		}
	}
	return out
}

// ---- periodic ----------------------------------------------------------------------------------------------------------

func (st *pwaState) periodicInterval(r *periodicReg) time.Duration {
	d := time.Duration(r.MinInterval) * time.Millisecond
	if d < st.pol.PeriodicFloor {
		d = st.pol.PeriodicFloor
	}
	return d
}

func (st *pwaState) schedulePeriodicLocked(r *periodicReg) {
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
	due := time.UnixMilli(r.LastFired).Add(st.periodicInterval(r))
	d := due.Sub(st.clock.Now())
	if d <= 0 {
		st.goJobLocked(func() { st.firePeriodic(r) })
		return
	}
	r.stop = st.clock.AfterFunc(d, func() { st.goJob(func() { st.firePeriodic(r) }) })
}

func (st *pwaState) firePeriodic(r *periodicReg) {
	st.mu.Lock()
	if r.dead || st.closed {
		st.mu.Unlock()
		return
	}
	if !st.isOnline() {
		r.waiting = true
		st.mu.Unlock()
		return
	}
	origin, scope, tag := r.origin, r.Scope, r.Tag
	started := st.clock.Now()
	st.mu.Unlock()

	if st.permission(origin, PermPeriodicSync) == "granted" {
		if reg := st.findReg(origin, scope); reg != nil {
			_, _ = st.c.sw().dispatchReg(reg, "periodicsync", map[string]any{"tag": tag}, st.pol.EventTimeout)
		} else {
			st.mu.Lock()
			st.removePeriodicLocked(st.originData0(origin), r)
			st.mu.Unlock()
			return
		}
	}
	st.mu.Lock()
	if !r.dead {
		r.LastFired = unixMS(started) // a failing or skipped event waits a full interval, too
		st.saveOriginLocked(origin)
		st.schedulePeriodicLocked(r)
	}
	st.mu.Unlock()
}

func (st *pwaState) removePeriodicLocked(d *pwaOrigin, r *periodicReg) {
	r.dead = true
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
	out := d.Periodic[:0]
	for _, o := range d.Periodic {
		if o != r {
			out = append(out, o)
		}
	}
	d.Periodic = out
	st.saveOriginLocked(r.origin)
}

func (st *pwaState) registerPeriodic(origin string, reg *swReg, tag string, minInterval int64) {
	d := st.originData(origin)
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, r := range d.Periodic {
		if r.Scope == reg.scope && r.Tag == tag && !r.dead {
			r.MinInterval = minInterval
			st.saveOriginLocked(origin)
			st.schedulePeriodicLocked(r)
			return
		}
	}
	r := &periodicReg{Scope: reg.scope, Tag: tag, MinInterval: minInterval, LastFired: unixMS(st.clock.Now()), origin: origin}
	d.Periodic = append(d.Periodic, r)
	st.saveOriginLocked(origin)
	st.schedulePeriodicLocked(r)
}

func (st *pwaState) unregisterPeriodic(origin string, reg *swReg, tag string) {
	d := st.originData(origin)
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, r := range d.Periodic {
		if r.Scope == reg.scope && r.Tag == tag {
			st.removePeriodicLocked(d, r)
			return
		}
	}
}

func (st *pwaState) periodicTags(origin string, reg *swReg) []string {
	d := st.originData(origin)
	st.mu.Lock()
	defer st.mu.Unlock()
	out := []string{}
	for _, r := range d.Periodic {
		if r.Scope == reg.scope && !r.dead {
			out = append(out, r.Tag)
		}
	}
	return out
}

func (st *pwaState) clearSync(origin string) {
	d := st.originData(origin)
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, r := range append([]*syncReg(nil), d.Sync...) {
		st.removeSyncLocked(d, r)
	}
	for _, r := range append([]*periodicReg(nil), d.Periodic...) {
		st.removePeriodicLocked(d, r)
	}
}

func (st *pwaState) dropSyncOf(origin, scope string) {
	d := st.originData(origin)
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, r := range append([]*syncReg(nil), d.Sync...) {
		if r.Scope == scope {
			st.removeSyncLocked(d, r)
		}
	}
	for _, r := range append([]*periodicReg(nil), d.Periodic...) {
		if r.Scope == scope {
			st.removePeriodicLocked(d, r)
		}
	}
}

// ---- host functions ---------------------------------------------------------------------------------------------------------

// regForCall resolves the registration a manager call is about; the caller must be on the same origin.
func (h *pwaHost) regForCall(id string) (*swReg, error) {
	st := h.st()
	reg := st.c.sw().regByID(id)
	if reg == nil || reg.origin != h.origin() || reg.unregistered {
		return nil, &swError{"InvalidStateError", "The service worker registration is gone."}
	}
	m := st.c.sw()
	m.mu.Lock()
	active := reg.active != nil
	m.mu.Unlock()
	if !active {
		return nil, &swError{"InvalidStateError", "Registration failed - no active Service Worker"}
	}
	return reg, nil
}

func (h *pwaHost) syncFns(f map[string]hostFn) {
	f["pwaSyncRegister"] = func(a []any) (any, error) {
		st := h.st()
		if !st.enabled.Load() || !h.secure() {
			return nil, &swError{"NotAllowedError", "Background Sync is not available here."}
		}
		reg, err := h.regForCall(argS(a, 0))
		if err != nil {
			return nil, err
		}
		st.registerSync(reg.origin, reg, argS(a, 1))
		return nil, nil
	}
	f["pwaSyncTags"] = func(a []any) (any, error) {
		reg, err := h.regForCall(argS(a, 0))
		if err != nil {
			return nil, err
		}
		return stringsToAny(h.st().syncTags(reg.origin, reg)), nil
	}
	f["pwaPeriodicRegister"] = func(a []any) (any, error) {
		st := h.st()
		reg, err := h.regForCall(argS(a, 0))
		if err != nil {
			return nil, err
		}
		if st.permission(reg.origin, PermPeriodicSync) != "granted" {
			return nil, &swError{"NotAllowedError", "Permission denied."}
		}
		st.registerPeriodic(reg.origin, reg, argS(a, 1), argI(a, 2))
		return nil, nil
	}
	f["pwaPeriodicUnregister"] = func(a []any) (any, error) {
		reg, err := h.regForCall(argS(a, 0))
		if err != nil {
			return nil, err
		}
		h.st().unregisterPeriodic(reg.origin, reg, argS(a, 1))
		return nil, nil
	}
	f["pwaPeriodicTags"] = func(a []any) (any, error) {
		reg, err := h.regForCall(argS(a, 0))
		if err != nil {
			return nil, err
		}
		return stringsToAny(h.st().periodicTags(reg.origin, reg)), nil
	}
}

func stringsToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// dispatchReg fires an event in the active worker of a registration, starting the worker's VM when it is not running.
func (m *swManager) dispatchReg(reg *swReg, kind string, payload any, timeout time.Duration) (swEventResult, error) {
	m.mu.Lock()
	w := reg.active
	m.mu.Unlock()
	if w == nil {
		return swEventResult{}, swErr("InvalidStateError", "the registration has no active service worker")
	}
	rt, err := m.runtimeFor(w)
	if err != nil {
		return swEventResult{}, err
	}
	return rt.dispatch(kind, payload, timeout)
}
