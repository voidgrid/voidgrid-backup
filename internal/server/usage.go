package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/guard"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// usageTimeout covers the agent's own walk limit (fsusage.DefaultLimits) plus
// the round trip.
const usageTimeout = 40 * time.Second

// usageTTL is how long a measurement is reused, so reloading a page doesn't
// walk the same trees again.
const usageTTL = 2 * time.Minute

type usageKey struct {
	agent, path string
	walk        bool
}

type usageEntry struct {
	at time.Time
	u  *agentpb.PathUsage
}

type usageCache struct {
	mu sync.Mutex
	m  map[usageKey]usageEntry
}

func (c *usageCache) get(k usageKey) (*agentpb.PathUsage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Since(e.at) > usageTTL {
		return nil, false
	}
	return e.u, true
}

func (c *usageCache) put(k usageKey, u *agentpb.PathUsage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[usageKey]usageEntry{}
	}
	for old, e := range c.m { // entries are few; drop the expired ones as we go
		if time.Since(e.at) > usageTTL {
			delete(c.m, old)
		}
	}
	c.m[k] = usageEntry{at: time.Now(), u: u}
}

// PathUsage measures one host path on an agent. walk also totals the files
// under it, which can take a while on a big tree.
func (c *Controller) PathUsage(ctx context.Context, agentID, path string, walk bool) (*agentpb.PathUsage, error) {
	clean, err := guard.SourcePath(path)
	if err != nil {
		return nil, err
	}
	key := usageKey{agent: agentID, path: clean, walk: walk}
	if u, ok := c.usage.get(key); ok {
		return u, nil
	}
	agent, err := c.Catalog.Agent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	resp, err := withAgent(c, agent, usageTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.PathUsageResponse, error) {
		return cl.PathUsage(ctx, &agentpb.PathUsageRequest{Paths: []string{clean}, Walk: walk})
	})
	if err != nil {
		return nil, err
	}
	if len(resp.GetUsages()) != 1 {
		return nil, fmt.Errorf("agent returned %d results for one path", len(resp.GetUsages()))
	}
	u := resp.GetUsages()[0]
	if u.GetError() == "" { // don't cache failures: the path may appear later
		c.usage.put(key, u)
	}
	return u, nil
}

// usageView is what the pages' script reads: sizes already humanised, plus the
// raw numbers.
type usageView struct {
	Path      string `json:"path"`
	Error     string `json:"error,omitempty"`
	Size      string `json:"size,omitempty"` // tree size, "" if not walked
	Bytes     int64  `json:"bytes"`
	Files     int64  `json:"files"`
	Truncated bool   `json:"truncated"`
	FSUsed    string `json:"fs_used"`
	FSTotal   string `json:"fs_total"`
	FSAvail   string `json:"fs_avail"`
}

// apiUsage serves GET /api/agents/{id}/usage?path=/srv/app[&walk=1].
func (u *ui) apiUsage(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	walk := r.URL.Query().Get("walk") != ""
	w.Header().Set("Content-Type", "application/json")
	out := usageView{Path: path}
	pu, err := u.c.PathUsage(r.Context(), r.PathValue("id"), path, walk)
	switch {
	case err != nil:
		out.Error = err.Error()
	case pu.GetError() != "":
		out.Path, out.Error = pu.GetPath(), pu.GetError()
	default:
		out.Path = pu.GetPath()
		out.Bytes, out.Files, out.Truncated = pu.GetDirBytes(), pu.GetFiles(), pu.GetTruncated()
		if pu.GetWalked() {
			out.Size = HumanBytes(pu.GetDirBytes())
			if pu.GetTruncated() {
				out.Size = "at least " + out.Size
			}
		}
		out.FSUsed, out.FSTotal, out.FSAvail = HumanBytes(pu.GetFsUsed()), HumanBytes(pu.GetFsTotal()), HumanBytes(pu.GetFsAvail())
	}
	json.NewEncoder(w).Encode(out) //nolint:errcheck // status already sent; a failed write means the client went away
}
