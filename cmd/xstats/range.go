package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// timeNow is the clock for "today" and range windows. Tests pin it.
// Ingest still uses time.Now so a pinned clock does not rewrite snapshot dates.
var timeNow = time.Now

// bucketThresholds is the one place long-range cutovers live.
// A span of MaxDays or fewer uses that bucket. Days are inclusive
// calendar days in XSTATS_TZ: <=31 by day, <=182 by week, anything
// longer by month.
var bucketThresholds = []struct {
	MaxDays int
	Bucket  string
}{
	{31, "day"},
	{182, "week"},
	{1<<31 - 1, "month"},
}

func bucketForDays(days int) string {
	for _, t := range bucketThresholds {
		if days <= t.MaxDays {
			return t.Bucket
		}
	}
	return "month"
}

// rangeSpec is a resolved window. To may fall after today for a week
// (Sunday) or a month (the last day) so the chart can keep an empty slot.
// DataTo is the last day whose snapshots, posts and deltas count.
// From is empty for all-time: each account starts at its own first
// visible day, which #29 already clipped to the start date.
type rangeSpec struct {
	Key    string
	From   string
	To     string
	DataTo string
}

type statusError struct {
	code int
	msg  string
}

func (e statusError) Error() string { return e.msg }

// parseRangeQuery reads ?range=. An empty value keeps the legacy payload
// (full daily history, posts from the last 30 days) so existing clients
// stay on that response. Custom dates are checked here; "to" after today
// needs the stats zone and is checked in resolveRange.
func parseRangeQuery(q url.Values) (key, from, to string, legacy bool, err error) {
	key = strings.TrimSpace(q.Get("range"))
	if key == "" {
		return "", "", "", true, nil
	}
	switch key {
	case "today", "yesterday", "week", "month", "all":
		return key, "", "", false, nil
	case "custom":
		from = strings.TrimSpace(q.Get("from"))
		to = strings.TrimSpace(q.Get("to"))
		if validStartDay(from) != nil || validStartDay(to) != nil {
			return "", "", "", false, statusError{400, "custom range needs from and to as YYYY-MM-DD"}
		}
		if to < from {
			return "", "", "", false, statusError{400, "custom range: to is before from"}
		}
		return key, from, to, false, nil
	default:
		return "", "", "", false, statusError{400, fmt.Sprintf("unknown range %q", key)}
	}
}

// resolveRange turns a preset into civil days in loc.
// now is converted into loc first, so Sunday evening in that zone does
// not become Monday because UTC has already rolled over.
func resolveRange(key, from, to string, now time.Time, loc *time.Location) (rangeSpec, error) {
	todayT := civil(now.In(loc).Format("2006-01-02"))
	today := todayT.Format("2006-01-02")
	switch key {
	case "today":
		return rangeSpec{Key: "today", From: today, To: today, DataTo: today}, nil
	case "yesterday":
		y := addCivil(todayT, 0, 0, -1).Format("2006-01-02")
		return rangeSpec{Key: "yesterday", From: y, To: y, DataTo: y}, nil
	case "week":
		mon := addCivil(todayT, 0, 0, -mondayIndex(todayT))
		sun := addCivil(mon, 0, 0, 6)
		return rangeSpec{Key: "week", From: mon.Format("2006-01-02"), To: sun.Format("2006-01-02"), DataTo: today}, nil
	case "month":
		first := time.Date(todayT.Year(), todayT.Month(), 1, 0, 0, 0, 0, time.UTC)
		last := addCivil(first, 0, 1, -1)
		return rangeSpec{Key: "month", From: first.Format("2006-01-02"), To: last.Format("2006-01-02"), DataTo: today}, nil
	case "all":
		return rangeSpec{Key: "all", To: today, DataTo: today}, nil
	case "custom":
		if to > today {
			return rangeSpec{}, statusError{400, fmt.Sprintf("custom range: to is after today (%s)", today)}
		}
		return rangeSpec{Key: "custom", From: from, To: to, DataTo: to}, nil
	default:
		return rangeSpec{}, statusError{400, fmt.Sprintf("unknown range %q", key)}
	}
}

// civil parses a YYYY-MM-DD day in UTC. Calendar steps stay on UTC so a
// zone where local midnight does not exist (DST spring-forward) cannot
// repeat the previous day.
func civil(day string) time.Time {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return time.Time{}
	}
	return t
}

// addCivil moves by calendar years, months and days and lands on UTC midnight.
func addCivil(t time.Time, years, months, days int) time.Time {
	u := t.In(time.UTC)
	return time.Date(u.Year()+years, u.Month()+time.Month(months), u.Day()+days, 0, 0, 0, 0, time.UTC)
}

// mondayIndex is 0 on Monday and 6 on Sunday.
func mondayIndex(t time.Time) int {
	return (int(t.Weekday()) + 6) % 7
}

// daysInclusive counts civil days from..to. Date strings are parsed in UTC
// so a DST spring-forward cannot drop a day from the count.
func daysInclusive(from, to string) int {
	if from == "" || to == "" || to < from {
		return 0
	}
	a, err1 := time.Parse("2006-01-02", from)
	b, err2 := time.Parse("2006-01-02", to)
	if err1 != nil || err2 != nil || b.Before(a) {
		return 0
	}
	return int(b.Sub(a)/(24*time.Hour)) + 1
}

// bucketStart is the Monday of a week or the 1st of a month.
// The zone is not used: the civil Monday is the same everywhere.
func bucketStart(day, bucket string, _ *time.Location) string {
	t := civil(day)
	switch bucket {
	case "week":
		return addCivil(t, 0, 0, -mondayIndex(t)).Format("2006-01-02")
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	default:
		return day
	}
}

// bucketEnd is the Sunday of that week or the last day of that month.
func bucketEnd(start, bucket string, _ *time.Location) string {
	t := civil(start)
	switch bucket {
	case "week":
		return addCivil(t, 0, 0, 6).Format("2006-01-02")
	case "month":
		first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		return addCivil(first, 0, 1, -1).Format("2006-01-02")
	default:
		return start
	}
}

// maxSlotKeys caps a chart. A stuck step used to loop forever on a DST
// gap; the cap and the "next day must be later" check keep that from
// hanging a request if the calendar math breaks again.
const maxSlotKeys = 10000

// slotKeys lists one chart slot per bucket from the range start through
// slotTo. Week and month slots use the unclipped bucket start, so the
// first label can name a Monday or a 1st that sits just before the range.
// The step is a UTC civil date, so loc cannot shift it.
func slotKeys(from, slotTo, bucket string, loc *time.Location) []string {
	if from == "" || slotTo == "" {
		return []string{}
	}
	start := from
	if bucket == "week" || bucket == "month" {
		start = bucketStart(from, bucket, loc)
	}
	if slotTo < start {
		return []string{}
	}
	stepY, stepM, stepD := 0, 0, 1
	switch bucket {
	case "week":
		stepD = 7
		from = start
	case "month":
		stepM, stepD = 1, 0
		from = start
	}
	out := []string{}
	for k := from; k <= slotTo && len(out) < maxSlotKeys; {
		out = append(out, k)
		next := addCivil(civil(k), stepY, stepM, stepD).Format("2006-01-02")
		if next <= k {
			break
		}
		k = next
	}
	return out
}

func asIntOK(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case *int:
		if n == nil {
			return 0, false
		}
		return *n, true
	default:
		return 0, false
	}
}

func asFloatOK(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case *int:
		if n == nil {
			return 0, false
		}
		return float64(*n), true
	default:
		return 0, false
	}
}

// groupDaily sums daily rows into week or month buckets.
// from/to clip the edge buckets; days before from or after to are not
// in rows. Score and the category counts are sums of the daily values.
// Nil counts add nothing. Followers is the close of the last day in the
// bucket that has a snapshot.
func groupDaily(rows []map[string]any, from, to, bucket string, loc *time.Location) []map[string]any {
	type acc struct {
		rows                                    []map[string]any
		score                                   float64
		comments, newFollowers, reshares, likes int
		impressions                             int
		commentsKnown, nfKnown, imprKnown       bool
		scored                                  int
		allBackfilled                           bool
		followers, following, posts             int
		haveClose                               bool
		last                                    string
	}
	order := []string{}
	by := map[string]*acc{}
	for _, row := range rows {
		day, _ := row["date"].(string)
		if day < from || day > to {
			continue
		}
		k := bucketStart(day, bucket, loc)
		g := by[k]
		if g == nil {
			g = &acc{allBackfilled: true}
			by[k] = g
			order = append(order, k)
		}
		g.rows = append(g.rows, row)
		e, _ := row["engagement"].(map[string]any)
		if e != nil {
			if sc, ok := asFloatOK(e["score"]); ok {
				g.score += sc
				g.scored++
			}
			if v, ok := asIntOK(e["comments"]); ok {
				g.comments += v
				g.commentsKnown = true
			}
			if v, ok := asIntOK(e["new_followers"]); ok {
				g.newFollowers += v
				g.nfKnown = true
			}
			if v, ok := asIntOK(e["reshares"]); ok {
				g.reshares += v
			}
			if v, ok := asIntOK(e["likes"]); ok {
				g.likes += v
			}
			if v, ok := asIntOK(e["impressions"]); ok {
				g.impressions += v
				g.imprKnown = true
			}
			if e["backfilled"] != true {
				g.allBackfilled = false
			}
		} else {
			g.allBackfilled = false
		}
		if f, ok := asIntOK(row["followers"]); ok {
			g.followers = f
			g.haveClose = true
			g.last = day
			if v, ok := asIntOK(row["following"]); ok {
				g.following = v
			}
			if v, ok := asIntOK(row["posts"]); ok {
				g.posts = v
			}
		}
	}
	out := make([]map[string]any, 0, len(order))
	for _, k := range order {
		g := by[k]
		end := bucketEnd(k, bucket, loc)
		clipFrom := k
		if clipFrom < from {
			clipFrom = from
		}
		clipTo := end
		if clipTo > to {
			clipTo = to
		}
		eng := map[string]any{
			"score":         g.score,
			"reshares":      g.reshares,
			"likes":         g.likes,
			"scored":        g.scored,
			"backfilled":    g.allBackfilled && g.scored > 0,
			"comments":      nil,
			"new_followers": nil,
			"impressions":   nil,
		}
		if g.commentsKnown {
			eng["comments"] = g.comments
		}
		if g.nfKnown {
			eng["new_followers"] = g.newFollowers
		}
		if g.imprKnown {
			eng["impressions"] = g.impressions
		}
		point := map[string]any{
			"date":       k,
			"from":       clipFrom,
			"to":         clipTo,
			"days":       daysInclusive(clipFrom, clipTo),
			"full":       daysInclusive(k, end),
			"snaps":      len(g.rows),
			"bucket":     bucket,
			"followers":  nil,
			"following":  nil,
			"posts":      nil,
			"last":       "",
			"engagement": eng,
		}
		if g.haveClose {
			point["followers"] = g.followers
			point["following"] = g.following
			point["posts"] = g.posts
			point["last"] = g.last
		}
		out = append(out, point)
	}
	return out
}

func sumDailyScores(rows []map[string]any) float64 {
	var s float64
	for _, row := range rows {
		e, _ := row["engagement"].(map[string]any)
		if e == nil {
			continue
		}
		if sc, ok := asFloatOK(e["score"]); ok {
			s += sc
		}
	}
	return s
}

// followerSnaps is every daily row that has a follower count, in order.
// Backfilled days have a null count and are left out. The page's dlt()
// uses this series for the fixed 7-day tile.
func followerSnaps(hist []map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, row := range hist {
		n, ok := asIntOK(row["followers"])
		if !ok {
			continue
		}
		d, _ := row["date"].(string)
		out = append(out, map[string]any{"date": d, "followers": n})
	}
	return out
}

func filterHist(rows []map[string]any, from, to string) []map[string]any {
	out := []map[string]any{}
	for _, row := range rows {
		d, _ := row["date"].(string)
		if d >= from && d <= to {
			out = append(out, row)
		}
	}
	return out
}

// deltaFollowers is the range's follower change.
// Baseline is the last snapshot before from. With no earlier snapshot the
// baseline is the first snapshot inside the window, so all-time is the
// change since the first visible day and a single first day is 0.
func deltaFollowers(hist []map[string]any, from, to string) map[string]any {
	var baseDate string
	var baseN int
	haveBase := false
	var firstN, lastN int
	haveFirst, haveLast := false, false
	for _, row := range hist {
		d, _ := row["date"].(string)
		f, ok := asIntOK(row["followers"])
		if !ok {
			continue
		}
		if d < from {
			baseDate, baseN, haveBase = d, f, true
			continue
		}
		if d > to {
			continue
		}
		if !haveFirst {
			firstN, haveFirst = f, true
		}
		lastN, haveLast = f, true
	}
	if !haveLast {
		return map[string]any{"followers": nil, "baseline": nil, "since_first": false}
	}
	if haveBase {
		return map[string]any{"followers": lastN - baseN, "baseline": baseDate, "since_first": false}
	}
	return map[string]any{"followers": lastN - firstN, "baseline": nil, "since_first": true}
}

func tilesFrom(rows []map[string]any) (followers, following, posts any) {
	followers, following, posts = nil, nil, nil
	for _, row := range rows {
		if _, ok := asIntOK(row["followers"]); !ok {
			continue
		}
		followers = row["followers"]
		following = row["following"]
		posts = row["posts"]
	}
	return followers, following, posts
}

func lastSnapshotOnOrBefore(hist []map[string]any, today string) string {
	last := ""
	for _, row := range hist {
		d, _ := row["date"].(string)
		if _, ok := asIntOK(row["followers"]); !ok || d > today {
			continue
		}
		if d >= last {
			last = d
		}
	}
	return last
}

const rangePostLimit = 100

func filterPosts(raw any, from, to string, loc *time.Location) ([]map[string]any, int) {
	rows, _ := raw.([]map[string]any)
	kept := []map[string]any{}
	if from == "" || to == "" || from > to {
		return kept, 0
	}
	for _, p := range rows {
		created, _ := p["created_at"].(string)
		day, ok := postDay(created, loc)
		if !ok || day < from || day > to {
			continue
		}
		kept = append(kept, p)
	}
	total := len(kept)
	if len(kept) > rangePostLimit {
		kept = kept[:rangePostLimit]
	}
	return kept, total
}

func filterDated(rows []map[string]any, from, to string) []map[string]any {
	out := []map[string]any{}
	for _, row := range rows {
		d, _ := row["date"].(string)
		if from != "" && d < from {
			continue
		}
		if to != "" && d > to {
			continue
		}
		out = append(out, row)
	}
	return out
}

// projectAccount replaces one account's history with the range's series.
// Days before the account start are already gone from hist (#29). All-time
// begins at the first remaining day: the later of the start date and the
// first snapshot, or a backfilled day on or after the start date when #29
// kept one. The picker uses that same first day as its lower bound.
func projectAccount(handle string, acct map[string]any, spec rangeSpec, loc *time.Location, today, globalFloor string) {
	hist, _ := acct["history"].([]map[string]any)
	if hist == nil {
		hist = []map[string]any{}
	}
	start := accountStart(handle)
	acct["start"] = start
	first := ""
	if len(hist) > 0 {
		first, _ = hist[0]["date"].(string)
	}
	acct["first"] = first
	acct["last_snapshot"] = lastSnapshotOnOrBefore(hist, today)
	// d7_snaps is the full daily follower series. The 7d tile runs the
	// v0.1.1 dlt() on it, so the number stays a fixed 7 days when history
	// is windowed or grouped.
	acct["d7_snaps"] = followerSnaps(hist)

	visFrom := spec.From
	dataTo := spec.DataTo
	slotTo := spec.To
	// floor is the first day this account can show. With no snapshots
	// that is the start date, or the earliest day anyone can show, so a
	// request for year 1 does not build slots back to 0001-01-01.
	floor := first
	if floor == "" {
		floor = start
	}
	if floor == "" {
		floor = globalFloor
	}
	if spec.Key == "all" || visFrom == "" || (floor != "" && visFrom < floor) {
		visFrom = floor
	}
	if spec.Key == "custom" || spec.Key == "all" {
		slotTo = dataTo
	}
	empty := func() {
		acct["from"] = visFrom
		acct["to"] = slotTo
		acct["data_to"] = dataTo
		acct["days"] = 0
		acct["bucket"] = "day"
		acct["history"] = []map[string]any{}
		acct["slots"] = []string{}
		acct["delta"] = map[string]any{"followers": nil, "baseline": nil, "since_first": false}
		acct["score"] = 0
		acct["followers"], acct["following"], acct["post_count"] = nil, nil, nil
		posts, total := filterPosts(acct["posts"], visFrom, dataTo, loc)
		acct["posts"] = posts
		acct["posts_total"] = total
	}
	if visFrom == "" || dataTo == "" || visFrom > dataTo {
		empty()
		return
	}
	if slotTo == "" || slotTo < visFrom {
		slotTo = dataTo
	}
	days := daysInclusive(visFrom, dataTo)
	bkt := bucketForDays(days)
	window := filterHist(hist, visFrom, dataTo)
	var series []map[string]any
	if bkt == "day" {
		series = window
	} else {
		series = groupDaily(window, visFrom, dataTo, bkt, loc)
	}
	acct["from"] = visFrom
	acct["to"] = slotTo
	acct["data_to"] = dataTo
	acct["days"] = days
	acct["bucket"] = bkt
	acct["history"] = series
	acct["slots"] = slotKeys(visFrom, slotTo, bkt, loc)
	acct["score"] = sumDailyScores(window)
	acct["delta"] = deltaFollowers(hist, visFrom, dataTo)
	f, g, p := tilesFrom(window)
	acct["followers"], acct["following"], acct["post_count"] = f, g, p
	posts, total := filterPosts(acct["posts"], visFrom, dataTo, loc)
	acct["posts"] = posts
	acct["posts_total"] = total
}

func statusCode(err error) int {
	if err == nil {
		return 200
	}
	if se, ok := err.(statusError); ok && se.code != 0 {
		return se.code
	}
	return 500
}

// apiDataRanged loads the full daily series, then keeps the requested window.
// Grouping runs here so the page plots the series it is given.
func apiDataRanged(db *sql.DB, key, from, to string) (any, error) {
	loc, err := statsLocation()
	if err != nil {
		return nil, err
	}
	now := timeNow().In(loc)
	spec, err := resolveRange(key, from, to, now, loc)
	if err != nil {
		return nil, err
	}
	raw, err := apiData(db, true)
	if err != nil {
		return nil, err
	}
	out := raw.(map[string]any)
	applyRangeView(out, spec, loc, now.Format("2006-01-02"))
	return out, nil
}

// applyRangeView shapes a full daily payload into one range.
// The top-level bucket follows the requested span. Each account's bucket
// follows the days that account can actually show, after its start date
// and first snapshot. The page charts the account bucket.
func applyRangeView(out map[string]any, spec rangeSpec, loc *time.Location, today string) {
	out["tz"] = loc.String()
	out["today"] = today
	accts, _ := out["accounts"].(map[string]any)
	earliest := ""
	for _, raw := range accts {
		acct, _ := raw.(map[string]any)
		if acct == nil {
			continue
		}
		hist, _ := acct["history"].([]map[string]any)
		if len(hist) == 0 {
			continue
		}
		if f, _ := hist[0]["date"].(string); f != "" && (earliest == "" || f < earliest) {
			earliest = f
		}
	}
	// globalFloor is the first day the response can start. A custom from
	// of 0001-01-01 would otherwise count ~100k days and bucket by month.
	globalFloor := earliest
	if globalFloor == "" {
		globalFloor = today
	}
	for handle, raw := range accts {
		acct, _ := raw.(map[string]any)
		if acct == nil {
			continue
		}
		projectAccount(handle, acct, spec, loc, today, globalFloor)
	}
	spanFrom, spanTo := spec.From, spec.DataTo
	if spec.Key == "all" {
		spanFrom = earliest
		spanTo = today
	}
	if spanFrom == "" || spanFrom < globalFloor {
		spanFrom = globalFloor
	}
	days := daysInclusive(spanFrom, spanTo)
	bucket := "day"
	if days > 0 {
		bucket = bucketForDays(days)
	}
	out["bucket"] = bucket
	rng := map[string]any{
		"key":     spec.Key,
		"to":      spec.To,
		"data_to": spec.DataTo,
		"days":    days,
	}
	if spanFrom != "" {
		rng["from"] = spanFrom
	}
	out["range"] = rng
	if ct, ok := out["clickthroughs"].([]map[string]any); ok {
		out["clickthroughs"] = filterDated(ct, spanFrom, spec.DataTo)
	}
}
