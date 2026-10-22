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

// ParseSchedule parses a cron expression.
func ParseSchedule(expr string) (Schedule, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return Schedule{}, fmt.Errorf("backup: schedule %q must have 5 fields (minute hour day month weekday)", expr)
	}
	s := Schedule{expr: expr, domStar: f[2] == "*", dowStar: f[4] == "*"}
	for _, p := range []struct {
		field  string
		set    *[64]bool
		lo, hi int
	}{{f[0], &s.min, 0, 59}, {f[1], &s.hour, 0, 23}, {f[2], &s.dom, 1, 31}, {f[3], &s.month, 1, 12}, {f[4], &s.dow, 0, 7}} {
		if err := parseField(p.field, p.set, p.lo, p.hi); err != nil {
			return Schedule{}, fmt.Errorf("backup: schedule %q: %w", expr, err)
		}
	}
	if s.dow[7] {
		s.dow[0] = true // 7 is Sunday too
	}
	return s, nil
}

func parseField(field string, set *[64]bool, lo, hi int) error {
	for part := range strings.SplitSeq(field, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return fmt.Errorf("bad step in %q", part)
			}
			step = n
		}
		from, to := lo, hi
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			x, err := strconv.Atoi(a)
			if err != nil {
				return fmt.Errorf("bad value %q", part)
			}
			from, to = x, x
			if isRange {
				if to, err = strconv.Atoi(b); err != nil {
					return fmt.Errorf("bad range %q", part)
				}
			} else if hasStep {
				to = hi
			}
		}
		if from < lo || to > hi || from > to {
			return fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := from; v <= to; v += step {
			set[v] = true
		}
	}
	return nil
}

// Next is the first matching minute strictly after t (UTC). A schedule that
// never matches (Feb 30) returns the zero time.
func (s Schedule) Next(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	for limit := t.AddDate(5, 0, 0); t.Before(limit); {
		switch {
		case !s.month[int(t.Month())]:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.day(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		case !s.hour[t.Hour()]:
			t = t.Truncate(time.Hour).Add(time.Hour)
		case !s.min[t.Minute()]:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

func (s Schedule) day(t time.Time) bool {
	dom, dow := s.dom[t.Day()], s.dow[int(t.Weekday())]
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	}
	return dom || dow
}

// String returns the expression.
func (s Schedule) String() string { return s.expr }
