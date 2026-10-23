package net

import "sync"

// StyleWriteLog records which elements had their style attribute written. A caller that wants to tell an element
// that animates for ever (a ticker, a spinner) from a page that is still revealing itself uses it: style writes do
// not count as content changes (Doc.ContentGen), and Doc.MutationGen counts them all alike.
type StyleWriteLog struct {
	mu  sync.Mutex
	cur map[int64]struct{}
}

// maxStyleLogged bounds the log: a page that rewrites thousands of elements per frame is not ambient anyway.
const maxStyleLogged = 4096

func (l *StyleWriteLog) add(id int64) {
	l.mu.Lock()
	if l.cur == nil {
		l.cur = map[int64]struct{}{}
	}
	if len(l.cur) < maxStyleLogged {
		l.cur[id] = struct{}{}
	}
	l.mu.Unlock()
}

// Take returns the backend ids of the elements written since the previous Take and clears the log.
func (l *StyleWriteLog) Take() map[int64]struct{} {
	l.mu.Lock()
	m := l.cur
	l.cur = nil
	l.mu.Unlock()
	return m
}

// RecordStyleWrites starts logging style attribute writes on d. The returned func stops it. Only one log is
// active per document at a time; starting another replaces it.
func (d *Doc) RecordStyleWrites() (*StyleWriteLog, func()) {
	l := &StyleWriteLog{}
	d.styleLog.Store(l)
	return l, func() { d.styleLog.CompareAndSwap(l, nil) }
}
