package server

import (
	"container/list"
	"sync"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// browseCacheEntries bounds the cache by number of directory listings.
const browseCacheEntries = 512

// A snapshot never changes, so the listing of a directory in it (identified
// by repository, snapshot and path) is valid forever; the cache only needs a
// size bound. Identical requests arriving together share one call.
type browseKey struct{ repo, snapshot, path string }

type browseCall struct {
	done    chan struct{}
	entries []*agentpb.DirEntry
	err     error
}

type browseCache struct {
	mu       sync.Mutex
	order    *list.List // front = most recently used; values are browseKey
	items    map[browseKey]*list.Element
	data     map[browseKey][]*agentpb.DirEntry
	inflight map[browseKey]*browseCall
}

// do returns the cached listing for k or runs fetch to get it. Failures are
// not cached.
func (b *browseCache) do(k browseKey, fetch func() ([]*agentpb.DirEntry, error)) ([]*agentpb.DirEntry, error) {
	b.mu.Lock()
	if b.items == nil {
		b.order, b.items = list.New(), map[browseKey]*list.Element{}
		b.data, b.inflight = map[browseKey][]*agentpb.DirEntry{}, map[browseKey]*browseCall{}
	}
	if el, ok := b.items[k]; ok {
		b.order.MoveToFront(el)
		e := b.data[k]
		b.mu.Unlock()
		return e, nil
	}
	if call, ok := b.inflight[k]; ok {
		b.mu.Unlock()
		<-call.done
		return call.entries, call.err
	}
	call := &browseCall{done: make(chan struct{})}
	b.inflight[k] = call
	b.mu.Unlock()

	call.entries, call.err = fetch()

	b.mu.Lock()
	delete(b.inflight, k)
	if call.err == nil {
		b.items[k] = b.order.PushFront(k)
		b.data[k] = call.entries
		for b.order.Len() > browseCacheEntries {
			old := b.order.Back()
			b.order.Remove(old)
			delete(b.items, old.Value.(browseKey))
			delete(b.data, old.Value.(browseKey))
		}
	}
	b.mu.Unlock()
	close(call.done)
	return call.entries, call.err
}
