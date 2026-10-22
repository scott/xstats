package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// accountStartDates is parsed once at startup from XSTATS_START_DATES.
// Nil and empty both mean no account has a start date. The running process
// does not re-read the variable; a change needs a service restart.
var accountStartDates map[string]string

// backfillLookbackDays is the furthest a backfilled day may sit before the
// account's first follower snapshot. Posts older than that stay stored.
const backfillLookbackDays = 30

// configureStartDates parses XSTATS_START_DATES and stores the result.
// An unset or empty value clears any start dates. A bad value returns an
// error and leaves the previous map alone.
func configureStartDates() error {
	m, err := parseStartDates(os.Getenv("XSTATS_START_DATES"))
	if err != nil {
		return err
	}
	accountStartDates = m
	return nil
}

// parseStartDates reads comma-separated handle:YYYY-MM-DD pairs.
// Spaces are trimmed, handles are lowercased, and one leading @ is removed.
// An unset or blank value is no start dates. Empty handles, impossible
// dates, duplicate handles, and malformed pairs are errors that name the pair.
func parseStartDates(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	seen := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		pair := strings.TrimSpace(part)
		if pair == "" || strings.Count(pair, ":") != 1 {
			label := pair
			if label == "" {
				label = part
			}
			return nil, fmt.Errorf("XSTATS_START_DATES: bad pair %q: malformed pair", label)
		}
		handlePart, datePart, _ := strings.Cut(pair, ":")
		handle := normStartHandle(handlePart)
		date := strings.TrimSpace(datePart)
		if handle == "" {
			return nil, fmt.Errorf("XSTATS_START_DATES: bad pair %q: empty handle", pair)
		}
		if err := validStartDay(date); err != nil {
			return nil, fmt.Errorf("XSTATS_START_DATES: bad pair %q: invalid date %q", pair, date)
		}
		if _, ok := seen[handle]; ok {
			return nil, fmt.Errorf("XSTATS_START_DATES: bad pair %q: duplicate handle %q", pair, handle)
		}
		seen[handle] = date
	}
	return seen, nil
}

func normStartHandle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimPrefix(s, "@")
}

// validStartDay accepts a real calendar day written YYYY-MM-DD.
// time.Parse rejects impossible days, including 2026-02-29 and 2026-02-31.
func validStartDay(s string) error {
	if len(s) != 10 {
		return fmt.Errorf("invalid date")
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return err
	}
	if t.Format("2006-01-02") != s {
		return fmt.Errorf("invalid date")
	}
	return nil
}

func accountStart(handle string) string {
	if len(accountStartDates) == 0 {
		return ""
	}
	return accountStartDates[normStartHandle(handle)]
}

// postsOnOrAfter drops posts whose created_at calendar day, in loc, is
// before start. A created_at that is not a day is dropped too: it cannot
// be shown to fall on or after the start date.
func postsOnOrAfter(posts []scoredPost, start string, loc *time.Location) []scoredPost {
	if start == "" {
		return posts
	}
	out := make([]scoredPost, 0, len(posts))
	for _, p := range posts {
		day, ok := postDay(p.CreatedAt, loc)
		if ok && day >= start {
			out = append(out, p)
		}
	}
	return out
}

func historyOnOrAfter(hist []map[string]any, start string) []map[string]any {
	if start == "" {
		return hist
	}
	out := make([]map[string]any, 0, len(hist))
	for _, row := range hist {
		if row["date"].(string) >= start {
			out = append(out, row)
		}
	}
	return out
}

// clipEngagement drops chart days before start and rebuilds follower
// deltas and score_delta from the days that remain. The first visible
// snapshot has new_followers 0, because the hidden prior snapshot is not
// a baseline. Backfilled days stay out of every score_delta window.
// Snapshot rows in the database are not changed.
func clipEngagement(days []dayEngagement, hist []histDay, start string) []dayEngagement {
	if start == "" {
		return days
	}
	followers := make(map[string]int, len(hist))
	for _, h := range hist {
		followers[h.Date] = h.Followers
	}
	out := make([]dayEngagement, 0, len(days))
	droppedSnap := false
	for _, d := range days {
		if d.Date < start {
			if !d.Backfilled {
				droppedSnap = true
			}
			continue
		}
		out = append(out, d)
	}
	if !droppedSnap {
		return out
	}
	var prev *int
	for i := range out {
		if out[i].Backfilled {
			continue
		}
		f := followers[out[i].Date]
		nf := 0
		if prev != nil {
			nf = f - *prev
		}
		held := f
		out[i].NewFollowers = &nf
		out[i].Points = dayLevelPoints(knownCount(out[i].Comments), nf, out[i].Reshares, out[i].Likes)
		sc := float64(out[i].Points)
		out[i].Score = &sc
		prev = &held
	}
	for i := range out {
		out[i].ScoreDelta = priorWeekDelta(out, i)
	}
	return out
}
