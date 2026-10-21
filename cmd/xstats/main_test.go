package main

import (
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

func TestUTCDay(t *testing.T) {
	d, ok := utcDay("2026-10-14T12:00:00.000Z")
	if !ok || d != "2026-10-14" {
		t.Fatalf("fractional Z: %q %v", d, ok)
	}
	d, ok = utcDay("2026-09-03T20:00:00-07:00") // 03:00Z next day
	if !ok || d != "2026-09-04" {
		t.Fatalf("offset: %q", d)
	}
	if _, ok = utcDay(""); ok {
		t.Fatal("empty created_at should not bucket")
	}
	if _, ok = utcDay("not-a-date"); ok {
		t.Fatal("garbage created_at should not bucket")
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
		{ID: "p1", Text: "first", CreatedAt: "2026-09-01T12:00:00.000Z", Impressions: 1000, Likes: 50},
		// Zero impressions: score stays nil (no divide by zero) even with a follower drop.
		{ID: "p2", Text: "quiet", CreatedAt: "2026-09-02T12:00:00Z", Impressions: 0, Likes: 4, Replies: 1},
		{ID: "p3", Text: "scored", CreatedAt: "2026-09-03T12:00:00Z", Impressions: 1000, Likes: 10},
		// 20:00-07:00 is the next UTC day, so this lands on 09-04, not 09-03.
		{ID: "p4", Text: "offset", CreatedAt: "2026-09-03T20:00:00-07:00", Impressions: 1000, Likes: 5},
		{ID: "skip", Text: "bad", CreatedAt: "", Impressions: 99999, Likes: 99999},
		{ID: "old", Text: "unaligned", CreatedAt: "2026-08-01T12:00:00Z", Impressions: 99999, Likes: 99999},
		{ID: "p5", Text: "latest", CreatedAt: "2026-09-10T12:00:00Z", Impressions: 1000, Likes: 16},
	}
	got := aggregateDays(hist, posts, "2026-09-10")
	if len(got) != len(hist) {
		t.Fatalf("len %d", len(got))
	}

	d0 := got[0]
	if d0.NewFollowers != 0 || d0.Score == nil || *d0.Score != 50 || d0.InProgress || d0.Points != 50 {
		t.Fatalf("first day: %+v score %v", d0, scoreOr(d0.Score))
	}
	if d0.ScoreDelta != nil {
		t.Fatal("first scored day has no prior week")
	}

	d1 := got[1]
	if d1.NewFollowers != -10 || d1.Impressions != 0 || d1.Score != nil || d1.Points != dayLevelPoints(1, -10, 0, 4) {
		t.Fatalf("zero impressions: %+v", d1)
	}
	if d1.ScoreDelta != nil {
		t.Fatal("nil score should not have a delta")
	}
	if math.IsInf(float64(d1.Points), 0) {
		t.Fatal("inf")
	}

	// 09-10's prior window is [09-03, 09-10). 09-01 (score 50) is outside it.
	// 09-02 has no score. Average of 09-03 (10) and 09-04 (5) is 7.5; delta is 8.5.
	last := got[4]
	if !last.InProgress || last.Score == nil || *last.Score != 16 {
		t.Fatalf("today: %+v score %v", last, scoreOr(last.Score))
	}
	if last.ScoreDelta == nil || math.Abs(*last.ScoreDelta-8.5) > 1e-9 {
		t.Fatalf("delta %v", scoreOr(last.ScoreDelta))
	}
	if got[2].InProgress || got[3].Date != "2026-09-04" || got[3].Likes != 5 || got[3].Impressions != 1000 {
		t.Fatalf("utc bucket: %+v %+v", got[2], got[3])
	}
	if got[2].Score == nil || *got[2].Score != 10 || got[3].Score == nil || *got[3].Score != 5 {
		t.Fatal("window inputs")
	}
	// 09-01 must not have been pulled into 09-10.
	if *last.ScoreDelta == 16-50 {
		t.Fatal("delta used the day outside the 7-day window")
	}
}

func TestTopPostTieBreak(t *testing.T) {
	hist := []histDay{{Date: "2026-10-01", Followers: 5}}
	higherImp := aggregateDays(hist, []scoredPost{
		{ID: "1", Text: "low", CreatedAt: "2026-10-01T00:00:00Z", Impressions: 100, Likes: 10},
		{ID: "2", Text: "high", CreatedAt: "2026-10-01T00:00:00Z", Impressions: 200, Likes: 10},
	}, "2026-01-01")
	if higherImp[0].Top == nil || higherImp[0].Top.ID != "2" || higherImp[0].Top.Points != 10 {
		t.Fatalf("impressions tie-break: %+v", higherImp[0].Top)
	}
	lowerID := aggregateDays(hist, []scoredPost{
		{ID: "200", Text: "b", CreatedAt: "2026-10-01T00:00:00Z", Impressions: 100, Likes: 10},
		{ID: "100", Text: "a", CreatedAt: "2026-10-01T00:00:00Z", Impressions: 100, Likes: 10},
	}, "2026-01-01")
	if lowerID[0].Top == nil || lowerID[0].Top.ID != "100" {
		t.Fatalf("id tie-break: %+v", lowerID[0].Top)
	}
}

func TestNegativeScore(t *testing.T) {
	hist := []histDay{{Date: "2026-10-01", Followers: 100}, {Date: "2026-10-02", Followers: 90}}
	posts := []scoredPost{{ID: "1", CreatedAt: "2026-10-02T00:00:00Z", Impressions: 1000}}
	got := aggregateDays(hist, posts, "2026-01-01")
	wantPts := dayLevelPoints(0, -10, 0, 0)
	if got[1].NewFollowers != -10 || got[1].Points != wantPts || got[1].Score == nil || *got[1].Score != float64(wantPts) {
		t.Fatalf("negative: %+v score %v", got[1], scoreOr(got[1].Score))
	}
}

func scoreOr(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
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
	d0 := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	d1 := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	d2 := time.Now().Format("2006-01-02")
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
	// First snapshot: new followers are 0. Zero impressions: no score.
	if asInt(e0["new_followers"]) != 0 || asInt(e0["impressions"]) != 0 || e0["score"] != nil || asInt(e0["points"]) != 7 || e0["in_progress"] == true {
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
	if !ok || math.Abs(sc-97*1000/2500.0) > 1e-9 {
		t.Fatalf("d1 score %v", e1["score"])
	}
	if e1["score_delta"] != nil || e1["in_progress"] == true {
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
	if asInt(e1["points"]) != 110 {
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
	if !ok || math.Abs(sc-21) > 1e-9 {
		t.Fatalf("today score %v", e2["score"])
	}
	delta, ok := asF(t, e2["score_delta"])
	wantDelta := 21 - (110 * 1000 / 4500.0)
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
	if !ok || math.Abs(sc-30) > 1e-9 {
		t.Fatalf("re-push score %v", e2["score"])
	}
	_, e1 = histEng(t, root, "demo", d1)
	if asInt(e1["impressions"]) != 4500 {
		t.Fatalf("re-push changed an older day: %v", e1["impressions"])
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
	for _, s := range []string{"Engagement score", "--bar:#a371f7", "--bar:#8250df", "No bar: this day has 0 impressions."} {
		if !strings.Contains(body, s) {
			t.Fatalf("page missing %q", s)
		}
	}
	if strings.Contains(body, "Example data") || strings.Contains(body, "EXAMPLE DATA") {
		t.Fatal("example-data label must not ship")
	}
}
