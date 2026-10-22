package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func pinNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := timeNow
	timeNow = func() time.Time { return at }
	t.Cleanup(func() { timeNow = prev })
}

func denverAt(t *testing.T, y int, m time.Month, d, hh, mm int) time.Time {
	t.Helper()
	return time.Date(y, m, d, hh, mm, 0, 0, denver(t))
}

func TestBucketThresholds(t *testing.T) {
	cases := []struct {
		days int
		want string
	}{
		{1, "day"},
		{31, "day"},
		{32, "week"},
		{182, "week"},
		{183, "month"},
		{400, "month"},
	}
	for _, c := range cases {
		if got := bucketForDays(c.days); got != c.want {
			t.Fatalf("%d days: got %s, want %s", c.days, got, c.want)
		}
	}
	if daysInclusive("2026-04-23", "2026-10-22") != 183 {
		t.Fatalf("183-day span is %d", daysInclusive("2026-04-23", "2026-10-22"))
	}
	if daysInclusive("2026-04-24", "2026-10-22") != 182 {
		t.Fatalf("182-day span is %d", daysInclusive("2026-04-24", "2026-10-22"))
	}
	if daysInclusive("2026-09-22", "2026-10-22") != 31 || daysInclusive("2026-09-21", "2026-10-22") != 32 {
		t.Fatal("31/32 day spans drifted")
	}
}

// Sunday evening in Denver is still Monday in UTC. The week must stay
// the Denver week, Monday through Sunday, not the UTC week.
func TestRangeWindowsInDenver(t *testing.T) {
	loc := denver(t)
	// 22:00 MDT on Sunday is 04:00 UTC Monday.
	sundayNight := time.Date(2026, 10, 18, 22, 0, 0, 0, loc)
	if sundayNight.UTC().Format("2006-01-02") != "2026-10-19" {
		t.Fatalf("fixture is not Monday in UTC: %s", sundayNight.UTC().Format(time.RFC3339))
	}
	week, err := resolveRange("week", "", "", sundayNight, loc)
	if err != nil {
		t.Fatal(err)
	}
	if week.From != "2026-10-12" || week.To != "2026-10-18" || week.DataTo != "2026-10-18" {
		t.Fatalf("sunday night week %+v", week)
	}
	if bucketStart("2026-10-18", "week", loc) != "2026-10-12" || bucketStart("2026-10-19", "week", loc) != "2026-10-19" {
		t.Fatal("Monday boundary moved")
	}

	// Thursday 23:30 MDT is Friday morning UTC. Today stays Thursday.
	thursday := time.Date(2026, 10, 22, 23, 30, 0, 0, loc)
	if thursday.UTC().Format("2006-01-02") != "2026-10-23" {
		t.Fatalf("fixture utc day %s", thursday.UTC().Format(time.RFC3339))
	}
	checks := []rangeSpec{
		{Key: "today", From: "2026-10-22", To: "2026-10-22", DataTo: "2026-10-22"},
		{Key: "yesterday", From: "2026-10-21", To: "2026-10-21", DataTo: "2026-10-21"},
		{Key: "week", From: "2026-10-19", To: "2026-10-25", DataTo: "2026-10-22"},
		{Key: "month", From: "2026-10-01", To: "2026-10-31", DataTo: "2026-10-22"},
	}
	for _, want := range checks {
		got, err := resolveRange(want.Key, "", "", thursday, loc)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: got %+v want %+v", want.Key, got, want)
		}
	}
	custom, err := resolveRange("custom", "2026-10-05", "2026-10-14", thursday, loc)
	if err != nil || custom.DataTo != "2026-10-14" || daysInclusive(custom.From, custom.DataTo) != 10 {
		t.Fatalf("custom %+v %v", custom, err)
	}

	// Monday morning in Tokyo is still Sunday UTC. The week starts that Monday.
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	mondayJST := time.Date(2026, 10, 19, 0, 30, 0, 0, tokyo)
	if mondayJST.UTC().Weekday() != time.Sunday {
		t.Fatalf("fixture utc weekday %s", mondayJST.UTC().Weekday())
	}
	tw, err := resolveRange("week", "", "", mondayJST, tokyo)
	if err != nil {
		t.Fatal(err)
	}
	if tw.From != "2026-10-19" || tw.DataTo != "2026-10-19" || tw.To != "2026-10-25" {
		t.Fatalf("tokyo week %+v", tw)
	}
}

func TestMondayWeeksInZone(t *testing.T) {
	loc := denver(t)
	// 2026-03-08 is the US spring-forward Sunday. The week is still 7 civil days.
	start := bucketStart("2026-03-08", "week", loc)
	end := bucketEnd(start, "week", loc)
	if start != "2026-03-02" || end != "2026-03-08" || daysInclusive(start, end) != 7 {
		t.Fatalf("dst week %s..%s", start, end)
	}
	if bucketStart("2026-03-09", "week", loc) != "2026-03-09" {
		t.Fatal("Monday after the spring-forward joined the previous week")
	}
	rows := []map[string]any{
		dayRow("2026-03-08", 10, 3),
		dayRow("2026-03-09", 11, 4),
	}
	got := groupDaily(rows, "2026-03-08", "2026-03-09", "week", loc)
	if len(got) != 2 || got[0]["date"] != "2026-03-02" || got[1]["date"] != "2026-03-09" {
		t.Fatalf("sunday/monday buckets %+v", []any{got[0]["date"], got[1]["date"]})
	}
}

func TestEdgeBucketClipping(t *testing.T) {
	loc := denver(t)
	// Jul 22 2026 is Wednesday. Oct 18 2026 is Sunday. First week is clipped.
	from, to := "2026-07-22", "2026-10-18"
	if daysInclusive(from, to) != 89 || bucketForDays(89) != "week" {
		t.Fatalf("span %d", daysInclusive(from, to))
	}
	var rows []map[string]any
	for d := civil(from); !d.After(civil(to)); d = addCivil(d, 0, 0, 1) {
		rows = append(rows, dayRow(d.Format("2006-01-02"), 100, 2))
	}
	got := groupDaily(rows, from, to, "week", loc)
	first, last := got[0], got[len(got)-1]
	if first["date"] != "2026-07-20" || first["from"] != "2026-07-22" || first["to"] != "2026-07-26" || first["days"] != 5 || first["full"] != 7 {
		t.Fatalf("first bucket %+v", first)
	}
	if last["date"] != "2026-10-12" || last["from"] != "2026-10-12" || last["to"] != "2026-10-18" || last["days"] != 7 || last["full"] != 7 {
		t.Fatalf("last bucket %+v", last)
	}
	// A month range that starts and ends mid-month clips both edges.
	march := dayRow("2026-03-15", 1, 1)
	april := dayRow("2026-04-10", 2, 1)
	months := groupDaily([]map[string]any{march, april}, "2026-03-15", "2026-04-10", "month", loc)
	if len(months) != 2 {
		t.Fatalf("months %d", len(months))
	}
	if months[0]["date"] != "2026-03-01" || months[0]["from"] != "2026-03-15" || months[0]["to"] != "2026-03-31" || months[0]["days"] != 17 || months[0]["full"] != 31 {
		t.Fatalf("march %+v", months[0])
	}
	if months[1]["date"] != "2026-04-01" || months[1]["from"] != "2026-04-01" || months[1]["to"] != "2026-04-10" || months[1]["days"] != 10 || months[1]["full"] != 30 {
		t.Fatalf("april %+v", months[1])
	}
}

func TestBucketSumsMatchDailyTotals(t *testing.T) {
	loc := denver(t)
	from, to := "2026-01-01", "2026-07-02"
	if daysInclusive(from, to) != 183 || bucketForDays(183) != "month" {
		t.Fatalf("span %d bucket %s", daysInclusive(from, to), bucketForDays(daysInclusive(from, to)))
	}
	var rows []map[string]any
	var wantScore float64
	var wantLikes, wantComments, wantReshares, wantNF, wantImpr int
	for i, d := 0, civil(from); !d.After(civil(to)); i, d = i+1, addCivil(d, 0, 0, 1) {
		likes := i % 5
		comments := i % 3
		reshares := i % 2
		nf := 1
		impr := 10 + i
		if i == 40 {
			// A backfilled day has no follower count and no new_followers.
			row := map[string]any{
				"date": d.Format("2006-01-02"), "followers": nil, "following": nil, "posts": nil,
				"engagement": map[string]any{
					"score": 4.0, "likes": likes, "comments": comments, "reshares": reshares,
					"new_followers": nil, "impressions": impr, "backfilled": true,
				},
			}
			rows = append(rows, row)
			wantScore += 4
			wantLikes += likes
			wantComments += comments
			wantReshares += reshares
			wantImpr += impr
			continue
		}
		score := float64(dayLevelPoints(comments, nf, reshares, likes))
		row := dayRow(d.Format("2006-01-02"), 1000+i, score)
		eng := row["engagement"].(map[string]any)
		eng["likes"] = likes
		eng["comments"] = comments
		eng["reshares"] = reshares
		eng["new_followers"] = nf
		eng["impressions"] = impr
		rows = append(rows, row)
		wantScore += score
		wantLikes += likes
		wantComments += comments
		wantReshares += reshares
		wantNF += nf
		wantImpr += impr
	}
	buckets := groupDaily(rows, from, to, "month", loc)
	if len(buckets) != 7 {
		t.Fatalf("months %d", len(buckets))
	}
	var gotScore float64
	var gotLikes, gotComments, gotReshares, gotNF, gotImpr int
	for _, b := range buckets {
		e := b["engagement"].(map[string]any)
		gotScore += e["score"].(float64)
		gotLikes += e["likes"].(int)
		gotComments += e["comments"].(int)
		gotReshares += e["reshares"].(int)
		gotNF += e["new_followers"].(int)
		gotImpr += e["impressions"].(int)
	}
	if math.Abs(gotScore-wantScore) > 1e-6 || gotLikes != wantLikes || gotComments != wantComments || gotReshares != wantReshares || gotNF != wantNF || gotImpr != wantImpr {
		t.Fatalf("month sums score %v/%v likes %d/%d comments %d/%d reshares %d/%d nf %d/%d impr %d/%d",
			gotScore, wantScore, gotLikes, wantLikes, gotComments, wantComments, gotReshares, wantReshares, gotNF, wantNF, gotImpr, wantImpr)
	}
	weeks := groupDaily(rows, from, to, "week", loc)
	gotScore = 0
	for _, b := range weeks {
		gotScore += b["engagement"].(map[string]any)["score"].(float64)
	}
	if math.Abs(gotScore-wantScore) > 1e-6 {
		t.Fatalf("week score %v want %v", gotScore, wantScore)
	}
	// January is a full month. July is clipped to the 2nd.
	if buckets[0]["from"] != "2026-01-01" || buckets[0]["days"] != 31 || buckets[0]["full"] != 31 {
		t.Fatalf("january %+v", buckets[0])
	}
	july := buckets[len(buckets)-1]
	if july["date"] != "2026-07-01" || july["to"] != "2026-07-02" || july["days"] != 2 || july["full"] != 31 {
		t.Fatalf("july %+v", july)
	}
}

func dayRow(date string, followers int, score float64) map[string]any {
	return map[string]any{
		"date": date, "followers": followers, "following": 2, "posts": 3,
		"engagement": map[string]any{
			"score": score, "likes": 1, "comments": 0, "reshares": 0,
			"new_followers": 0, "impressions": 5, "backfilled": false,
		},
	}
}

func TestCustomRangeValidation(t *testing.T) {
	loc := denver(t)
	pinNow(t, time.Date(2026, 10, 22, 23, 30, 0, 0, loc))
	t.Setenv("XSTATS_TZ", "America/Denver")
	mux, ing := testDB(t)
	ing("a.json", bundle("2026-10-14", 1))
	bad := []string{
		"range=custom",
		"range=custom&from=2026-10-05",
		"range=custom&from=2026-13-01&to=2026-10-14",
		"range=custom&from=10/05/2026&to=2026-10-14",
		"range=custom&from=2026-02-31&to=2026-10-14",
		"range=custom&from=2026-10-14&to=2026-10-05",
		"range=custom&from=2026-10-05&to=2999-01-01",
		"range=decade",
		"range=Week",
	}
	for _, q := range bad {
		w := get(mux, "/api/data?"+q)
		if w.Code != 400 {
			t.Fatalf("%s: got %d %s", q, w.Code, w.Body.String())
		}
	}
	late := get(mux, "/api/data?range=custom&from=2026-10-05&to=2026-10-23")
	if late.Code != 400 || !strings.Contains(late.Body.String(), "after today (2026-10-22)") {
		t.Fatalf("future to: %d %s", late.Code, late.Body.String())
	}
	ok := get(mux, "/api/data?range=custom&from=2026-10-14&to=2026-10-14")
	if ok.Code != 200 {
		t.Fatalf("single day %d %s", ok.Code, ok.Body.String())
	}
	var root map[string]any
	if err := json.Unmarshal(ok.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if root["bucket"] != "day" || root["today"] != "2026-10-22" || root["tz"] != "America/Denver" {
		t.Fatalf("single day shape %v %v %v", root["bucket"], root["today"], root["tz"])
	}
	if get(mux, "/api/data?range=week").Code != 200 || get(mux, "/api/data").Code != 200 {
		t.Fatal("preset and legacy ranges must pass")
	}
}

func TestRangeAPIGroupsInDenver(t *testing.T) {
	loc := denver(t)
	pinNow(t, time.Date(2026, 10, 22, 23, 30, 0, 0, loc))
	t.Setenv("XSTATS_TZ", "America/Denver")
	t.Setenv("XSTATS_START_DATES", "demo:2026-09-08")
	if err := configureStartDates(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { accountStartDates = nil })

	mux, ing := testDB(t)
	// Snapshots on the 183-day and 182-day edges, plus a dense September/October
	// so week clipping and start-date hiding can be checked on real rows.
	ing("edge.json", snapBundle("2026-04-23", "demo", 500, []string{
		livePost("90001", denverStamp(t, "2026-04-23"), "example early", 1),
	}))
	// A second account with no start date covers the 182/183 thresholds.
	ing("long-a.json", snapBundle("2026-04-23", "demo_long", 10, []string{
		livePost("91001", denverStamp(t, "2026-04-23"), "example long open", 1),
	}))
	ing("long-b.json", snapBundle("2026-10-22", "demo_long", 80, []string{
		livePost("91002", denverStamp(t, "2026-10-22"), "example long close", 1),
	}))
	for d := civil("2026-09-01"); !d.After(civil("2026-10-22")); d = addCivil(d, 0, 0, 1) {
		day := d.Format("2006-01-02")
		id := "9" + strings.ReplaceAll(day, "-", "")
		ing(day+".json", snapBundle(day, "demo", 1000+d.YearDay(), []string{
			livePost(id, denverStamp(t, day), "example "+day, 1),
		}))
	}
	ing("ct.json", `{"date":"2026-10-22","accounts":{},"clickthroughs":{"date":"2026-10-20","visits":4}}`)
	ing("ct2.json", `{"date":"2026-10-22","accounts":{},"clickthroughs":{"date":"2026-04-01","visits":9}}`)

	legacy := apiRoot(t, mux)
	legacyHist := legacy["accounts"].(map[string]any)["demo"].(map[string]any)["history"].([]any)
	if legacyHist[0].(map[string]any)["date"] != "2026-09-08" {
		t.Fatalf("legacy history starts at %v", legacyHist[0].(map[string]any)["date"])
	}

	week := rangeRoot(t, mux, "range=week")
	wr := week["range"].(map[string]any)
	if wr["from"] != "2026-10-19" || wr["to"] != "2026-10-25" || wr["data_to"] != "2026-10-22" || week["bucket"] != "day" {
		t.Fatalf("week window %+v bucket %v", wr, week["bucket"])
	}
	wh := week["accounts"].(map[string]any)["demo"].(map[string]any)
	if strings.Join(historyDates(t, wh), ",") != "2026-10-19,2026-10-20,2026-10-21,2026-10-22" {
		t.Fatalf("week history %v", historyDates(t, wh))
	}
	slots, _ := wh["slots"].([]any)
	if len(slots) != 7 || slots[0] != "2026-10-19" || slots[6] != "2026-10-25" {
		t.Fatalf("week slots %v", slots)
	}

	today := rangeRoot(t, mux, "range=today")
	th := today["accounts"].(map[string]any)["demo"].(map[string]any)
	if today["today"] != "2026-10-22" || strings.Join(historyDates(t, th), ",") != "2026-10-22" {
		t.Fatalf("today %+v hist %v", today["today"], historyDates(t, th))
	}
	if postIDs(t, th)["920261022"] != true || len(postIDs(t, th)) != 1 {
		t.Fatalf("today posts %v", postIDs(t, th))
	}
	yest := rangeRoot(t, mux, "range=yesterday")
	if yest["range"].(map[string]any)["from"] != "2026-10-21" {
		t.Fatalf("yesterday %+v", yest["range"])
	}

	month := rangeRoot(t, mux, "range=month")
	mr := month["range"].(map[string]any)
	if mr["from"] != "2026-10-01" || mr["to"] != "2026-10-31" || mr["data_to"] != "2026-10-22" || month["bucket"] != "day" {
		t.Fatalf("month %+v bucket %v", mr, month["bucket"])
	}

	for _, c := range []struct{ q, bucket string }{
		{"range=custom&from=2026-09-22&to=2026-10-22", "day"},
		{"range=custom&from=2026-09-21&to=2026-10-22", "week"},
	} {
		root := rangeRoot(t, mux, c.q)
		acct := root["accounts"].(map[string]any)["demo"].(map[string]any)
		if root["bucket"] != c.bucket || acct["bucket"] != c.bucket || asInt(acct["days"]) != daysInclusive(acct["from"].(string), acct["data_to"].(string)) {
			t.Fatalf("%s: top %v account %v days %v", c.q, root["bucket"], acct["bucket"], acct["days"])
		}
	}
	long := rangeRoot(t, mux, "range=custom&from=2026-04-23&to=2026-10-22")
	if long["bucket"] != "month" || long["accounts"].(map[string]any)["demo_long"].(map[string]any)["bucket"] != "month" {
		t.Fatalf("183-day bucket top %v account %v", long["bucket"], long["accounts"].(map[string]any)["demo_long"])
	}
	// demo's start date hides April, so the days in view are the shorter clipped span.
	if long["accounts"].(map[string]any)["demo"].(map[string]any)["from"] != "2026-09-08" {
		t.Fatalf("183-day request still starts demo at %v", long["accounts"].(map[string]any)["demo"].(map[string]any)["from"])
	}
	long182 := rangeRoot(t, mux, "range=custom&from=2026-04-24&to=2026-10-22")
	if long182["bucket"] != "week" || long182["accounts"].(map[string]any)["demo_long"].(map[string]any)["bucket"] != "week" {
		t.Fatalf("182-day bucket top %v account %v", long182["bucket"], long182["accounts"].(map[string]any)["demo_long"].(map[string]any)["bucket"])
	}

	// All-time starts at the start date, later than the April snapshot.
	all := rangeRoot(t, mux, "range=all")
	ah := all["accounts"].(map[string]any)["demo"].(map[string]any)
	if ah["from"] != "2026-09-08" || ah["first"] != "2026-09-08" || ah["start"] != "2026-09-08" || ah["bucket"] != "week" {
		t.Fatalf("all-time account from %v first %v start %v bucket %v days %v", ah["from"], ah["first"], ah["start"], ah["bucket"], ah["days"])
	}
	if postIDs(t, ah)["90001"] {
		t.Fatal("post before the start date is in the all-time list")
	}
	for _, raw := range ah["history"].([]any) {
		b := raw.(map[string]any)
		if b["from"].(string) < "2026-09-08" {
			t.Fatalf("bucket includes days before the start: %+v", b)
		}
	}
	cts := all["clickthroughs"].([]any)
	if len(cts) != 1 || cts[0].(map[string]any)["date"] != "2026-10-20" {
		t.Fatalf("clickthroughs %+v", cts)
	}
	weekCT := week["clickthroughs"].([]any)
	if len(weekCT) != 1 || weekCT[0].(map[string]any)["date"] != "2026-10-20" {
		t.Fatalf("week clickthroughs %+v", weekCT)
	}

	// Custom range that asks for days before the start date stays 200 and clips.
	custom := rangeRoot(t, mux, "range=custom&from=2026-09-01&to=2026-10-22")
	ch := custom["accounts"].(map[string]any)["demo"].(map[string]any)
	if custom["bucket"] != "week" || ch["from"] != "2026-09-08" || ch["bucket"] != "week" {
		t.Fatalf("clipped custom top %v from %v bucket %v", custom["bucket"], ch["from"], ch["bucket"])
	}
	hist := ch["history"].([]any)
	first := hist[0].(map[string]any)
	if first["date"] != "2026-09-07" || first["from"] != "2026-09-08" || first["to"] != "2026-09-13" || asInt(first["days"]) != 6 || asInt(first["full"]) != 7 {
		t.Fatalf("clipped first week %+v", first)
	}
	last := hist[len(hist)-1].(map[string]any)
	if last["date"] != "2026-10-19" || last["from"] != "2026-10-19" || last["to"] != "2026-10-22" || asInt(last["days"]) != 4 || asInt(last["full"]) != 7 {
		t.Fatalf("clipped last week %+v", last)
	}
	var bucketScore float64
	for _, raw := range hist {
		bucketScore += raw.(map[string]any)["engagement"].(map[string]any)["score"].(float64)
	}
	var daily float64
	for _, raw := range legacyHist {
		row := raw.(map[string]any)
		if row["date"].(string) > "2026-10-22" {
			continue
		}
		daily += row["engagement"].(map[string]any)["score"].(float64)
	}
	if math.Abs(bucketScore-daily) > 1e-6 || bucketScore == 0 {
		t.Fatalf("bucket score %v daily %v", bucketScore, daily)
	}
	// The clipped window starts on the first visible day, so the delta has no earlier baseline.
	if ch["delta"].(map[string]any)["since_first"] != true {
		t.Fatalf("delta %+v", ch["delta"])
	}
	mid := rangeRoot(t, mux, "range=custom&from=2026-10-05&to=2026-10-14")
	mh := mid["accounts"].(map[string]any)["demo"].(map[string]any)
	if asInt(mh["followers"]) != 1000+civil("2026-10-14").YearDay() {
		t.Fatalf("followers tile %v", mh["followers"])
	}
	delta := mh["delta"].(map[string]any)
	if delta["baseline"] != "2026-10-04" {
		t.Fatalf("baseline %+v", delta)
	}
	wantDelta := (1000 + civil("2026-10-14").YearDay()) - (1000 + civil("2026-10-04").YearDay())
	if asInt(delta["followers"]) != wantDelta {
		t.Fatalf("delta %v want %d", delta["followers"], wantDelta)
	}
}

// slotsWithin fails fast if slotKeys stops advancing. A DST gap used to
// make the day step return the same date, and the loop never finished.
func slotsWithin(t *testing.T, from, to, bucket string, loc *time.Location) []string {
	t.Helper()
	ch := make(chan []string, 1)
	go func() { ch <- slotKeys(from, to, bucket, loc) }()
	select {
	case s := <-ch:
		return s
	case <-time.After(2 * time.Second):
		t.Fatalf("slotKeys %s %s..%s hung", bucket, from, to)
		return nil
	}
}

func TestSantiagoDSTSlots(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	// 2025-09-07 00:00 does not exist in Santiago. Clocks jump forward,
	// and a local-midnight parse lands on the previous evening.
	sun := civil("2025-09-07")
	if sun.Weekday() != time.Sunday || mondayIndex(sun) != 6 {
		t.Fatalf("DST day weekday %s index %d", sun.Weekday(), mondayIndex(sun))
	}
	mon := civil("2025-09-08")
	if mon.Weekday() != time.Monday || mondayIndex(mon) != 0 {
		t.Fatalf("next day weekday %s index %d", mon.Weekday(), mondayIndex(mon))
	}
	if got := addCivil(sun, 0, 0, 1).Format("2006-01-02"); got != "2025-09-08" {
		t.Fatalf("day after the gap is %s", got)
	}
	if bucketStart("2025-09-07", "week", loc) != "2025-09-01" || bucketStart("2025-09-08", "week", loc) != "2025-09-08" {
		t.Fatalf("week starts %s %s", bucketStart("2025-09-07", "week", loc), bucketStart("2025-09-08", "week", loc))
	}
	if bucketStart("2025-09-07", "month", loc) != "2025-09-01" {
		t.Fatalf("month start %s", bucketStart("2025-09-07", "month", loc))
	}
	days := slotsWithin(t, "2025-09-01", "2025-09-15", "day", loc)
	if len(days) != 15 || days[0] != "2025-09-01" || days[6] != "2025-09-07" || days[14] != "2025-09-15" {
		t.Fatalf("day slots %v", days)
	}
	weeks := slotsWithin(t, "2025-09-03", "2025-09-15", "week", loc)
	if strings.Join(weeks, ",") != "2025-09-01,2025-09-08,2025-09-15" {
		t.Fatalf("week slots %v", weeks)
	}
	months := slotsWithin(t, "2025-08-20", "2025-10-10", "month", loc)
	if strings.Join(months, ",") != "2025-08-01,2025-09-01,2025-10-01" {
		t.Fatalf("month slots %v", months)
	}
	gap := slotsWithin(t, "2025-09-07", "2025-09-09", "day", loc)
	if strings.Join(gap, ",") != "2025-09-07,2025-09-08,2025-09-09" {
		t.Fatalf("gap slots %v", gap)
	}
	now := time.Date(2025, 9, 7, 15, 0, 0, 0, loc)
	week, err := resolveRange("week", "", "", now, loc)
	if err != nil || week.From != "2025-09-01" || week.To != "2025-09-07" || week.DataTo != "2025-09-07" {
		t.Fatalf("week window %+v %v", week, err)
	}
	month, err := resolveRange("month", "", "", now, loc)
	if err != nil || month.From != "2025-09-01" || month.To != "2025-09-30" || month.DataTo != "2025-09-07" {
		t.Fatalf("month window %+v %v", month, err)
	}
}

func TestRangeClampsBeforeFirstDay(t *testing.T) {
	pinNow(t, denverAt(t, 2026, time.October, 22, 12, 0))
	t.Setenv("XSTATS_TZ", "America/Denver")
	t.Setenv("XSTATS_START_DATES", "")
	if err := configureStartDates(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { accountStartDates = nil })

	db := openTestDB(t)
	if err := ingestBytes(db, []byte(snapBundle("2026-10-20", "shown", 100, nil))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO accounts(handle,user_id,name,avatar) VALUES('blank','2','Blank','')`); err != nil {
		t.Fatal(err)
	}
	root := rangeRoot(t, newMux(db, "secret"), "range=custom&from=0001-01-01&to=2026-10-22")
	rng := root["range"].(map[string]any)
	if rng["from"] != "2026-10-20" || asInt(rng["days"]) != 3 || root["bucket"] != "day" {
		t.Fatalf("top range %+v bucket %v", rng, root["bucket"])
	}
	blank := root["accounts"].(map[string]any)["blank"].(map[string]any)
	if blank["from"] != "2026-10-20" || asInt(blank["days"]) != 3 {
		t.Fatalf("blank window from %v days %v", blank["from"], blank["days"])
	}
	slots, _ := blank["slots"].([]any)
	if len(slots) != 3 || slots[0] != "2026-10-20" || slots[2] != "2026-10-22" {
		t.Fatalf("blank slots %v", slots)
	}
	if root["accounts"].(map[string]any)["shown"].(map[string]any)["from"] != "2026-10-20" {
		t.Fatalf("shown from %v", root["accounts"].(map[string]any)["shown"].(map[string]any)["from"])
	}

	empty := openTestDB(t)
	if _, err := empty.Exec(`INSERT INTO accounts(handle,user_id,name,avatar) VALUES('blank','2','Blank','')`); err != nil {
		t.Fatal(err)
	}
	root = rangeRoot(t, newMux(empty, "secret"), "range=custom&from=0001-01-01&to=2026-10-22")
	rng = root["range"].(map[string]any)
	if rng["from"] != "2026-10-22" || asInt(rng["days"]) != 1 || root["bucket"] != "day" {
		t.Fatalf("no-snapshot range %+v bucket %v", rng, root["bucket"])
	}
	slots, _ = root["accounts"].(map[string]any)["blank"].(map[string]any)["slots"].([]any)
	if len(slots) != 1 || slots[0] != "2026-10-22" {
		t.Fatalf("no-snapshot slots %v", slots)
	}
}

func rangeRoot(t *testing.T, mux *http.ServeMux, q string) map[string]any {
	t.Helper()
	w := get(mux, "/api/data?"+q)
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
	}
	var root map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	return root
}

func denverStamp(t *testing.T, day string) string {
	t.Helper()
	d := civil(day)
	return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, denver(t)).Format(time.RFC3339)
}
