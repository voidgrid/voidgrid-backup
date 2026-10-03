package server

import (
	"context"
	"sync"
	"time"
)

// maxRefusalSources bounds how many source addresses refusalLog tracks at
// once; beyond it, new sources are counted together.
const maxRefusalSources = 1000

const otherSources = "other sources"

// refusalLog keeps wrong-token warnings from flooding the log. Per source
// address it logs the first refusal at once, counts the rest for one window,
// and then logs a single line with that count. Nothing is lost: the volume is
// bounded, but every source and the number of attempts still show up.
type refusalLog struct {
	window time.Duration
	warn   func(msg string, args ...any)

	mu      sync.Mutex
	sources map[string]*refusals
}

type refusals struct {
	since      time.Time
	suppressed int
}

func newRefusalLog(window time.Duration, warn func(string, ...any)) *refusalLog {
	return &refusalLog{window: window, warn: warn, sources: map[string]*refusals{}}
}

// record notes a refused attempt from src at now.
func (l *refusalLog) record(src string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.sources[src]
	if !ok && len(l.sources) >= maxRefusalSources {
		src = otherSources
		r, ok = l.sources[src]
	}
	if ok && now.Sub(r.since) < l.window {
		r.suppressed++
		return
	}
	if ok {
		l.summaryLocked(src, r)
	}
	l.sources[src] = &refusals{since: now}
	l.warn("agent registration refused: wrong token", "from", src)
}

// flush writes the count for every window that has ended and forgets those
// sources.
func (l *refusalLog) flush(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for src, r := range l.sources {
		if now.Sub(r.since) >= l.window {
			l.summaryLocked(src, r)
			delete(l.sources, src)
		}
	}
}

func (l *refusalLog) summaryLocked(src string, r *refusals) {
	if r.suppressed > 0 {
		l.warn("agent registration refused: more wrong-token attempts", "from", src, "count", r.suppressed, "within", l.window.String())
	}
}

// run flushes once per window until ctx ends.
func (l *refusalLog) run(ctx context.Context) {
	t := time.NewTicker(l.window)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.flush(time.Now().Add(l.window))
			return
		case now := <-t.C:
			l.flush(now)
		}
	}
}
