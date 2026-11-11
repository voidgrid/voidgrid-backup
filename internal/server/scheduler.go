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
// past means it is due now; missed runs collapse into one. A schedule that was
// changed, or switched back on, counts from that moment, so a new time of day
// that has already passed today waits for tomorrow, and a job that was off for
// a week does not run the instant it is switched on.
func NextRun(ctx context.Context, cat *catalog.Catalog, j catalog.Job) (time.Time, error) {
	from := j.CreatedAt
	if last, err := cat.LastRun(ctx, j.ID, "backup"); err == nil {
		from = last.StartedAt
	} else if !errors.Is(err, catalog.ErrNotFound) {
		return time.Time{}, err
	}
	if changed := scheduleChangedAt(ctx, cat, j.ID); changed.After(from) {
		from = changed
	}
	return nextRun(j.Schedule, from)
}

func scheduleChangedKey(jobID string) string { return "schedule_changed:" + jobID }

// scheduleChangedAt is when the job's schedule was last changed, zero if it
// never was (or the record cannot be read: the job is then scheduled as before).
func scheduleChangedAt(ctx context.Context, cat *catalog.Catalog, jobID string) time.Time {
	v, err := cat.GetSetting(ctx, scheduleChangedKey(jobID))
	if err != nil || v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// saveSchedule stores j, whose schedule an operator may just have changed from
// was, or switched on (wasEnabled says whether it was on before). Either is
// noted with the time, which NextRun counts from.
func (c *Controller) saveSchedule(ctx context.Context, j catalog.Job, was string, wasEnabled bool) error {
	if err := c.Catalog.UpdateJob(ctx, j); err != nil {
		return err
	}
	if j.Schedule == was && (wasEnabled || !j.Enabled) {
		return nil
	}
	return c.noteScheduleChange(ctx, j.ID)
}

func (c *Controller) noteScheduleChange(ctx context.Context, jobID string) error {
	return c.Catalog.SetSetting(ctx, scheduleChangedKey(jobID), time.Now().UTC().Format(time.RFC3339Nano))
}

// SetJobEnabled switches a job's schedule on or off. Switching it on counts as
// a schedule change: the job waits for its next scheduled start instead of
// running at once for the starts it missed while it was off.
func (c *Controller) SetJobEnabled(ctx context.Context, id string, enabled bool) error {
	if err := c.Catalog.SetJobEnabled(ctx, id, enabled); err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	return c.noteScheduleChange(ctx, id)
}

// nextRun is the first time after from that the cron expression matches, in the
// server's local zone. The catalog hands times back in UTC, and a cron
// expression without a zone of its own is read in the zone of the time it is
// given, so from is converted first: without that, "0 20 * * *" would mean
// 20:00 UTC whatever TZ is set to.
func nextRun(schedule string, from time.Time) (time.Time, error) {
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(from.In(time.Local)), nil
}

// Schedule starts due jobs and due repository maintenance every tick until
// ctx ends.
func (c *Controller) Schedule(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		now := time.Now()
		c.failOrphanRuns(ctx, now)
		c.startDue(ctx, now)
		c.startMaintenance(ctx, now)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// finishRunWaits are the pauses between attempts to record a run's result.
var finishRunWaits = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second}

// finishRun records how a run ended. The run is over whatever the catalog
// says, so a write that fails (a busy or briefly unwritable database) is tried
// again a few times rather than leaving the run recorded as running.
func (c *Controller) finishRun(ctx context.Context, run catalog.Run) error {
	ctx = context.WithoutCancel(ctx)
	err := c.Catalog.FinishRun(ctx, run)
	for _, wait := range finishRunWaits {
		if err == nil || errors.Is(err, catalog.ErrNotFound) {
			break
		}
		time.Sleep(wait)
		err = c.Catalog.FinishRun(ctx, run)
	}
	return err
}

// orphanRunAfter is how long a run may be recorded as running with nothing
// running for its job before it is taken for one whose result was never
// recorded. A run starts its record a moment after it takes its job.
const orphanRunAfter = 2 * time.Minute

// failOrphanRuns marks as failed the runs that are recorded as running while
// nothing is running for their job: the run ended and recording its result
// failed for good. Left alone such a run would show as running until the next
// restart.
func (c *Controller) failOrphanRuns(ctx context.Context, now time.Time) {
	runs, err := c.Catalog.RunningRuns(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("scheduler: list running runs", "err", err)
		}
		return
	}
	for _, r := range runs {
		if c.IsRunning(r.JobID) || now.Sub(r.StartedAt) < orphanRunAfter {
			continue
		}
		err := c.Catalog.FailRunning(ctx, r.ID, now, "the run ended, but its result could not be recorded; see the server's log from that time")
		if err != nil && !errors.Is(err, catalog.ErrNotFound) {
			slog.Error("scheduler: close a run whose result was never recorded", "job", r.JobID, "run", r.ID, "err", err)
			continue
		}
		slog.Warn("a run was still recorded as running with nothing running for its job; marked failed", "job", r.JobID, "run", r.ID, "kind", r.Kind)
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
