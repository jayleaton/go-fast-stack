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
var DefaultRetention = Retention{Daily: 7, Weekly: 4}

const timeLayout = "20060102T150405Z"
const suffix = ".dump.age"

// Key is the backup id for db taken at t: <db>/<time>.dump.age.
func Key(db string, t time.Time) string {
	return db + "/" + t.UTC().Format(timeLayout) + suffix
}

// ParseKey returns the database and time of a backup id.
func ParseKey(key string) (db string, t time.Time, err error) {
	db, name, ok := strings.Cut(key, "/")
	if !ok || db == "" || !strings.HasSuffix(name, suffix) {
		return "", time.Time{}, fmt.Errorf("backup: %q is not a backup id (want <db>/<time>%s)", key, suffix)
	}
	t, err = time.Parse(timeLayout, strings.TrimSuffix(name, suffix))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("backup: %q is not a backup id: %w", key, err)
	}
	return db, t, nil
}

// Expired returns the objects of one database prune may delete at now,
// oldest first.
func (r Retention) Expired(objs []Object, now time.Time) []Object {
	type backup struct {
		Object
		t time.Time
	}
	var bs []backup
	for _, o := range objs {
		if _, t, err := ParseKey(o.Key); err == nil {
			bs = append(bs, backup{o, t})
		}
	}
	if len(bs) == 0 {
		return nil
	}
	slices.SortFunc(bs, func(a, b backup) int { return b.t.Compare(a.t) }) // newest first

	keep := map[string]bool{bs[0].Key: true}
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	days := map[time.Time]bool{}
	for i := range r.Daily {
		days[today.AddDate(0, 0, -i)] = true
	}
	weeks := map[[2]int]bool{}
	for i := range r.Weekly {
		y, w := today.AddDate(0, 0, -7*i).ISOWeek()
		weeks[[2]int{y, w}] = true
	}
	seenDay, seenWeek := map[time.Time]bool{}, map[[2]int]bool{}
	for _, b := range bs { // newest first: the first backup of a day/week is its newest
		t := b.t.UTC()
		if now.Sub(t) < 24*time.Hour {
			keep[b.Key] = true
		}
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		if days[day] && !seenDay[day] {
			seenDay[day] = true
			keep[b.Key] = true
		}
		y, w := t.ISOWeek()
		if wk := [2]int{y, w}; weeks[wk] && !seenWeek[wk] {
			seenWeek[wk] = true
			keep[b.Key] = true
		}
	}
	var out []Object
	for i := len(bs) - 1; i >= 0; i-- {
		if !keep[bs[i].Key] {
			out = append(out, bs[i].Object)
		}
	}
	return out
}
