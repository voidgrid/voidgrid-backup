package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrNotRunning is returned by CancelRun when the job has nothing running.
var ErrNotRunning = errors.New("nothing is running for this job")

// runCancels holds the cancel function of each job's run in progress, so the
// Cancel button can stop it.
type runCancels struct {
	mu sync.Mutex
	m  map[string]context.CancelCauseFunc
}

// beginRun gives a run its own context, detached from the request that
// started it (a closed browser tab must not abort a restore) but cancellable
// through CancelRun. The returned func ends the registration.
func (c *Controller) beginRun(ctx context.Context, jobID string) (context.Context, func()) {
	rctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	c.cancels.mu.Lock()
	if c.cancels.m == nil {
		c.cancels.m = map[string]context.CancelCauseFunc{}
	}
	c.cancels.m[jobID] = cancel
	c.cancels.mu.Unlock()
	return rctx, func() {
		c.cancels.mu.Lock()
		delete(c.cancels.m, jobID)
		c.cancels.mu.Unlock()
		cancel(nil)
	}
}

// CancelRun stops the backup, check or restore running for the job. by names
// who asked, for the run's record. The run ends as failed with that reason, and
// notifies like any other run that did not succeed. Work the agent was doing
// stops at its next step; containers it paused are resumed and a VM's disk
// overlay is merged back either way.
func (c *Controller) CancelRun(jobID, by string) error {
	c.cancels.mu.Lock()
	cancel, ok := c.cancels.m[jobID]
	c.cancels.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	if by == "" {
		by = "the operator"
	}
	cancel(fmt.Errorf("cancelled by %s", by))
	return nil
}

// cancelledErr is the reason a run's context was cancelled with, in place of
// the transport's own "context canceled", when err is the result of that.
func cancelledErr(rctx context.Context, err error) error {
	if err == nil || rctx.Err() == nil {
		return err
	}
	if cause := context.Cause(rctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}
