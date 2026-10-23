package backup

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Retention is how many backups prune keeps per database: the newest backup
// of each of the last Daily UTC days and of each of the last Weekly ISO
// weeks (today and this week included). Backups from the last 24 hours
// (a manual one before a risky migration) and the newest backup are always
// kept, and objects whose key is not a backup this package wrote are never
// touched.
type Retention struct {
	Daily  int
	Weekly int
}

// DefaultRetention keeps a week of dailies and a month of weeklies.
var DefaultRetention = Retention{Daily: defaultKeepDaily, Weekly: defaultKeepWeekly}

const (
	defaultKeepDaily  = 7 // days
	defaultKeepWeekly = 4 // ISO weeks
)

const timeLayout = "20060102T150405Z"
const suffix = ".dump.age"

// Key is the backup id for db taken at t: <db>/<time>.dump.age.
func Key(db string, moment time.Time) string {
	return db + "/" + moment.UTC().Format(timeLayout) + suffix
}

// ParseKey returns the database and time of a backup id.
func ParseKey(key string) (db string, moment time.Time, err error) {
	db, name, ok := strings.Cut(key, "/")
	if !ok || db == "" || !strings.HasSuffix(name, suffix) {
		return "", time.Time{}, fmt.Errorf("backup: %q is not a backup id (want <db>/<time>%s)", key, suffix)
	}
	moment, err = time.Parse(timeLayout, strings.TrimSuffix(name, suffix))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("backup: %q is not a backup id: %w", key, err)
	}
	return db, moment, nil
}

// Expired returns the objects of one database prune may delete at now,
// oldest first.
func (retention Retention) Expired(objs []Object, now time.Time) []Object {
	type backup struct {
		Object
		moment time.Time
	}
	var bs []backup
	for _, object := range objs {
		if _, moment, err := ParseKey(object.Key); err == nil {
			bs = append(bs, backup{object, moment})
		}
	}
	if len(bs) == 0 {
		return nil
	}
	slices.SortFunc(bs, func(left, right backup) int { return right.moment.Compare(left.moment) }) // newest first

	keep := map[string]bool{bs[0].Key: true}
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := map[time.Time]bool{}
	for index := range retention.Daily {
		days[today.AddDate(0, 0, -index)] = true
	}
	weeks := map[[2]int]bool{}
	for index := range retention.Weekly {
		week, width := today.AddDate(0, 0, -7*index).ISOWeek()
		weeks[[2]int{week, width}] = true
	}
	seenDay, seenWeek := map[time.Time]bool{}, map[[2]int]bool{}
	for _, backup := range bs { // newest first: the first backup of a day/week is its newest
		moment := backup.moment.UTC()
		if now.Sub(moment) < 24*time.Hour {
			keep[backup.Key] = true
		}
		day := time.Date(moment.Year(), moment.Month(), moment.Day(), 0, 0, 0, 0, time.UTC)
		if days[day] && !seenDay[day] {
			seenDay[day] = true
			keep[backup.Key] = true
		}
		week, width := moment.ISOWeek()
		if wk := [2]int{week, width}; weeks[wk] && !seenWeek[wk] {
			seenWeek[wk] = true
			keep[backup.Key] = true
		}
	}
	var out []Object
	for index := len(bs) - 1; index >= 0; index-- {
		if !keep[bs[index].Key] {
			out = append(out, bs[index].Object)
		}
	}
	return out
}
