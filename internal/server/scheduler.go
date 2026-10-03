package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// NextRun is when a scheduled job is next due: the first schedule time after
// its last backup (or after it was created, if it never ran). A time in the
// past means it is due now; missed runs collapse into one.
func NextRun(ctx context.Context, cat *catalog.Catalog, j catalog.Job) (time.Time, error) {
	sched, err := cron.ParseStandard(j.Schedule)
	if err != nil {
		return time.Time{}, err
	}
	from := j.CreatedAt
	if last, err := cat.LastRun(ctx, j.ID, "backup"); err == nil {
		from = last.StartedAt
	} else if !errors.Is(err, catalog.ErrNotFound) {
		return time.Time{}, err
	}
	return sched.Next(from), nil
}

// Schedule starts due jobs every tick until ctx ends.
func (c *Controller) Schedule(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		c.startDue(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Controller) startDue(ctx context.Context, now time.Time) {
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("scheduler: list jobs", "err", err)
		}
		return
	}
	for _, j := range jobs {
		if !j.Enabled || j.Schedule == "" || c.IsRunning(j.ID) {
			continue
		}
		next, err := NextRun(ctx, c.Catalog, j)
		if err != nil {
			slog.Error("scheduler: next run", "job", j.Name, "err", err)
			continue
		}
		if next.After(now) {
			continue
		}
		go func(j catalog.Job) {
			if _, err := c.RunJob(context.WithoutCancel(ctx), j.ID, "schedule"); err != nil && !errors.Is(err, ErrRunning) {
				slog.Error("scheduled backup", "job", j.Name, "err", err)
			}
		}(j)
	}
}
