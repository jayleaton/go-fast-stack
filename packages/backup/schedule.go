package backup

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a 5-field cron expression (minute hour day-of-month month
// day-of-week, UTC) with *, lists, ranges and steps: "0 3 * * *" is 03:00
// every night. Day-of-month and day-of-week combine like cron: when both are
// restricted, either matching is enough.
type Schedule struct {
	expr                       string
	min, hour, dom, month, dow [64]bool
	domStar, dowStar           bool
}

const cronFields = 5 // minute hour day-of-month month day-of-week

// ParseSchedule parses a cron expression.
func ParseSchedule(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != cronFields {
		return Schedule{}, fmt.Errorf("backup: schedule %q must have %d fields (minute hour day month weekday)", expr, cronFields)
	}
	schedule := Schedule{expr: expr, domStar: fields[2] == "*", dowStar: fields[4] == "*"}
	for _, item := range []struct {
		field  string
		set    *[64]bool
		lo, hi int
	}{{fields[0], &schedule.min, 0, 59}, {fields[1], &schedule.hour, 0, 23}, {fields[2], &schedule.dom, 1, 31}, {fields[3], &schedule.month, 1, 12}, {fields[4], &schedule.dow, 0, 7}} {
		if err := parseField(item.field, item.set, item.lo, item.hi); err != nil {
			return Schedule{}, fmt.Errorf("backup: schedule %q: %w", expr, err)
		}
	}
	if schedule.dow[7] {
		schedule.dow[0] = true // 7 is Sunday too
	}
	return schedule, nil
}

func parseField(field string, set *[64]bool, lo, hi int) error {
	for part := range strings.SplitSeq(field, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			count, err := strconv.Atoi(stepStr)
			if err != nil || count < 1 {
				return fmt.Errorf("bad step in %q", part)
			}
			step = count
		}
		from, to := lo, hi
		if rng != "*" {
			left, right, isRange := strings.Cut(rng, "-")
			value, err := strconv.Atoi(left)
			if err != nil {
				return fmt.Errorf("bad value %q", part)
			}
			from, to = value, value
			if isRange {
				if to, err = strconv.Atoi(right); err != nil {
					return fmt.Errorf("bad range %q", part)
				}
			} else if hasStep {
				to = hi
			}
		}
		if from < lo || to > hi || from > to {
			return fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for value := from; value <= to; value += step {
			set[value] = true
		}
	}
	return nil
}

const maxScheduleYears = 5 // how far ahead Next looks before giving up

// Next is the first matching minute strictly after t (UTC). A schedule that
// never matches (Feb 30) returns the zero time.
func (schedule Schedule) Next(moment time.Time) time.Time {
	moment = moment.UTC().Truncate(time.Minute).Add(time.Minute)
	for limit := moment.AddDate(maxScheduleYears, 0, 0); moment.Before(limit); {
		switch {
		case !schedule.month[int(moment.Month())]:
			moment = time.Date(moment.Year(), moment.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !schedule.day(moment):
			moment = time.Date(moment.Year(), moment.Month(), moment.Day()+1, 0, 0, 0, 0, time.UTC)
		case !schedule.hour[moment.Hour()]:
			moment = moment.Truncate(time.Hour).Add(time.Hour)
		case !schedule.min[moment.Minute()]:
			moment = moment.Add(time.Minute)
		default:
			return moment
		}
	}
	return time.Time{}
}

func (schedule Schedule) day(moment time.Time) bool {
	dom, dow := schedule.dom[moment.Day()], schedule.dow[int(moment.Weekday())]
	switch {
	case schedule.domStar && schedule.dowStar:
		return true
	case schedule.domStar:
		return dow
	case schedule.dowStar:
		return dom
	}
	return dom || dow
}

// String returns the expression.
func (schedule Schedule) String() string { return schedule.expr }
