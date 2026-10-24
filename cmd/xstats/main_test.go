package main

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func bundle(date string, likes int) string {
	return `{"date":"` + date + `","accounts":{"demo":{"user":{"id":"1","username":"demo","public_metrics":{"followers_count":10}},
	"posts":[{"id":"111","text":"hello","created_at":"` + date + `T12:00:00.000Z","public_metrics":{"impression_count":5,"like_count":` + itoa(likes) + `}},
	{"id":"222","text":"RT @x: skip me","created_at":"` + date + `T12:00:00.000Z","public_metrics":{}},
	{"id":"333","text":"@a reply","created_at":"` + date + `T12:00:00.000Z","public_metrics":{}}]}}}`
}
func itoa(i int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+i))) }

func testDB(t *testing.T) (*http.ServeMux, func(string, string)) {
	dir := t.TempDir()
	db := openDB(filepath.Join(dir, "t.db"))
	ing := func(name, body string) {
		f := filepath.Join(dir, name)
		os.WriteFile(f, []byte(body), 0o600)
		if err := ingest(db, f); err != nil {
			t.Fatal(err)
		}
	}
	return newMux(db, "secret"), ing
}

func get(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestPostHistoryKeepsDailyRows(t *testing.T) {
	mux, ing := testDB(t)
	ing("a.json", bundle("2026-10-14", 1))
	ing("b.json", bundle("2026-10-15", 3))
	ing("b2.json", bundle("2026-10-15", 4)) // same day re-ingest upserts
	w := get(mux, "/api/post/111")
	if w.Code != 200 || strings.Count(w.Body.String(), `"date"`) != 2 || !strings.Contains(w.Body.String(), `"likes":4`) {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

func TestRetweetsSkippedRepliesFlagged(t *testing.T) {
	mux, ing := testDB(t)
	ing("a.json", bundle("2026-10-14", 1))
	if get(mux, "/api/post/222").Code != 404 {
		t.Fatal("retweet was stored")
	}
	if !strings.Contains(get(mux, "/api/data").Body.String(), `"is_reply":true`) {
		t.Fatal("reply not flagged")
	}
}

func TestSameDateReplacesRows(t *testing.T) {
	mux, ing := testDB(t)
	ing("a.json", bundle("2026-10-14", 1))
	ing("b.json", bundle("2026-10-15", 2))
	// Second push for 10-15 without post 111 and with new follower count.
	ing("c.json", `{"date":"2026-10-15","accounts":{"demo":{"user":{"id":"1","username":"demo","public_metrics":{"followers_count":12}},
	"posts":[{"id":"333","text":"@a reply","created_at":"2026-10-15T12:00:00.000Z","public_metrics":{"like_count":7}}]}}}`)
	h := get(mux, "/api/post/111").Body.String()
	if strings.Count(h, `"date"`) != 1 || strings.Contains(h, "2026-10-15") {
		t.Fatalf("stale row kept: %s", h)
	}
	d := get(mux, "/api/data").Body.String()
	if strings.Count(d, `"date":"2026-10-15"`) != 1 || !strings.Contains(d, `"followers":12`) {
		t.Fatalf("follower row not replaced: %s", d)
	}
	// Empty posts list keeps existing rows.
	ing("d.json", `{"date":"2026-10-15","accounts":{"demo":{"user":{"id":"1","username":"demo","public_metrics":{"followers_count":13}},"posts":[]}}}`)
	if !strings.Contains(get(mux, "/api/post/333").Body.String(), `"likes":7`) {
		t.Fatal("empty push wiped rows")
	}
}

func TestAccountOrder(t *testing.T) {
	mux, ing := testDB(t)
	ing("a.json", bundle("2026-10-14", 1))
	t.Setenv("XSTATS_ACCOUNTS", "@Demo,missing")
	if !strings.Contains(get(mux, "/api/data").Body.String(), `"order":["demo"]`) {
		t.Fatal("order not applied")
	}
}

func TestBadIDAndIngestAuth(t *testing.T) {
	mux, _ := testDB(t)
	if get(mux, "/api/post/abc").Code != 400 {
		t.Fatal("expected 400")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/ingest", strings.NewReader("{}")))
	if w.Code != 401 {
		t.Fatalf("ingest without token: %d", w.Code)
	}
}

func denver(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestPostDay(t *testing.T) {
	loc := denver(t)
	d, ok := postDay("2026-10-14T12:00:00.000Z", loc)
	if !ok || d != "2026-10-14" {
		t.Fatalf("fractional Z: %q %v", d, ok)
	}
	// 20:00 at UTC-7 is 21:00 MDT the same calendar day, not the next UTC day.
	d, ok = postDay("2026-09-03T20:00:00-07:00", loc)
	if !ok || d != "2026-09-03" {
		t.Fatalf("offset: %q", d)
	}
	// 23:30 Mountain Time is 05:30 UTC the next morning, and stays on the MT day.
	d, ok = postDay("2026-10-15T05:30:00Z", loc)
	if !ok || d != "2026-10-14" {
		t.Fatalf("23:30 MT via Z: %q", d)
	}
	d, ok = postDay("2026-10-14T23:30:00-06:00", loc)
	if !ok || d != "2026-10-14" {
		t.Fatalf("23:30 MT offset: %q", d)
	}
	// A date with no clock is that calendar day. UTC midnight would be the
	// previous evening in Denver, and must not be shifted.
	d, ok = postDay("2026-10-15", loc)
	if !ok || d != "2026-10-15" {
		t.Fatalf("date-only: %q", d)
	}
	if _, ok = postDay("", loc); ok {
		t.Fatal("empty created_at should not bucket")
	}
	if _, ok = postDay("not-a-date", loc); ok {
		t.Fatal("garbage created_at should not bucket")
	}
}

func TestStatsLocation(t *testing.T) {
	t.Setenv("XSTATS_TZ", "")
	loc, err := statsLocation()
	if err != nil || loc.String() != "America/Denver" {
		t.Fatalf("default: %v %v", loc, err)
	}
	t.Setenv("XSTATS_TZ", "UTC")
	loc, err = statsLocation()
	if err != nil || loc.String() != "UTC" {
		t.Fatalf("override: %v %v", loc, err)
	}
	d, ok := postDay("2026-10-15T05:30:00Z", loc)
	if !ok || d != "2026-10-15" {
		t.Fatalf("utc bucket %q", d)
	}
	t.Setenv("XSTATS_TZ", "America/Denver")
	loc, err = statsLocation()
	if err != nil || loc.String() != "America/Denver" {
		t.Fatalf("denver after utc: %v %v", loc, err)
	}
	d, ok = postDay("2026-10-15T05:30:00Z", loc)
	if !ok || d != "2026-10-14" {
		t.Fatalf("cached zone? %q", d)
	}
	t.Setenv("XSTATS_TZ", "Not/AZone")
	if _, err := statsLocation(); err == nil {
		t.Fatal("invalid zone should fail")
	}
}

func TestBadZoneFailsAPI(t *testing.T) {
	t.Setenv("XSTATS_TZ", "Not/AZone")
	mux, _ := testDB(t)
	if get(mux, "/api/data").Code != 500 {
		t.Fatal("invalid XSTATS_TZ should fail /api/data")
	}
}

func TestAggregateDays(t *testing.T) {
	hist := []histDay{
		{Date: "2026-09-01", Followers: 100},
		{Date: "2026-09-02", Followers: 90},
		{Date: "2026-09-03", Followers: 90},
		{Date: "2026-09-04", Followers: 90},
		{Date: "2026-09-10", Followers: 90},
	}
	posts := []scoredPost{
		// First day. new_followers must be 0 (no prior snapshot), not omitted.
		// Impressions are not 1000, so a rate would not equal the points total.
		{ID: "p1", Text: "first", CreatedAt: "2026-09-01T12:00:00.000Z", Impressions: ip(2500), Likes: 50},
		// Zero impressions still scores. The follower drop makes the total negative.
		{ID: "p2", Text: "quiet", CreatedAt: "2026-09-02T12:00:00Z", Impressions: ip(0), Likes: 4, Replies: ip(1)},
		{ID: "p3", Text: "scored", CreatedAt: "2026-09-03T12:00:00Z", Impressions: ip(2000), Likes: 10},
		// 05:30Z on Sep 5 is 23:30 MT on Sep 4, so this stays on the Sep 4 snapshot.
		{ID: "p4", Text: "offset", CreatedAt: "2026-09-05T05:30:00Z", Impressions: ip(500), Likes: 5},
		{ID: "skip", Text: "bad", CreatedAt: "", Impressions: ip(99999), Likes: 99999},
		// After tracking has started, a post on a day with no snapshot is still left out.
		{ID: "old", Text: "unaligned", CreatedAt: "2026-09-06T12:00:00Z", Impressions: ip(99999), Likes: 99999},
		{ID: "p5", Text: "latest", CreatedAt: "2026-09-10T12:00:00Z", Impressions: ip(4000), Likes: 16},
	}
	loc := denver(t)
	got := aggregateDays(hist, posts, "2026-09-10", loc)
	if len(got) != len(hist) {
		t.Fatalf("len %d", len(got))
	}

	d0 := got[0]
	if !nfIs(d0.NewFollowers, 0) || d0.Score == nil || *d0.Score != 50 || d0.Points != 50 || d0.InProgress || d0.Backfilled {
		t.Fatalf("first day: %+v score %v", d0, scoreOr(d0.Score))
	}
	if d0.ScoreDelta != nil {
		t.Fatal("first scored day has no prior week")
	}

	d1 := got[1]
	wantQuiet := dayLevelPoints(1, -10, 0, 4)
	if !nfIs(d1.NewFollowers, -10) || !imprIs(d1.Impressions, 0) || d1.Score == nil || *d1.Score != float64(wantQuiet) || d1.Points != wantQuiet {
		t.Fatalf("zero impressions: %+v score %v", d1, scoreOr(d1.Score))
	}
	if d1.ScoreDelta == nil || math.Abs(*d1.ScoreDelta-float64(wantQuiet-50)) > 1e-9 {
		t.Fatalf("zero-impression delta %v", scoreOr(d1.ScoreDelta))
	}

	// 09-10's prior window is [09-03, 09-10). 09-01 (50) and 09-02 are outside it.
	// Average of 09-03 (10) and 09-04 (5) is 7.5; delta is 8.5.
	last := got[4]
	if !last.InProgress || last.Score == nil || *last.Score != 16 {
		t.Fatalf("today: %+v score %v", last, scoreOr(last.Score))
	}
	if last.ScoreDelta == nil || math.Abs(*last.ScoreDelta-8.5) > 1e-9 {
		t.Fatalf("delta %v", scoreOr(last.ScoreDelta))
	}
	if got[2].InProgress || got[3].Date != "2026-09-04" || got[3].Likes != 5 || !imprIs(got[3].Impressions, 500) {
		t.Fatalf("zone bucket: %+v %+v", got[2], got[3])
	}
	if got[2].Score == nil || *got[2].Score != 10 || got[2].Points != 10 || got[3].Score == nil || *got[3].Score != 5 || got[3].Points != 5 {
		t.Fatal("window inputs")
	}
	// 09-01 must not have been pulled into 09-10.
	if *last.ScoreDelta == 16-50 {
		t.Fatal("delta used the day outside the 7-day window")
	}
}

func TestTopPostTieBreak(t *testing.T) {
	hist := []histDay{{Date: "2026-10-01", Followers: 5}}
	loc := denver(t)
	higherImp := aggregateDays(hist, []scoredPost{
		{ID: "1", Text: "low", CreatedAt: "2026-10-01T18:00:00Z", Impressions: ip(100), Likes: 10},
		{ID: "2", Text: "high", CreatedAt: "2026-10-01T18:00:00Z", Impressions: ip(200), Likes: 10},
	}, "2026-01-01", loc)
	if higherImp[0].Top == nil || higherImp[0].Top.ID != "2" || higherImp[0].Top.Points != 10 {
		t.Fatalf("impressions tie-break: %+v", higherImp[0].Top)
	}
	lowerID := aggregateDays(hist, []scoredPost{
		{ID: "200", Text: "b", CreatedAt: "2026-10-01T18:00:00Z", Impressions: ip(100), Likes: 10},
		{ID: "100", Text: "a", CreatedAt: "2026-10-01T18:00:00Z", Impressions: ip(100), Likes: 10},
	}, "2026-01-01", loc)
	if lowerID[0].Top == nil || lowerID[0].Top.ID != "100" {
		t.Fatalf("id tie-break: %+v", lowerID[0].Top)
	}
	// A known 0 beats an omitted count. Treating nil as 0 would pick the lower id.
	knownZero := aggregateDays(hist, []scoredPost{
		{ID: "1", Text: "unknown", CreatedAt: "2026-10-01T18:00:00Z", Impressions: nil, Likes: 1},
		{ID: "2", Text: "zero", CreatedAt: "2026-10-01T18:00:00Z", Impressions: ip(0), Likes: 1},
	}, "2026-01-01", loc)
	if knownZero[0].Top == nil || knownZero[0].Top.ID != "2" {
		t.Fatalf("nil vs zero tie-break: %+v", knownZero[0].Top)
	}
}

func TestNegativeScore(t *testing.T) {
	hist := []histDay{{Date: "2026-10-01", Followers: 100}, {Date: "2026-10-02", Followers: 90}}
	posts := []scoredPost{{ID: "1", CreatedAt: "2026-10-02T18:00:00Z", Impressions: ip(500)}}
	got := aggregateDays(hist, posts, "2026-01-01", denver(t))
	wantPts := dayLevelPoints(0, -10, 0, 0)
	if !nfIs(got[1].NewFollowers, -10) || got[1].Points != wantPts || got[1].Score == nil || *got[1].Score != float64(wantPts) {
		t.Fatalf("negative: %+v score %v", got[1], scoreOr(got[1].Score))
	}
	// impressions/1000 would have turned -40 into -80.
	if *got[1].Score == float64(wantPts)*1000/500 {
		t.Fatal("negative score was divided by impressions")
	}
}

func scoreOr(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func ip(n int) *int { return &n }

func imprIs(p *int, n int) bool { return p != nil && *p == n }

func nfIs(p *int, n int) bool { return p != nil && *p == n }

func TestNullImpressionsExcludedFromTotal(t *testing.T) {
	hist := []histDay{{Date: "2026-10-01", Followers: 10}}
	loc := denver(t)
	mixed := aggregateDays(hist, []scoredPost{
		{ID: "a", CreatedAt: "2026-10-01T18:00:00Z", Impressions: ip(10), Likes: 1},
		{ID: "b", CreatedAt: "2026-10-01T18:00:00Z", Impressions: nil, Likes: 1},
	}, "2026-01-01", loc)
	if !imprIs(mixed[0].Impressions, 10) || mixed[0].Score == nil || *mixed[0].Score != 2 {
		t.Fatalf("mixed total %+v score %v", mixed[0].Impressions, scoreOr(mixed[0].Score))
	}
	none := aggregateDays(hist, []scoredPost{
		{ID: "c", CreatedAt: "2026-10-01T18:00:00Z", Impressions: nil, Likes: 3},
	}, "2026-01-01", loc)
	if none[0].Impressions != nil || none[0].Likes != 3 || none[0].Score == nil || *none[0].Score != 3 {
		t.Fatalf("all unknown %+v score %v", none[0].Impressions, scoreOr(none[0].Score))
	}
	zero := aggregateDays(hist, []scoredPost{
		{ID: "d", CreatedAt: "2026-10-01T18:00:00Z", Impressions: ip(0)},
		{ID: "e", CreatedAt: "2026-10-01T18:00:00Z", Impressions: nil},
	}, "2026-01-01", loc)
	if !imprIs(zero[0].Impressions, 0) {
		t.Fatalf("explicit zero %+v", zero[0].Impressions)
	}
	empty := aggregateDays(hist, nil, "2026-01-01", loc)
	if !imprIs(empty[0].Impressions, 0) {
		t.Fatalf("day with no posts %+v", empty[0].Impressions)
	}
}

func apiRoot(t *testing.T, mux *http.ServeMux) map[string]any {
	t.Helper()
	w := get(mux, "/api/data")
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	var root map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	return root
}

func histEng(t *testing.T, root map[string]any, acct, date string) (map[string]any, map[string]any) {
	t.Helper()
	a := root["accounts"].(map[string]any)[acct].(map[string]any)
	for _, raw := range a["history"].([]any) {
		row := raw.(map[string]any)
		if row["date"] == date {
			e, _ := row["engagement"].(map[string]any)
			if e == nil {
				t.Fatalf("no engagement on %s", date)
			}
			return row, e
		}
	}
	t.Fatalf("missing %s/%s", acct, date)
	return nil, nil
}

func asInt(v any) int { return int(v.(float64)) }

func asF(t *testing.T, v any) (float64, bool) {
	t.Helper()
	if v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("not a number: %T %v", v, v)
	}
	return f, true
}

// TestEngagementIngest covers reshares = retweets + quotes, follower deltas,
// zero impressions, latest-snapshot aggregation (not a sum across days), and
// today's in-progress re-push.
func TestEngagementIngest(t *testing.T) {
	mux, ing := testDB(t)
	loc, err := statsLocation()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(loc)
	d0 := now.AddDate(0, 0, -2).Format("2006-01-02")
	d1 := now.AddDate(0, 0, -1).Format("2006-01-02")
	d2 := now.Format("2006-01-02")
	user := func(followers, following, tweets int) string {
		return `{"id":"1","username":"demo","name":"Demo","public_metrics":{"followers_count":` +
			itoaN(followers) + `,"following_count":` + itoaN(following) + `,"tweet_count":` + itoaN(tweets) + `}}`
	}
	ing("d0.json", `{"date":"`+d0+`","accounts":{"demo":{"user":`+user(40, 1, 2)+`,"posts":[
		{"id":"1","text":"silent","created_at":"`+d0+`T12:00:00.000Z","public_metrics":{"impression_count":0,"like_count":4,"reply_count":1,"retweet_count":0,"quote_count":0}}
	]},"other":{"user":{"id":"2","username":"other","public_metrics":{"followers_count":9}},"posts":[
		{"id":"9","text":"not demo","created_at":"`+d0+`T12:00:00.000Z","public_metrics":{"impression_count":99999,"like_count":999,"reply_count":999,"retweet_count":50,"quote_count":50}}
	]}}}`)
	ing("d1.json", `{"date":"`+d1+`","accounts":{"demo":{"user":`+user(50, 1, 3)+`,"posts":[
		{"id":"2","text":"main post","created_at":"`+d1+`T12:00:00.000Z","public_metrics":{"impression_count":2000,"like_count":10,"reply_count":1,"retweet_count":3,"quote_count":4}},
		{"id":"3","text":"other post","created_at":"`+d1+`T12:00:00.000Z","public_metrics":{"impression_count":500,"like_count":30,"reply_count":0,"retweet_count":0,"quote_count":0}}
	]}}}`)

	root := apiRoot(t, mux)
	wts := root["engagement_weights"].(map[string]any)
	if asInt(wts["comments"]) != 3 || asInt(wts["new_followers"]) != 4 || asInt(wts["reshares"]) != 2 || asInt(wts["likes"]) != 1 {
		t.Fatalf("weights %v", wts)
	}
	row0, e0 := histEng(t, root, "demo", d0)
	if asInt(row0["followers"]) != 40 || asInt(row0["following"]) != 1 || asInt(row0["posts"]) != 2 {
		t.Fatalf("history fields changed: %v", row0)
	}
	// First snapshot: new followers are 0. Zero impressions still score the points total.
	if asInt(e0["new_followers"]) != 0 || asInt(e0["impressions"]) != 0 || asInt(e0["score"]) != 7 || asInt(e0["points"]) != 7 || e0["in_progress"] == true {
		t.Fatalf("d0 %+v", e0)
	}
	if e0["top_post"].(map[string]any)["id"] != "1" {
		t.Fatalf("d0 top %v", e0["top_post"])
	}
	_, other := histEng(t, root, "other", d0)
	if asInt(other["impressions"]) != 99999 {
		t.Fatalf("other account %v", other)
	}

	_, e1 := histEng(t, root, "demo", d1)
	// reshares = 3 retweets + 4 quotes. other post adds likes only.
	if asInt(e1["reshares"]) != 7 || asInt(e1["comments"]) != 1 || asInt(e1["likes"]) != 40 || asInt(e1["impressions"]) != 2500 || asInt(e1["new_followers"]) != 10 {
		t.Fatalf("d1 parts %+v", e1)
	}
	if asInt(e1["points"]) != 97 {
		t.Fatalf("d1 points %v", e1["points"])
	}
	sc, ok := asF(t, e1["score"])
	if !ok || sc != 97 || asInt(e1["points"]) != 97 {
		t.Fatalf("d1 score %v points %v", e1["score"], e1["points"])
	}
	d1delta, ok := asF(t, e1["score_delta"])
	if !ok || math.Abs(d1delta-(97-7)) > 1e-9 || e1["in_progress"] == true {
		t.Fatalf("d1 flags %+v", e1)
	}
	top := e1["top_post"].(map[string]any)
	// post 3 has 30 post-level points; post 2 has 3*1 + 2*7 + 10 = 27.
	if top["id"] != "3" || asInt(top["points"]) != 30 || top["url"] != "https://x.com/demo/status/3" {
		t.Fatalf("top %+v", top)
	}

	// Later snapshot of post 2 must replace, not add to, the creation-day total.
	ing("d2.json", `{"date":"`+d2+`","accounts":{"demo":{"user":`+user(53, 2, 4)+`,"posts":[
		{"id":"2","text":"main post","created_at":"`+d1+`T12:00:00.000Z","public_metrics":{"impression_count":4000,"like_count":20,"reply_count":2,"retweet_count":3,"quote_count":4}},
		{"id":"4","text":"today post","created_at":"`+d2+`T18:00:00.000Z","public_metrics":{"impression_count":1000,"like_count":5,"reply_count":0,"retweet_count":1,"quote_count":1}}
	]}}}`)
	root = apiRoot(t, mux)
	_, e1 = histEng(t, root, "demo", d1)
	if asInt(e1["impressions"]) != 4500 || asInt(e1["likes"]) != 50 || asInt(e1["comments"]) != 2 || asInt(e1["reshares"]) != 7 {
		t.Fatalf("latest snapshot not used: %+v", e1)
	}
	if asInt(e1["points"]) != 110 || asInt(e1["score"]) != 110 {
		t.Fatalf("d1 points after update %v", e1["points"])
	}
	if e1["top_post"].(map[string]any)["id"] != "2" {
		t.Fatalf("top after update %v", e1["top_post"])
	}
	_, e2 := histEng(t, root, "demo", d2)
	if e2["in_progress"] != true {
		t.Fatalf("today should be in progress: %+v", e2)
	}
	if asInt(e2["new_followers"]) != 3 || asInt(e2["reshares"]) != 2 || asInt(e2["impressions"]) != 1000 || asInt(e2["points"]) != 21 {
		t.Fatalf("today %+v", e2)
	}
	sc, ok = asF(t, e2["score"])
	if !ok || sc != 21 || asInt(e2["points"]) != 21 {
		t.Fatalf("today score %v", e2["score"])
	}
	delta, ok := asF(t, e2["score_delta"])
	wantDelta := 21 - (7.0+110)/2
	if !ok || math.Abs(delta-wantDelta) > 1e-6 {
		t.Fatalf("today delta %v want %v", e2["score_delta"], wantDelta)
	}
	if e2["top_post"].(map[string]any)["url"] != "https://x.com/demo/status/4" {
		t.Fatalf("today top %v", e2["top_post"])
	}

	// Re-pushing today replaces the row. Impressions must be 1500, not 1000+1500.
	ing("d2b.json", `{"date":"`+d2+`","accounts":{"demo":{"user":`+user(58, 2, 4)+`,"posts":[
		{"id":"2","text":"main post","created_at":"`+d1+`T12:00:00.000Z","public_metrics":{"impression_count":4000,"like_count":20,"reply_count":2,"retweet_count":3,"quote_count":4}},
		{"id":"4","text":"today post","created_at":"`+d2+`T18:00:00.000Z","public_metrics":{"impression_count":1500,"like_count":9,"reply_count":0,"retweet_count":1,"quote_count":1}}
	]}}}`)
	root = apiRoot(t, mux)
	row2, e2 := histEng(t, root, "demo", d2)
	if asInt(row2["followers"]) != 58 || asInt(row2["following"]) != 2 || asInt(row2["posts"]) != 4 {
		t.Fatalf("re-push history %v", row2)
	}
	if e2["in_progress"] != true || asInt(e2["impressions"]) != 1500 || asInt(e2["new_followers"]) != 8 || asInt(e2["points"]) != 45 {
		t.Fatalf("re-push today %+v", e2)
	}
	sc, ok = asF(t, e2["score"])
	if !ok || sc != 45 || asInt(e2["points"]) != 45 {
		t.Fatalf("re-push score %v points %v", e2["score"], e2["points"])
	}
	_, e1 = histEng(t, root, "demo", d1)
	if asInt(e1["impressions"]) != 4500 {
		t.Fatalf("re-push changed an older day: %v", e1["impressions"])
	}
}

func TestMigrateKeepsImpressionValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Schema as created before this change, including the later is_reply column.
	if _, err := raw.Exec(`CREATE TABLE post_metrics(post_id TEXT, account TEXT, date TEXT, text TEXT, created_at TEXT, impressions INT, likes INT, replies INT, reposts INT, is_reply INT DEFAULT 0, PRIMARY KEY(post_id,date))`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO post_metrics(post_id,account,date,text,created_at,impressions,likes,replies,reposts,is_reply) VALUES
		('z','demo','2020-01-01','real zero','2020-01-01T12:00:00Z',0,1,0,0,0),
		('p','demo','2020-01-01','some views','2020-01-01T12:00:00Z',120,4,1,2,0),
		('r','demo','2020-01-01','@someone hi','2020-01-01T12:00:00Z',7,0,0,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db := openDB(path)
	defer db.Close()
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM post_metrics`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 3 {
		t.Fatalf("row count %d", rows)
	}
	check := func(id string, want sql.NullInt64, reply int) {
		t.Helper()
		var got sql.NullInt64
		var ir int
		if err := db.QueryRow(`SELECT impressions, is_reply FROM post_metrics WHERE post_id=?`, id).Scan(&got, &ir); err != nil {
			t.Fatal(err)
		}
		if got.Valid != want.Valid || got.Int64 != want.Int64 || ir != reply {
			t.Fatalf("%s impressions %+v reply %d, want %+v reply %d", id, got, ir, want, reply)
		}
	}
	check("z", sql.NullInt64{Int64: 0, Valid: true}, 0)
	check("p", sql.NullInt64{Int64: 120, Valid: true}, 0)
	check("r", sql.NullInt64{Int64: 7, Valid: true}, 1)

	var notnull int
	if err := db.QueryRow(`SELECT "notnull" FROM pragma_table_info('post_metrics') WHERE name='impressions'`).Scan(&notnull); err != nil {
		t.Fatal(err)
	}
	if notnull != 0 {
		t.Fatal("impressions became NOT NULL")
	}
	if _, err := db.Exec(`INSERT INTO post_metrics(post_id,account,date,text,created_at,impressions,likes,replies,reposts) VALUES('n','demo','2020-01-02','omitted','2020-01-02T12:00:00Z',NULL,0,0,0)`); err != nil {
		t.Fatal(err)
	}
	check("n", sql.NullInt64{}, 0)
	if err := db.QueryRow(`SELECT COUNT(*) FROM post_metrics`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 4 {
		t.Fatalf("row count after null insert %d", rows)
	}
}

func TestIngestImpressionsNullVsZero(t *testing.T) {
	mux, ing := testDB(t)
	loc, err := statsLocation()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(loc)
	d0 := now.AddDate(0, 0, -2).Format("2006-01-02")
	d1 := now.AddDate(0, 0, -1).Format("2006-01-02")
	d2 := now.Format("2006-01-02")
	stamp := func(day string, hour int) string {
		t.Helper()
		d, err := time.ParseInLocation("2006-01-02", day, loc)
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, loc).Format(time.RFC3339)
	}
	user := `{"id":"1","username":"demo","name":"Demo","public_metrics":{"followers_count":10,"following_count":1,"tweet_count":1}}`
	ing("d0.json", `{"date":"`+d0+`","accounts":{"demo":{"user":`+user+`,"posts":[
		{"id":"10","text":"archive post","created_at":"`+stamp(d0, 12)+`","public_metrics":{"like_count":2,"reply_count":0,"retweet_count":0,"quote_count":0}}
	]}}}`)
	ing("d1.json", `{"date":"`+d1+`","accounts":{"demo":{"user":`+user+`,"posts":[
		{"id":"11","text":"zero views","created_at":"`+stamp(d1, 12)+`","public_metrics":{"impression_count":0,"like_count":1,"reply_count":0,"retweet_count":0,"quote_count":0}},
		{"id":"12","text":"also unknown","created_at":"`+stamp(d1, 15)+`","public_metrics":{"like_count":1,"reply_count":0,"retweet_count":0,"quote_count":0}}
	]}}}`)
	ing("d2.json", `{"date":"`+d2+`","accounts":{"demo":{"user":`+user+`,"posts":[
		{"id":"13","text":"known","created_at":"`+stamp(d2, 12)+`","public_metrics":{"impression_count":10,"like_count":1,"reply_count":0,"retweet_count":0,"quote_count":0}},
		{"id":"14","text":"omitted","created_at":"`+stamp(d2, 15)+`","public_metrics":{"like_count":4,"reply_count":0,"retweet_count":0,"quote_count":0}}
	]}}}`)

	missing := get(mux, "/api/post/10").Body.String()
	if !strings.Contains(missing, `"impressions":null`) {
		t.Fatalf("missing count stored as non-null: %s", missing)
	}
	zero := get(mux, "/api/post/11").Body.String()
	if !strings.Contains(zero, `"impressions":0`) || strings.Contains(zero, `"impressions":null`) {
		t.Fatalf("explicit zero: %s", zero)
	}

	root := apiRoot(t, mux)
	_, e0 := histEng(t, root, "demo", d0)
	if _, ok := e0["impressions"]; !ok || e0["impressions"] != nil {
		t.Fatalf("all-unknown total %#v", e0["impressions"])
	}
	_, e1 := histEng(t, root, "demo", d1)
	if e1["impressions"] == nil || asInt(e1["impressions"]) != 0 {
		t.Fatalf("zero plus unknown %#v", e1["impressions"])
	}
	_, e2 := histEng(t, root, "demo", d2)
	if asInt(e2["impressions"]) != 10 {
		t.Fatalf("null not excluded from total: %#v", e2["impressions"])
	}
	posts := root["accounts"].(map[string]any)["demo"].(map[string]any)["posts"].([]any)
	byID := map[string]any{}
	for _, rawp := range posts {
		p := rawp.(map[string]any)
		byID[p["id"].(string)] = p["impressions"]
	}
	if byID["10"] != nil || byID["12"] != nil || byID["14"] != nil {
		t.Fatalf("list nulls %#v", byID)
	}
	if asInt(byID["11"]) != 0 || asInt(byID["13"]) != 10 {
		t.Fatalf("list values %#v", byID)
	}

	// Re-pushing without the field replaces a previous count with NULL.
	ing("d2b.json", `{"date":"`+d2+`","accounts":{"demo":{"user":`+user+`,"posts":[
		{"id":"13","text":"known","created_at":"`+stamp(d2, 12)+`","public_metrics":{"like_count":1,"reply_count":0,"retweet_count":0,"quote_count":0}}
	]}}}`)
	again := get(mux, "/api/post/13").Body.String()
	if !strings.Contains(again, `"impressions":null`) || strings.Contains(again, `"impressions":10`) {
		t.Fatalf("re-push history: %s", again)
	}
	root = apiRoot(t, mux)
	_, e2 = histEng(t, root, "demo", d2)
	if e2["impressions"] != nil {
		t.Fatalf("re-push total %#v", e2["impressions"])
	}
}

func itoaN(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestPageMarksEngagement(t *testing.T) {
	mux, _ := testDB(t)
	body := get(mux, "/").Body.String()
	for _, s := range []string{"Engagement score", "--bar:#a371f7", "--bar:#8250df", "No snapshot", "grid-template-columns:1fr auto", ">points<"} {
		if !strings.Contains(body, s) {
			t.Fatalf("page missing %q", s)
		}
	}
	if strings.Contains(body, "No bar: this day has 0 impressions.") {
		t.Fatal("zero-impressions explanation must not ship")
	}
	if !strings.Contains(body, "followers not tracked yet") || !strings.Contains(body, "data-bf") {
		t.Fatal("backfilled hover note or muted bar marker missing")
	}
	if strings.Contains(body, "e.impressions?compact(e.impressions):'0'") {
		t.Fatal("missing impressions still render as 0")
	}
	if !strings.Contains(body, "e.impressions==null?'-':compact(e.impressions)") {
		t.Fatal("null impressions should render as a dash")
	}
	if !strings.Contains(body, "last[k]==null||prev[k]==null") {
		t.Fatal("null metric delta should not be subtracted as zero")
	}
	if strings.Contains(body, "Example data") || strings.Contains(body, "EXAMPLE DATA") {
		t.Fatal("example-data label must not ship")
	}
	if strings.Contains(body, "1fr auto auto") || strings.Contains(body, "wt*n") || strings.Contains(body, "\\u00d7${") {
		t.Fatal("hover card still shows the per-category formula")
	}
}

// The posts table shows one page of 25, newest first, for the selected range.
// The first view is page 1 with a Show more button when another page exists.
// Show more hides that button, opens page 2, and switches to page numbers.
// Each number replaces the rows with that page. A new range, account, or
// replies toggle goes through render, which returns to page 1 and Show more.
func TestPostsShowMore(t *testing.T) {
	body := get(newMux(openTestDB(t), "secret"), "/").Body.String()
	for _, s := range []string{
		`const POST_PAGE=25`,
		`let postPage=0,postPaged=false`,
		`(a.posts||[]).filter(p=>inc||!p.is_reply)`,
		`const total=shown.length,pages=Math.max(1,Math.ceil(total/POST_PAGE))`,
		`const from=postPage*POST_PAGE,n=Math.min(from+POST_PAGE,total)`,
		`const rows=shown.slice(from,n)`,
		`const span=from&&n>from+1?fmt(from+1)+'-'+fmt(n):fmt(n)`,
		`note='Showing '+span+' of '+fmt(total)+' posts. '+note`,
		`$('rows').innerHTML=html||'<tr><td><div class="empty"`,
		`const paging=postPaged&&pages>1`,
		`$('more').hidden=paging||n>=total`,
		`pgs.hidden=!paging`,
		`pageList(pages,postPage+1)`,
		`aria-current="page"`,
		`data-page=`,
		`if(pages>7&&!edge&&!near&&!head&&!tail)`,
		`'\u2026'`,
		`postPage=0;postPaged=false;paintPosts(a)`,
		`$('more').onclick=()=>{if(!D||!D.accounts[cur])return;postPaged=true;postPage=1;paintPosts(D.accounts[cur])}`,
		`$('pgs').onclick=e=>{const b=e.target.closest('[data-page]');if(!b||!D||!D.accounts[cur])return;const n=+b.dataset.page-1;if(n===postPage)return;postPage=n;paintPosts(D.accounts[cur])}`,
		`<button type="button" class="mor" id="more" hidden>Show more</button>`,
		`<nav class="pgs" id="pgs" hidden aria-label="Posts pages"></nav>`,
		`.mor{margin-top:14px;height:40px;border-radius:10px;border:1px solid var(--bd);background:var(--sw);color:var(--fg);font:inherit;font-size:14px;font-weight:600;padding:0 18px;cursor:pointer}`,
		`.mor:hover,.mor:focus-visible{border-color:var(--acc);outline:none}`,
		`.mor[hidden]{display:none}`,
		`.mor{display:block;width:100%}`,
		`.pgs{margin-top:14px;display:flex;flex-wrap:wrap;gap:6px;align-items:center}`,
		`.pgs[hidden]{display:none}`,
		`.pg{height:40px;min-width:40px;border-radius:10px;border:1px solid var(--bd);background:var(--sw);color:var(--fg);font:inherit;font-size:14px;font-weight:600;padding:0 12px;cursor:pointer}`,
		`.pg:hover,.pg:focus-visible{border-color:var(--acc);outline:none}`,
		`.pg[aria-current=page]{border-color:var(--acc);background:color-mix(in srgb,var(--acc) 18%,transparent);color:var(--acc);cursor:default}`,
		`$('sw').onchange=e=>{closePicker(false);cur=e.target.value;localStorage.setItem('acct',cur);const u=pageURL();history.replaceState(null,'',u);render()}`,
		`$('incr').onchange=e=>{localStorage.setItem('incReplies',e.target.checked?'1':'0');render()}`,
		`function applyRange(k){closePicker(true);rg=k;load()}`,
		`function zoomTo(f,t){hideTip();CR={from:f,to:t};rg='custom';histMode='push';load()}`,
		`D=await r.json();bootHandles();saveRange();render()`,
		`saveRange();bootHandles();render()}`,
	} {
		if !strings.Contains(body, s) {
			t.Fatalf("page missing %q", s)
		}
	}
	more := body[strings.Index(body, "$('more').onclick"):]
	more = more[:strings.Index(more, "};")]
	if strings.Contains(more, "render()") {
		t.Fatal("show more calls render and would reset the page")
	}
	pages := body[strings.Index(body, "$('pgs').onclick"):]
	pages = pages[:strings.Index(pages, "};")]
	if strings.Contains(pages, "render()") || strings.Contains(pages, "postPaged=false") {
		t.Fatal("page numbers reset the Show more state")
	}
	if strings.Contains(body, "insertAdjacentHTML") || strings.Contains(body, "postLimit") {
		t.Fatal("show more still appends pages")
	}
	if strings.Contains(body, "most recent") || strings.Contains(body, "const cap=") {
		t.Fatal("12-post cap still in the page")
	}
}

// 05:30 UTC is still the previous Mountain Time evening. That post belongs on
// today's Denver snapshot, and the next UTC date is not in progress.
func TestEveningMountainTime(t *testing.T) {
	loc := denver(t)
	instant := time.Date(2026, 10, 15, 5, 30, 0, 0, time.UTC)
	today := instant.In(loc).Format("2006-01-02")
	if today != "2026-10-14" {
		t.Fatalf("denver day of 05:30Z %s", today)
	}
	hist := []histDay{{Date: "2026-10-14", Followers: 10}, {Date: "2026-10-15", Followers: 12}}
	posts := []scoredPost{{ID: "eve", Text: "late", CreatedAt: "2026-10-15T05:30:00Z", Impressions: ip(250), Likes: 3}}
	got := aggregateDays(hist, posts, today, loc)
	if !got[0].InProgress || got[0].Likes != 3 || got[0].Score == nil || *got[0].Score != 3 || got[0].Points != 3 {
		t.Fatalf("evening post %+v score %v", got[0], scoreOr(got[0].Score))
	}
	// The next snapshot has no posts. Its score is the follower change only.
	if got[1].InProgress || got[1].Likes != 0 || !imprIs(got[1].Impressions, 0) || got[1].Score == nil || *got[1].Score != 8 || got[1].Points != 8 {
		t.Fatalf("next utc day %+v score %v", got[1], scoreOr(got[1].Score))
	}
}

// Posts before the first snapshot extend the chart. Their follower change is
// unknown, the score has no follower term, and they are not part of any
// 7-day average. The first snapshot still starts at new_followers 0.
func TestBackfilledDaysBeforeFirstSnapshot(t *testing.T) {
	loc := denver(t)
	hist := []histDay{
		{Date: "2026-10-10", Followers: 100},
		{Date: "2026-10-11", Followers: 110},
		{Date: "2026-10-18", Followers: 112},
	}
	posts := []scoredPost{
		// 05:30Z is still Oct 7 in Denver, the same bucketing as snapshot days.
		{ID: "eve", Text: "evening", CreatedAt: "2026-10-08T05:30:00Z", Impressions: ip(20), Likes: 1},
		{ID: "arc", Text: "archive", CreatedAt: "2026-10-08T18:00:00Z", Impressions: ip(50), Likes: 4, Replies: ip(2), Reposts: 3},
		{ID: "launch", Text: "launch", CreatedAt: "2026-10-10T18:00:00Z", Impressions: ip(10), Likes: 1},
		{ID: "next", Text: "next", CreatedAt: "2026-10-11T18:00:00Z", Likes: 2},
		// After tracking has started, a day with no snapshot is still omitted.
		{ID: "gap", Text: "gap", CreatedAt: "2026-10-12T18:00:00Z", Likes: 50},
	}
	got := aggregateDays(hist, posts, "2026-10-18", loc)
	if len(got) != 5 {
		t.Fatalf("len %d", len(got))
	}
	wantDates := []string{"2026-10-07", "2026-10-08", "2026-10-10", "2026-10-11", "2026-10-18"}
	for i, d := range wantDates {
		if got[i].Date != d {
			t.Fatalf("day %d %s", i, got[i].Date)
		}
	}
	early := got[0]
	if !early.Backfilled || early.NewFollowers != nil || early.ScoreDelta != nil || early.InProgress {
		t.Fatalf("early backfill %+v", early)
	}
	if early.Likes != 1 || !imprIs(early.Impressions, 20) || early.Score == nil || *early.Score != 1 || early.Points != 1 {
		t.Fatalf("early score %+v %v", early, scoreOr(early.Score))
	}
	arc := got[1]
	wantPartial := postLevelPoints(2, 3, 4)
	if !arc.Backfilled || arc.NewFollowers != nil || arc.ScoreDelta != nil {
		t.Fatalf("archive flags %+v", arc)
	}
	if !nfIs(arc.Comments, 2) || arc.Reshares != 3 || arc.Likes != 4 || !imprIs(arc.Impressions, 50) || arc.Points != wantPartial || arc.Score == nil || *arc.Score != float64(wantPartial) {
		t.Fatalf("archive partial %+v score %v", arc, scoreOr(arc.Score))
	}
	if arc.Top == nil || arc.Top.ID != "arc" || arc.Top.Points != wantPartial {
		t.Fatalf("archive top %+v", arc.Top)
	}
	// A follower term of 0 would be the same number. Guard the weights used.
	if wantPartial != weightComments*2+weightReshares*3+weightLikes*4 || wantPartial == dayLevelPoints(2, 1, 3, 4) {
		t.Fatalf("partial formula %d", wantPartial)
	}

	first := got[2]
	if first.Backfilled || !nfIs(first.NewFollowers, 0) || first.Score == nil || *first.Score != 1 || first.Points != 1 || first.ScoreDelta != nil {
		t.Fatalf("first snapshot %+v delta %v", first, scoreOr(first.ScoreDelta))
	}
	// Oct 11's window holds both backfilled days and the first snapshot.
	// Including the backfilled scores would average (1+16+1)/3 = 6, delta 36.
	second := got[3]
	if second.Backfilled || !nfIs(second.NewFollowers, 10) || second.Score == nil || *second.Score != 42 || second.Points != 42 {
		t.Fatalf("second %+v score %v", second, scoreOr(second.Score))
	}
	if second.ScoreDelta == nil || math.Abs(*second.ScoreDelta-(42-1)) > 1e-9 {
		t.Fatalf("second delta %v", scoreOr(second.ScoreDelta))
	}
	last := got[4]
	if last.Backfilled || !last.InProgress || !nfIs(last.NewFollowers, 2) || !imprIs(last.Impressions, 0) || last.Likes != 0 || last.Score == nil || *last.Score != 8 {
		t.Fatalf("last %+v score %v", last, scoreOr(last.Score))
	}
	// Oct 12's likes are not on this day, and Oct 7-8 are outside the window.
	if last.ScoreDelta == nil || math.Abs(*last.ScoreDelta-(8-42)) > 1e-9 {
		t.Fatalf("last delta %v", scoreOr(last.ScoreDelta))
	}
}

func TestBackfilledEngagementAPI(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	mux, ing := testDB(t)
	user := func(followers int) string {
		return `{"id":"1","username":"demo","name":"Demo","public_metrics":{"followers_count":` + itoaN(followers) + `,"following_count":2,"tweet_count":3}}`
	}
	ing("d10.json", `{"date":"2026-10-10","accounts":{"demo":{"user":`+user(100)+`,"posts":[
		{"id":"800","text":"from the archive","created_at":"2026-10-08T18:00:00Z","public_metrics":{"impression_count":40,"like_count":1,"reply_count":1,"retweet_count":0,"quote_count":0}},
		{"id":"100","text":"launch","created_at":"2026-10-10T18:00:00Z","public_metrics":{"impression_count":10,"like_count":1,"reply_count":0,"retweet_count":0,"quote_count":0}}
	]}}}`)
	ing("d11.json", `{"date":"2026-10-11","accounts":{"demo":{"user":`+user(110)+`,"posts":[
		{"id":"800","text":"from the archive","created_at":"2026-10-08T18:00:00Z","public_metrics":{"impression_count":50,"like_count":4,"reply_count":2,"retweet_count":1,"quote_count":2}},
		{"id":"110","text":"next day","created_at":"2026-10-11T18:00:00Z","public_metrics":{"like_count":2,"reply_count":0,"retweet_count":0,"quote_count":0}}
	]}}}`)
	root := apiRoot(t, mux)
	hist := root["accounts"].(map[string]any)["demo"].(map[string]any)["history"].([]any)
	if len(hist) != 3 {
		t.Fatalf("history len %d", len(hist))
	}
	dates := []string{"2026-10-08", "2026-10-10", "2026-10-11"}
	for i, d := range dates {
		if hist[i].(map[string]any)["date"] != d {
			t.Fatalf("order %v", hist[i].(map[string]any)["date"])
		}
	}
	rowB, eB := histEng(t, root, "demo", "2026-10-08")
	if _, ok := eB["new_followers"]; !ok || eB["new_followers"] != nil || eB["backfilled"] != true {
		t.Fatalf("backfilled followers %#v flag %#v", eB["new_followers"], eB["backfilled"])
	}
	// Latest snapshot of the archive post: 3*2 + 2*3 + 4 = 16. No follower term.
	if asInt(eB["comments"]) != 2 || asInt(eB["reshares"]) != 3 || asInt(eB["likes"]) != 4 || asInt(eB["score"]) != 16 || asInt(eB["points"]) != 16 || asInt(eB["impressions"]) != 50 {
		t.Fatalf("partial score %+v", eB)
	}
	if eB["score_delta"] != nil || eB["in_progress"] == true {
		t.Fatalf("backfilled delta %#v", eB["score_delta"])
	}
	if rowB["followers"] != nil || rowB["following"] != nil || rowB["posts"] != nil {
		t.Fatalf("backfilled row has a snapshot: %+v", rowB)
	}
	row0, e0 := histEng(t, root, "demo", "2026-10-10")
	if asInt(row0["followers"]) != 100 || e0["backfilled"] != false || asInt(e0["new_followers"]) != 0 || asInt(e0["score"]) != 1 || e0["score_delta"] != nil {
		t.Fatalf("first snapshot %+v row %+v", e0, row0)
	}
	_, e1 := histEng(t, root, "demo", "2026-10-11")
	if e1["backfilled"] != false || asInt(e1["new_followers"]) != 10 || asInt(e1["score"]) != 42 {
		t.Fatalf("second snapshot %+v", e1)
	}
	delta, ok := asF(t, e1["score_delta"])
	if !ok || math.Abs(delta-(42-1)) > 1e-9 {
		t.Fatalf("delta %#v", e1["score_delta"])
	}
}

// A range whose posts all fall on snapshot days keeps the same days, scores,
// and follower deltas as before backfilled days existed.
func TestAllSnapshotRangeUnchanged(t *testing.T) {
	hist := []histDay{
		{Date: "2026-11-01", Followers: 20},
		{Date: "2026-11-02", Followers: 25},
		{Date: "2026-11-04", Followers: 25},
	}
	posts := []scoredPost{
		{ID: "a", Text: "a", CreatedAt: "2026-11-01T16:00:00Z", Impressions: ip(100), Likes: 4, Replies: ip(1), Reposts: 2},
		{ID: "b", Text: "b", CreatedAt: "2026-11-04T16:00:00Z", Impressions: ip(40), Likes: 3},
	}
	got := aggregateDays(hist, posts, "2026-11-04", denver(t))
	if len(got) != len(hist) {
		t.Fatalf("len %d", len(got))
	}
	wantPts := []int{
		dayLevelPoints(1, 0, 2, 4),
		dayLevelPoints(0, 5, 0, 0),
		dayLevelPoints(0, 0, 0, 3),
	}
	for i, want := range wantPts {
		e := got[i]
		if e.Date != hist[i].Date || e.Backfilled || e.NewFollowers == nil || e.Score == nil || *e.Score != float64(want) || e.Points != want {
			t.Fatalf("day %s %+v score %v", e.Date, e, scoreOr(e.Score))
		}
	}
	if !nfIs(got[0].NewFollowers, 0) || got[0].ScoreDelta != nil || !imprIs(got[0].Impressions, 100) {
		t.Fatalf("first %+v", got[0])
	}
	if !nfIs(got[1].NewFollowers, 5) || !imprIs(got[1].Impressions, 0) || got[1].Likes != 0 {
		t.Fatalf("empty day %+v", got[1])
	}
	d1 := *got[1].Score - *got[0].Score
	if got[1].ScoreDelta == nil || math.Abs(*got[1].ScoreDelta-d1) > 1e-9 {
		t.Fatalf("d1 delta %v", scoreOr(got[1].ScoreDelta))
	}
	// Nov 4's window is [Oct 28, Nov 4): both earlier snapshots, not a gap day.
	avg := (*got[0].Score + *got[1].Score) / 2
	if !got[2].InProgress || !nfIs(got[2].NewFollowers, 0) || !imprIs(got[2].Impressions, 40) || got[2].ScoreDelta == nil || math.Abs(*got[2].ScoreDelta-(*got[2].Score-avg)) > 1e-9 {
		t.Fatalf("last %+v delta %v", got[2], scoreOr(got[2].ScoreDelta))
	}
}
