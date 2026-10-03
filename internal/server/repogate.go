package server

import (
	"context"
	"sync"
)

// repoConcurrency is how many interactive calls (browsing, refreshing the
// snapshot list, measuring) may use one repository's storage at once. A
// Hetzner Storage Box allows 10 simultaneous connections in total, shared by
// every agent and by scheduled backups, which are never held back by this.
const repoConcurrency = 2

type repoGate struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

// acquire waits for a free slot on the repository, or for ctx to end. The
// returned func gives the slot back.
func (g *repoGate) acquire(ctx context.Context, repoID string) (func(), error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]chan struct{}{}
	}
	ch := g.m[repoID]
	if ch == nil {
		ch = make(chan struct{}, repoConcurrency)
		g.m[repoID] = ch
	}
	g.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
