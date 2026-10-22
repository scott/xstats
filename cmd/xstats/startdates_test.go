package main

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseStartDates(t *testing.T) {
	ok := map[string]string{"example_a": "2026-10-14", "example_b": "2026-09-01"}
	cases := []struct {
		in   string
		want map[string]string
		bad  string
	}{
		{in: "", want: nil},
		{in: "   ", want: nil},
		{in: "example_a:2026-10-14,example_b:2026-09-01", want: ok},
		{in: "  @Example_A : 2026-10-14 , example_b:2026-09-01 ", want: ok},
		{in: "example_leap:2024-02-29", want: map[string]string{"example_leap": "2024-02-29"}},
		{in: "example:2026-02-31", bad: `bad pair "example:2026-02-31"`},
		{in: "example:2026-02-29", bad: `bad pair "example:2026-02-29"`},
		{in: "example:2026-13-01", bad: `bad pair "example:2026-13-01"`},
		{in: "example:2026-00-10", bad: `bad pair "example:2026-00-10"`},
		{in: "example:2026-10-00", bad: `bad pair "example:2026-10-00"`},
		{in: "example:not-a-date", bad: `bad pair "example:not-a-date"`},
		{in: "example:2026-10-14T00:00:00Z", bad: `bad pair "example:2026-10-14T00:00:00Z"`},
		{in: "example:2026/10/14", bad: `bad pair "example:2026/10/14"`},
		{in: "example:2026-1-02", bad: `bad pair "example:2026-1-02"`},
		{in: ":2026-10-14", bad: `bad pair ":2026-10-14"`},
		{in: "@:2026-10-14", bad: `bad pair "@:2026-10-14"`},
		{in: "  :2026-10-14", bad: `bad pair ":2026-10-14"`},
		{in: "example", bad: `bad pair "example"`},
		{in: "example:2026-10-14:extra", bad: `bad pair "example:2026-10-14:extra"`},
		{in: "example:", bad: `bad pair "example:"`},
		{in: "example:2026-10-14,", bad: `bad pair ""`},
		{in: "example:2026-10-14, ,example_b:2026-09-01", bad: `bad pair " "`},
		{in: "example_a:2026-10-14,example_a:2026-10-15", bad: `bad pair "example_a:2026-10-15"`},
		{in: "Example_A:2026-10-14,@example_a:2026-11-01", bad: `bad pair "@example_a:2026-11-01"`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseStartDates(tc.in)
			if tc.bad != "" {
				if err == nil || !strings.Contains(err.Error(), tc.bad) {
					t.Fatalf("parse error %v, want %s", err, tc.bad)
				}
				if !strings.Contains(err.Error(), "XSTATS_START_DATES") {
					t.Fatalf("error %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v", got)
			}
		})
	}
}

func TestConfigureStartDates(t *testing.T) {
	accountStartDates = map[string]string{"example_keep": "2026-01-02"}
	t.Cleanup(func() { accountStartDates = nil })

	t.Setenv("XSTATS_START_DATES", "example:2026-02-31")
	err := configureStartDates()
	if err == nil || !strings.Contains(err.Error(), `bad pair "example:2026-02-31"`) {
		t.Fatal(err)
	}
	if accountStartDates["example_keep"] != "2026-01-02" || len(accountStartDates) != 1 {
		t.Fatalf("failed parse replaced the map: %#v", accountStartDates)
	}

	t.Setenv("XSTATS_START_DATES", "   ")
	if err := configureStartDates(); err != nil {
		t.Fatal(err)
	}
	if accountStartDates != nil {
		t.Fatalf("blank value stored %#v", accountStartDates)
	}

	t.Setenv("XSTATS_START_DATES", "")
	if err := configureStartDates(); err != nil {
		t.Fatal(err)
	}
	if accountStartDates != nil {
		t.Fatalf("empty value stored %#v", accountStartDates)
	}
}

func TestBadStartDatesExits(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "t.db")
	cmd := exec.Command("go", "run", ".", "ingest", "--db", dbPath)
	cmd.Env = envWith("XSTATS_START_DATES", "example:2026-02-31")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, output %s", out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("exit %v output %s", err, out)
	}
	if !strings.Contains(string(out), `bad pair "example:2026-02-31"`) {
		t.Fatalf("output %s", out)
	}
	if _, statErr := os.Stat(dbPath); statErr == nil {
		t.Fatal("bad start dates still created a database")
	}
}

func TestBackfillCap(t *testing.T) {
	loc := time.UTC
	hist := []histDay{{Date: "2026-10-20", Followers: 10}, {Date: "2026-10-21", Followers: 12}}
	posts := []scoredPost{
		{ID: "73011", Text: "example too old", CreatedAt: "2026-09-19T12:00:00Z", Likes: 50},
		{ID: "73012", Text: "example floor", CreatedAt: "2026-09-20T12:00:00Z", Likes: 3},
		{ID: "73013", Text: "example snap", CreatedAt: "2026-10-20T12:00:00Z", Likes: 1},
	}
	got := aggregateDays(hist, posts, "2026-10-21", loc)
	wantDates := []string{"2026-09-20", "2026-10-20", "2026-10-21"}
	if len(got) != len(wantDates) {
		t.Fatalf("len %d dates %v", len(got), dayNames(got))
	}
	for i, d := range wantDates {
		if got[i].Date != d {
			t.Fatalf("day %d %s", i, got[i].Date)
		}
	}
	if !got[0].Backfilled || got[0].Likes != 3 || got[0].Score == nil || *got[0].Score != 3 {
		t.Fatalf("floor day %+v", got[0])
	}
	if got[1].Backfilled || got[1].Likes != 1 || got[1].Score == nil || *got[1].Score != 1 {
		t.Fatalf("snapshot absorbed the old post %+v", got[1])
	}
	// 2026-03-01 minus 30 calendar days is 2026-01-30 (February has 28 days).
	march := []histDay{{Date: "2026-03-01", Followers: 5}}
	month := aggregateDays(march, []scoredPost{
		{ID: "73014", Text: "example jan 29", CreatedAt: "2026-01-29T12:00:00Z", Likes: 8},
		{ID: "73015", Text: "example jan 30", CreatedAt: "2026-01-30T12:00:00Z", Likes: 4},
	}, "2026-03-01", loc)
	if len(month) != 2 || month[0].Date != "2026-01-30" || month[1].Date != "2026-03-01" || month[0].Likes != 4 || month[1].Likes != 0 {
		t.Fatalf("month boundary %+v", month)
	}
}

func TestBackfillCapKeepsPostsList(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	t.Setenv("XSTATS_START_DATES", "")
	if err := configureStartDates(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { accountStartDates = nil })
	if accountStart("example_cap") != "" {
		t.Fatal("empty XSTATS_START_DATES configured a start date")
	}

	db := openTestDB(t)
	now := time.Now().UTC()
	snap := now.AddDate(0, 0, 15)
	snapDay := snap.Format("2006-01-02")
	floorDay := snap.AddDate(0, 0, -30).Format("2006-01-02")
	floorAt, err := time.Parse("2006-01-02", floorDay)
	if err != nil {
		t.Fatal(err)
	}
	outside := now.AddDate(0, 0, -20)
	if outside.Format("2006-01-02") >= floorDay || !outside.After(now.AddDate(0, 0, -30)) {
		t.Fatalf("outside %s floor %s", outside.Format(time.RFC3339), floorDay)
	}
	onFloor := floorAt.Add(15 * time.Hour)
	justOut := floorAt.AddDate(0, 0, -1).Add(15 * time.Hour)

	mustIngest(t, db, snapBundle(snapDay, "example_cap", 10, []string{
		livePost("73001", outside.Format(time.RFC3339), "example outside cap", 50),
		livePost("73002", onFloor.Format(time.RFC3339), "example on cap floor", 3),
		livePost("73003", justOut.Format(time.RFC3339), "example before cap floor", 9),
		livePost("73004", snap.Format(time.RFC3339), "example snapshot day", 1),
	}))
	postsBefore := countTable(t, db, "post_metrics")
	snapsBefore := countTable(t, db, "follower_snapshots")

	mux := newMux(db, "secret")
	root := apiRoot(t, mux)
	acct := root["accounts"].(map[string]any)["example_cap"].(map[string]any)
	dates := historyDates(t, acct)
	if strings.Join(dates, ",") != floorDay+","+snapDay {
		t.Fatalf("history %v", dates)
	}
	_, floorEng := histEng(t, root, "example_cap", floorDay)
	if floorEng["backfilled"] != true || asInt(floorEng["likes"]) != 3 || asInt(floorEng["score"]) != 3 || floorEng["new_followers"] != nil {
		t.Fatalf("floor engagement %+v", floorEng)
	}
	_, snapEng := histEng(t, root, "example_cap", snapDay)
	if snapEng["backfilled"] != false || asInt(snapEng["likes"]) != 1 || asInt(snapEng["score"]) != 1 || asInt(snapEng["new_followers"]) != 0 {
		t.Fatalf("snapshot engagement %+v", snapEng)
	}
	if engagementLikes(t, acct) != 4 {
		t.Fatalf("totals include a capped post: %d", engagementLikes(t, acct))
	}
	ids := postIDs(t, acct)
	for _, id := range []string{"73001", "73002", "73003", "73004"} {
		if !ids[id] {
			t.Fatalf("posts list missing %s: %v", id, ids)
		}
	}
	if get(mux, "/api/post/73001").Code != 200 {
		t.Fatal("capped post was removed from the drill-in")
	}
	body := get(mux, "/api/post/73001").Body.String()
	if !strings.Contains(body, "example outside cap") {
		t.Fatalf("drill-in %s", body)
	}
	if countTable(t, db, "post_metrics") != postsBefore || countTable(t, db, "follower_snapshots") != snapsBefore {
		t.Fatal("cap changed stored rows")
	}
	var likes int
	if err := db.QueryRow(`SELECT likes FROM post_metrics WHERE post_id='73001'`).Scan(&likes); err != nil || likes != 50 {
		t.Fatalf("stored likes %d %v", likes, err)
	}
}

func TestStartDateHidesStrayPost(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	start := now.AddDate(0, 0, -1).Format("2006-01-02")
	early := now.AddDate(0, 0, -3).Format("2006-01-02")
	gap := now.AddDate(0, 0, -5).Format("2006-01-02")
	plainSnap := now.AddDate(0, 0, -2).Format("2006-01-02")
	plainBack := now.AddDate(0, 0, -12).Format("2006-01-02")
	earlyOnly := now.AddDate(0, 0, -4).Format("2006-01-02")

	t.Setenv("XSTATS_START_DATES", "example_dated:"+start+",@Example_Early:"+today)
	if err := configureStartDates(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { accountStartDates = nil })
	if accountStart("example_plain") != "" || accountStart("example_dated") != start || accountStart("example_early") != today {
		t.Fatalf("starts dated %q early %q plain %q", accountStart("example_dated"), accountStart("example_early"), accountStart("example_plain"))
	}

	db := openTestDB(t)
	day := func(d string) string { return d + "T15:00:00Z" }
	mustIngest(t, db, snapBundle(early, "example_dated", 40, []string{
		livePost("71011", day(early), "example before start", 9),
		livePost("71012", day(start), "example kept post", 99),
	}))
	mustIngest(t, db, snapBundle(start, "example_dated", 80, []string{
		livePost("71012", day(start), "example kept post", 1),
	}))
	mustIngest(t, db, snapBundle(today, "example_dated", 85, []string{
		livePost("71013", day(today), "example today", 2),
		livePost("71010", "2019-06-01T15:00:00Z", "example ancient dated", 100),
		livePost("71014", day(gap), "example gap before start", 7),
	}))
	mustIngest(t, db, snapBundle(plainSnap, "example_plain", 20, []string{
		livePost("71022", day(plainSnap), "example plain snap", 1),
		livePost("71021", day(plainBack), "example plain backfill", 4),
	}))
	mustIngest(t, db, snapBundle(today, "example_plain", 23, []string{
		livePost("71020", "2018-01-01T15:00:00Z", "example ancient plain", 77),
	}))
	mustIngest(t, db, snapBundle(earlyOnly, "example_early", 15, []string{
		livePost("71030", day(earlyOnly), "example entirely before start", 5),
	}))

	postsBefore := countTable(t, db, "post_metrics")
	snapsBefore := countTable(t, db, "follower_snapshots")
	mux := newMux(db, "secret")
	root := apiRoot(t, mux)
	accts := root["accounts"].(map[string]any)

	dated := accts["example_dated"].(map[string]any)
	if strings.Join(historyDates(t, dated), ",") != start+","+today {
		t.Fatalf("dated history %v", historyDates(t, dated))
	}
	rowStart, engStart := histEng(t, root, "example_dated", start)
	if asInt(rowStart["followers"]) != 80 || asInt(engStart["new_followers"]) != 0 || asInt(engStart["likes"]) != 1 || asInt(engStart["score"]) != 1 || engStart["score_delta"] != nil {
		t.Fatalf("start day row %+v eng %+v", rowStart, engStart)
	}
	_, engToday := histEng(t, root, "example_dated", today)
	delta, ok := asF(t, engToday["score_delta"])
	if asInt(engToday["new_followers"]) != 5 || asInt(engToday["likes"]) != 2 || asInt(engToday["score"]) != 22 || !ok || delta != 21 {
		t.Fatalf("today eng %+v", engToday)
	}
	if engagementLikes(t, dated) != 3 {
		t.Fatalf("dated totals %d", engagementLikes(t, dated))
	}
	ids := postIDs(t, dated)
	if !ids["71012"] || !ids["71013"] || ids["71010"] || ids["71011"] || ids["71014"] {
		t.Fatalf("dated posts %v", ids)
	}
	kept := get(mux, "/api/post/71012")
	if kept.Code != 200 || strings.Contains(kept.Body.String(), early) || !strings.Contains(kept.Body.String(), start) {
		t.Fatalf("kept drill-in %d %s", kept.Code, kept.Body)
	}
	for _, id := range []string{"71010", "71011", "71014"} {
		if get(mux, "/api/post/"+id).Code != 404 {
			t.Fatalf("post %s still visible", id)
		}
	}

	plain := accts["example_plain"].(map[string]any)
	if strings.Join(historyDates(t, plain), ",") != plainBack+","+plainSnap+","+today {
		t.Fatalf("plain history %v", historyDates(t, plain))
	}
	_, plainEarly := histEng(t, root, "example_plain", plainBack)
	if plainEarly["backfilled"] != true || asInt(plainEarly["likes"]) != 4 || asInt(plainEarly["score"]) != 4 || plainEarly["new_followers"] != nil || plainEarly["score_delta"] != nil {
		t.Fatalf("plain backfill %+v", plainEarly)
	}
	_, plainMid := histEng(t, root, "example_plain", plainSnap)
	if plainMid["backfilled"] != false || asInt(plainMid["likes"]) != 1 || asInt(plainMid["score"]) != 1 || asInt(plainMid["new_followers"]) != 0 {
		t.Fatalf("plain snapshot %+v", plainMid)
	}
	_, plainLast := histEng(t, root, "example_plain", today)
	plainDelta, ok := asF(t, plainLast["score_delta"])
	if asInt(plainLast["likes"]) != 0 || asInt(plainLast["new_followers"]) != 3 || asInt(plainLast["score"]) != 12 || !ok || plainDelta != 11 {
		t.Fatalf("plain today %+v", plainLast)
	}
	plainIDs := postIDs(t, plain)
	if !plainIDs["71021"] || !plainIDs["71022"] || plainIDs["71020"] {
		t.Fatalf("plain posts %v", plainIDs)
	}
	if get(mux, "/api/post/71020").Code != 200 {
		t.Fatal("uncapped drill-in hidden")
	}

	earlyAcct := accts["example_early"].(map[string]any)
	if len(historyDates(t, earlyAcct)) != 0 || len(postIDs(t, earlyAcct)) != 0 {
		t.Fatalf("early account still visible hist %v posts %v", historyDates(t, earlyAcct), postIDs(t, earlyAcct))
	}
	if get(mux, "/api/post/71030").Code != 404 {
		t.Fatal("early drill-in still visible")
	}

	if countTable(t, db, "post_metrics") != postsBefore || countTable(t, db, "follower_snapshots") != snapsBefore {
		t.Fatal("start date changed stored rows")
	}
	var ancientLikes, earlyFollowers int
	if err := db.QueryRow(`SELECT likes FROM post_metrics WHERE post_id='71010'`).Scan(&ancientLikes); err != nil || ancientLikes != 100 {
		t.Fatalf("ancient likes %d %v", ancientLikes, err)
	}
	if err := db.QueryRow(`SELECT followers FROM follower_snapshots WHERE account='example_dated' AND date=?`, early).Scan(&earlyFollowers); err != nil || earlyFollowers != 40 {
		t.Fatalf("early followers %d %v", earlyFollowers, err)
	}
	var keptRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM post_metrics WHERE post_id='71012'`).Scan(&keptRows); err != nil || keptRows != 2 {
		t.Fatalf("kept post rows %d %v", keptRows, err)
	}
}

func TestStartDateZoneBoundary(t *testing.T) {
	for _, zone := range []string{"America/Denver", "UTC"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().In(loc)
			start := now.Format("2006-01-02")
			midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
			before := midnight.Add(-time.Second)
			after := midnight
			beforeAt := before.UTC().Format(time.RFC3339)
			afterAt := after.UTC().Format(time.RFC3339)
			beforeDay, ok := postDay(beforeAt, loc)
			if !ok || beforeDay >= start {
				t.Fatalf("before %s day %s start %s", beforeAt, beforeDay, start)
			}
			afterDay, ok := postDay(afterAt, loc)
			if !ok || afterDay != start {
				t.Fatalf("after %s day %s start %s", afterAt, afterDay, start)
			}
			if zone == "America/Denver" {
				if before.UTC().Format("2006-01-02") != start || after.UTC().Format("2006-01-02") != start {
					t.Fatalf("denver utc dates before %s after %s start %s", before.UTC().Format(time.RFC3339), after.UTC().Format(time.RFC3339), start)
				}
			}
			if zone == "UTC" && (before.UTC().Format("2006-01-02") == start || after.UTC().Format("2006-01-02") != start) {
				t.Fatalf("utc dates before %s after %s", beforeAt, afterAt)
			}

			handle := "example_denver"
			beforeID, afterID := "74001", "74002"
			if zone == "UTC" {
				handle = "example_utc"
				beforeID, afterID = "74003", "74004"
			}
			t.Setenv("XSTATS_TZ", zone)
			t.Setenv("XSTATS_START_DATES", handle+":"+start)
			if err := configureStartDates(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { accountStartDates = nil })

			db := openTestDB(t)
			mustIngest(t, db, snapBundle(start, handle, 30, []string{
				livePost(beforeID, beforeAt, "example just before local midnight", 40),
				livePost(afterID, afterAt, "example just after local midnight", 2),
			}))
			postsBefore := countTable(t, db, "post_metrics")
			mux := newMux(db, "secret")
			root := apiRoot(t, mux)
			acct := root["accounts"].(map[string]any)[handle].(map[string]any)
			dates := historyDates(t, acct)
			if len(dates) != 1 || dates[0] != start {
				t.Fatalf("history %v", dates)
			}
			_, eng := histEng(t, root, handle, start)
			if asInt(eng["likes"]) != 2 || asInt(eng["score"]) != 2 || asInt(eng["new_followers"]) != 0 {
				t.Fatalf("engagement %+v", eng)
			}
			if strings.Contains(get(mux, "/api/data").Body.String(), beforeDay) && beforeDay != start {
				t.Fatalf("history still has %s", beforeDay)
			}
			ids := postIDs(t, acct)
			if ids[beforeID] || !ids[afterID] {
				t.Fatalf("posts %v", ids)
			}
			if get(mux, "/api/post/"+beforeID).Code != 404 {
				t.Fatal("before post still in drill-in")
			}
			if got := get(mux, "/api/post/"+afterID); got.Code != 200 || !strings.Contains(got.Body.String(), "example just after local midnight") {
				t.Fatalf("after drill-in %d %s", got.Code, got.Body)
			}
			if countTable(t, db, "post_metrics") != postsBefore {
				t.Fatal("zone filter changed stored rows")
			}
		})
	}
}

func envWith(key, value string) []string {
	var out []string
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		if name == key {
			continue
		}
		out = append(out, e)
	}
	return append(out, key+"="+value)
}

func mustIngest(t *testing.T, db *sql.DB, body string) {
	t.Helper()
	if err := ingestBytes(db, []byte(body)); err != nil {
		t.Fatal(err)
	}
}

func countTable(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	switch table {
	case "post_metrics", "follower_snapshots":
	default:
		t.Fatalf("unexpected table %s", table)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func historyDates(t *testing.T, acct map[string]any) []string {
	t.Helper()
	raw, ok := acct["history"].([]any)
	if !ok {
		t.Fatalf("history %#v", acct["history"])
	}
	out := make([]string, 0, len(raw))
	for _, row := range raw {
		out = append(out, row.(map[string]any)["date"].(string))
	}
	return out
}

func postIDs(t *testing.T, acct map[string]any) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	raw, _ := acct["posts"].([]any)
	for _, p := range raw {
		got[p.(map[string]any)["id"].(string)] = true
	}
	return got
}

func engagementLikes(t *testing.T, acct map[string]any) int {
	t.Helper()
	sum := 0
	for _, row := range acct["history"].([]any) {
		e := row.(map[string]any)["engagement"].(map[string]any)
		sum += asInt(e["likes"])
	}
	return sum
}

func snapBundle(date, handle string, followers int, posts []string) string {
	return `{"date":"` + date + `","accounts":{"` + handle + `":{"user":{"id":"1","username":"` + handle + `","name":"Example","public_metrics":{"followers_count":` + strconv.Itoa(followers) + `,"following_count":2,"tweet_count":3}},"posts":[` + strings.Join(posts, ",") + `]}}}`
}

func livePost(id, created, text string, likes int) string {
	return `{"id":"` + id + `","text":` + strconv.Quote(text) + `,"created_at":"` + created + `","public_metrics":{"impression_count":10,"like_count":` + strconv.Itoa(likes) + `,"reply_count":0,"retweet_count":0,"quote_count":0}}`
}

func dayNames(days []dayEngagement) []string {
	out := make([]string, len(days))
	for i, d := range days {
		out[i] = d.Date
	}
	return out
}
