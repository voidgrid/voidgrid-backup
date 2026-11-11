package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Dragging a bar on the Schedule page moves its job's time of day. That is only
// well defined for a schedule with one start time per day, so these helpers
// accept just "M H * * D": a plain minute and hour, no day of the month or
// month, and a day of the week that is "*" or a list of numbers. Anything else
// (steps, ranges, several hours, @daily) is edited on the Jobs page.

var (
	ErrNotMovable      = errors.New("this schedule can't be moved by dragging; edit it on the Jobs page")
	ErrScheduleChanged = errors.New("the schedule changed since this page was loaded")
)

const minutesPerDay = 24 * 60

// parseMovable splits a schedule into its time of day (minutes after midnight)
// and the days of the week it is limited to (nil for every day).
func parseMovable(spec string) (tod int, dows []int, ok bool) {
	f := strings.Fields(spec)
	if len(f) != 5 || (f[2] != "*" && f[2] != "?") || f[3] != "*" {
		return 0, nil, false
	}
	m, err1 := strconv.Atoi(f[0])
	h, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || m < 0 || m > 59 || h < 0 || h > 23 {
		return 0, nil, false
	}
	if f[4] != "*" && f[4] != "?" {
		for _, s := range strings.Split(f[4], ",") {
			d, err := strconv.Atoi(s)
			if err != nil || d < 0 || d > 7 {
				return 0, nil, false
			}
			dows = append(dows, d%7) // cron allows both 0 and 7 for Sunday
		}
	}
	return h*60 + m, dows, true
}

// movable reports whether shiftSchedule can move spec.
func movable(spec string) bool {
	_, _, ok := parseMovable(spec)
	return ok
}

// shiftSchedule moves a movable schedule by minutes (negative is earlier). A
// move across midnight also moves the days of the week it is limited to.
func shiftSchedule(spec string, minutes int) (string, error) {
	tod, dows, ok := parseMovable(spec)
	if !ok {
		return "", ErrNotMovable
	}
	total := tod + minutes
	days := total / minutesPerDay
	if total%minutesPerDay < 0 {
		days-- // floor, not truncate toward zero
	}
	tod = total - days*minutesPerDay

	dow := "*"
	if dows != nil {
		seen := map[int]bool{}
		var moved []int
		for _, d := range dows {
			d = ((d+days)%7 + 7) % 7
			if !seen[d] {
				seen[d] = true
				moved = append(moved, d)
			}
		}
		sort.Ints(moved)
		parts := make([]string, len(moved))
		for i, d := range moved {
			parts[i] = strconv.Itoa(d)
		}
		dow = strings.Join(parts, ",")
	}
	out := fmt.Sprintf("%d %d * * %s", tod%60, tod/60, dow)
	if _, err := cron.ParseStandard(out); err != nil {
		return "", fmt.Errorf("moved schedule %q is invalid: %w", out, err)
	}
	return out, nil
}

// MoveSchedule shifts a job's schedule by minutes, provided it is still the
// schedule the caller saw (was). It returns the new schedule.
func (c *Controller) MoveSchedule(ctx context.Context, id, was string, minutes int) (string, error) {
	if !c.inflight.start(id) {
		return "", ErrRunning
	}
	defer c.inflight.done(id)
	j, err := c.Catalog.Job(ctx, id)
	if err != nil {
		return "", err
	}
	if j.Schedule != was {
		return "", ErrScheduleChanged
	}
	moved, err := shiftSchedule(j.Schedule, minutes)
	if err != nil {
		return "", err
	}
	j.Schedule = moved
	return moved, c.saveSchedule(ctx, j, was, j.Enabled)
}

// One-time conversion. Before v0.9.0-beta.14 every cron expression was read in
// UTC whatever TZ said, so the schedules saved then hold UTC hours. They are
// read in the server's zone now, and utcToLocal rewrites such a schedule so it
// fires at the same clock time as before.

const settingSchedulesConverted = "schedules_converted"

// ErrAlreadyConverted is returned once the conversion has been applied or
// dismissed: running it twice would shift every schedule twice.
var ErrAlreadyConverted = errors.New("the schedule conversion was already done (or hidden)")

// utcToLocal rewrites a schedule written in UTC hours into the server's zone,
// at the zone's offset as of now. A schedule that shiftSchedule can't move
// (steps, ranges, several hours, a day of the month) is ErrNotMovable.
func utcToLocal(spec string, now time.Time) (string, error) {
	_, off := now.Local().Zone()
	return shiftSchedule(spec, off/60)
}

// ScheduleConversion is one job in the conversion preview.
type ScheduleConversion struct {
	JobID, Name string
	From, To    string
	Skip        string // why this one can't be converted, "" if it can
}

// SchedulesConverted reports whether the conversion was applied or hidden.
func (c *Controller) SchedulesConverted(ctx context.Context) (bool, error) {
	v, err := c.Catalog.GetSetting(ctx, settingSchedulesConverted)
	return v != "", err
}

// ConversionPreview lists every job that has a schedule with what it would
// become. Nothing is offered when the server's zone is UTC: there is nothing to
// convert.
func (c *Controller) ConversionPreview(ctx context.Context, now time.Time) ([]ScheduleConversion, error) {
	if _, off := now.Local().Zone(); off == 0 {
		return nil, nil
	}
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	var out []ScheduleConversion
	for _, j := range jobs {
		if j.Schedule == "" {
			continue
		}
		sc := ScheduleConversion{JobID: j.ID, Name: j.Name, From: j.Schedule}
		if to, err := utcToLocal(j.Schedule, now); err != nil {
			sc.Skip = "not a single daily time: edit it by hand"
		} else {
			sc.To = to
		}
		out = append(out, sc)
	}
	return out, nil
}

// ConvertSchedules converts the schedules of the jobs in ids and records that
// the conversion is done. Jobs that are running or can't be converted are
// returned in skipped, with why.
func (c *Controller) ConvertSchedules(ctx context.Context, ids []string, now time.Time) (converted int, skipped []string, err error) {
	if done, err := c.SchedulesConverted(ctx); err != nil {
		return 0, nil, err
	} else if done {
		return 0, nil, ErrAlreadyConverted
	}
	for _, id := range ids {
		if !c.inflight.start(id) {
			skipped = append(skipped, id+": running")
			continue
		}
		err := func() error {
			defer c.inflight.done(id)
			j, err := c.Catalog.Job(ctx, id)
			if err != nil {
				return err
			}
			to, err := utcToLocal(j.Schedule, now)
			if err != nil {
				skipped = append(skipped, j.Name+": "+err.Error())
				return nil
			}
			j.Schedule = to
			if err := c.Catalog.UpdateJob(ctx, j); err != nil {
				return err
			}
			converted++
			return nil
		}()
		if err != nil {
			return converted, skipped, err
		}
	}
	return converted, skipped, c.Catalog.SetSetting(ctx, settingSchedulesConverted, now.UTC().Format(time.RFC3339))
}

// DismissConversion records that the schedules need no conversion.
func (c *Controller) DismissConversion(ctx context.Context, now time.Time) error {
	if done, err := c.SchedulesConverted(ctx); err != nil || done {
		return err
	}
	return c.Catalog.SetSetting(ctx, settingSchedulesConverted, "dismissed "+now.UTC().Format(time.RFC3339))
}
