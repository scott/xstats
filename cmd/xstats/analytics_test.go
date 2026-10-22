package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real header rows from the X analytics export. Fixture cells below are invented.
const (
	postsHeaderLiteral    = "Post id,Date,Post text,Post Link,Impressions,Likes,Engagements,Bookmarks,Shares,New follows,Replies,Reposts,Profile visits,Detail Expands,URL Clicks,Hashtag Clicks,Permalink Clicks"
	overviewHeaderLiteral = "Date,Impressions,Likes,Engagements,Bookmarks,Shares,New follows,Unfollows,Replies,Reposts,Profile visits,Create Post,Video views,Media views"
)

func TestAnalyticsHeaderLiterals(t *testing.T) {
	if strings.Join(postsColumns, ",") != postsHeaderLiteral {
		t.Fatalf("posts columns drifted:\n%s", strings.Join(postsColumns, ","))
	}
	if strings.Join(overviewColumns, ",") != overviewHeaderLiteral {
		t.Fatalf("overview columns drifted:\n%s", strings.Join(overviewColumns, ","))
	}
}

func splitHeader(literal string) []string {
	return strings.Split(literal, ",")
}

func writeAnalyticsCSV(t *testing.T, headers []string, rows [][]string, bom bool, pad bool) string {
	t.Helper()
	hdr := append([]string(nil), headers...)
	if pad {
		for i, h := range hdr {
			hdr[i] = " " + h + " "
		}
	}
	var buf bytes.Buffer
	if bom {
		buf.WriteString("\uFEFF")
	}
	w := csv.NewWriter(&buf)
	if err := w.Write(hdr); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "analytics.csv")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func rowByHeader(headers []string, values map[string]string) []string {
	out := make([]string, len(headers))
	for i, h := range headers {
		out[i] = values[h]
	}
	return out
}

func insertMetric(t *testing.T, db *sql.DB, id, account, date, source string, impr, replies any, likes, reposts int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO accounts(handle,user_id,name,avatar) VALUES(?,?,?,?) ON CONFLICT(handle) DO NOTHING`, account, "", "Example", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO post_metrics(post_id,account,date,text,created_at,impressions,likes,replies,reposts,is_reply,source) VALUES(?,?,?,?,?,?,?,?,?,0,?)`,
		id, account, date, "Fake post about widgets.", "2026-10-20T15:04:05Z", impr, likes, replies, reposts, source); err != nil {
		t.Fatal(err)
	}
}

func postsValues(id, impr, replies, likes string) map[string]string {
	return map[string]string{
		"Post id": id, "Date": "2026-10-20 15:04:05 +0000",
		"Post text":   `Fake line, with a comma and "quotes"`,
		"Post Link":   "https://x.com/example_posts/status/" + id,
		"Impressions": impr, "Likes": likes, "Engagements": "9", "Bookmarks": "0",
		"Shares": "0", "New follows": "0", "Replies": replies, "Reposts": "99",
		"Profile visits": "0", "Detail Expands": "0", "URL Clicks": "0",
		"Hashtag Clicks": "0", "Permalink Clicks": "0",
	}
}

func TestAnalyticsPostsExactHeader(t *testing.T) {
	db := openTestDB(t)
	const (
		idFill  = "930000000000000001"
		idZero  = "930000000000000002"
		idReply = "930000000000000003"
		idMiss  = "930000000000000099"
		account = "example_posts"
	)
	insertMetric(t, db, idFill, account, "2026-10-22", "archive", nil, nil, 4, 3)
	insertMetric(t, db, idZero, account, "2026-10-22", "archive", nil, nil, 1, 0)
	// Replies still unknown. Impressions were already filled and must stay.
	insertMetric(t, db, idReply, account, "2026-10-22", "archive", 10, nil, 2, 1)
	before := snapPosts(t, db)

	headers := splitHeader(postsHeaderLiteral)
	path := writeAnalyticsCSV(t, headers, [][]string{
		rowByHeader(headers, postsValues(idFill, "500", "7", "1")),
		rowByHeader(headers, postsValues(idZero, "0", "0", "8")),
		rowByHeader(headers, postsValues(idReply, "999", "4", "6")),
		rowByHeader(headers, postsValues(idMiss, "100", "1", "1")),
	}, true, true)

	line, err := applyAnalytics(db, path, "@Example_Posts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "3 updates") || !strings.Contains(line, "1 not in db") || !strings.Contains(line, "@example_posts") {
		t.Fatalf("summary %q", line)
	}
	byID := map[string]postSnap{}
	for _, p := range snapPosts(t, db) {
		byID[p.ID] = p
	}
	if len(byID) != len(before) {
		t.Fatalf("row count changed: %+v", byID)
	}
	got := byID[idFill]
	if !got.Impr.Valid || got.Impr.Int64 != 500 || !got.Replies.Valid || got.Replies.Int64 != 7 || got.Likes != 4 || got.Reposts != 3 || got.Source != "archive" || got.Text != "Fake post about widgets." {
		t.Fatalf("filled %+v", got)
	}
	zero := byID[idZero]
	if !zero.Impr.Valid || zero.Impr.Int64 != 0 || !zero.Replies.Valid || zero.Replies.Int64 != 0 || zero.Likes != 1 {
		t.Fatalf("zero %+v", zero)
	}
	reply := byID[idReply]
	if !reply.Impr.Valid || reply.Impr.Int64 != 10 || !reply.Replies.Valid || reply.Replies.Int64 != 4 || reply.Likes != 2 {
		t.Fatalf("partial %+v", reply)
	}
	if _, ok := byID[idMiss]; ok {
		t.Fatal("missing post was inserted")
	}

	mux := newMux(db, "secret")
	row := historyRow(t, postBody(t, mux, idFill))
	if row["impressions"].(float64) != 500 || row["replies"].(float64) != 7 || row["likes"].(float64) != 4 {
		t.Fatalf("drill-in %#v", row)
	}

	// Same file, then a file with different counts. Neither run writes again.
	again, err := applyAnalytics(db, path, "example_posts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again, "0 updates") {
		t.Fatalf("rerun %q", again)
	}
	changed := writeAnalyticsCSV(t, headers, [][]string{
		rowByHeader(headers, postsValues(idFill, "1", "1", "1")),
		rowByHeader(headers, postsValues(idZero, "5", "5", "5")),
		rowByHeader(headers, postsValues(idReply, "5", "5", "5")),
		rowByHeader(headers, postsValues(idMiss, "5", "5", "5")),
	}, false, false)
	third, err := applyAnalytics(db, changed, "example_posts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(third, "0 updates") {
		t.Fatalf("changed rerun %q", third)
	}
	after := snapPosts(t, db)
	byAfter := map[string]postSnap{}
	for _, p := range after {
		byAfter[p.ID] = p
	}
	if byAfter[idFill].Impr.Int64 != 500 || byAfter[idZero].Impr.Int64 != 0 || byAfter[idReply].Impr.Int64 != 10 || byAfter[idReply].Replies.Int64 != 4 {
		t.Fatalf("rerun overwrote %+v", after)
	}
}

func TestAnalyticsPostsShuffledColumns(t *testing.T) {
	db := openTestDB(t)
	const id = "930000000000000010"
	insertMetric(t, db, id, "example_posts", "2026-10-22", "archive", nil, nil, 4, 3)
	headers := append(splitHeader(postsHeaderLiteral), "Campaign")
	// Reverse the real columns so a position-based reader would take Likes
	// (the last original column after reversal is not Impressions) as the id
	// or the impression count. Campaign sits where a naive reader might look.
	for i, j := 0, len(headers)-2; i < j; i, j = i+1, j-1 {
		headers[i], headers[j] = headers[j], headers[i]
	}
	values := postsValues(id, "500", "7", "1")
	values["Campaign"] = "ignore-me"
	path := writeAnalyticsCSV(t, headers, [][]string{rowByHeader(headers, values)}, false, false)
	line, err := applyAnalytics(db, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "1 updates") || !strings.Contains(line, "ignored columns: Campaign") {
		t.Fatalf("summary %q", line)
	}
	got := snapPosts(t, db)[0]
	if !got.Impr.Valid || got.Impr.Int64 != 500 || !got.Replies.Valid || got.Replies.Int64 != 7 || got.Likes != 4 || got.Reposts != 3 {
		t.Fatalf("shuffled mapping %+v", got)
	}
	if strings.Contains(got.Text, "ignore-me") || strings.Contains(got.Text, "Fake line") {
		t.Fatalf("csv text stored: %q", got.Text)
	}
}

func TestAnalyticsUnknownLayout(t *testing.T) {
	db := openTestDB(t)
	insertMetric(t, db, "930000000000000020", "example_posts", "2026-10-22", "archive", nil, nil, 1, 0)
	before := snapPosts(t, db)
	path := writeAnalyticsCSV(t, []string{"Foo", "Bar", "Baz"}, [][]string{{"1", "2", "3"}}, false, false)
	_, err := applyAnalytics(db, path, "example_posts")
	if err == nil {
		t.Fatal("unknown layout succeeded")
	}
	msg := err.Error()
	for _, want := range []string{"matches neither", "found: Foo, Bar, Baz", "expected posts: " + strings.Join(postsColumns, ", "), "expected overview: " + strings.Join(overviewColumns, ", ")} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
	if !reflectPostsEqual(t, before, snapPosts(t, db)) {
		t.Fatal("unknown layout wrote posts")
	}
	assertNoFollows(t, db)
}

func TestAnalyticsMissingColumn(t *testing.T) {
	db := openTestDB(t)
	insertMetric(t, db, "930000000000000021", "example_posts", "2026-10-22", "archive", nil, nil, 1, 0)
	before := snapPosts(t, db)
	headers := splitHeader(postsHeaderLiteral)
	var kept []string
	for _, h := range headers {
		if h != "Impressions" {
			kept = append(kept, h)
		}
	}
	values := postsValues("930000000000000021", "500", "7", "1")
	path := writeAnalyticsCSV(t, kept, [][]string{rowByHeader(kept, values)}, false, false)
	_, err := applyAnalytics(db, path, "example_posts")
	if err == nil {
		t.Fatal("missing column succeeded")
	}
	msg := err.Error()
	for _, want := range []string{"missing required columns: Impressions", "found: " + strings.Join(kept, ", "), "expected: " + strings.Join(postsColumns, ", ")} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
	if !reflectPostsEqual(t, before, snapPosts(t, db)) {
		t.Fatal("missing column wrote posts")
	}
	assertNoFollows(t, db)
}

func TestAnalyticsNeverOverwriteLive(t *testing.T) {
	db := openTestDB(t)
	const (
		idKnown = "930000000000000030"
		idNull  = "930000000000000031"
		idArch  = "930000000000000032"
	)
	insertMetric(t, db, idKnown, "example_live", "2026-10-01", "live", 40, 2, 7, 1)
	insertMetric(t, db, idNull, "example_live", "2026-10-01", "live", nil, 0, 1, 0)
	insertMetric(t, db, idArch, "example_live", "2026-10-22", "archive", nil, nil, 4, 3)
	headers := splitHeader(postsHeaderLiteral)
	path := writeAnalyticsCSV(t, headers, [][]string{
		rowByHeader(headers, postsValues(idKnown, "999", "9", "9")),
		rowByHeader(headers, postsValues(idNull, "999", "9", "9")),
		rowByHeader(headers, postsValues(idArch, "500", "7", "1")),
	}, false, false)
	line, err := applyAnalytics(db, path, "example_live")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "1 updates") || !strings.Contains(line, "2 left live") {
		t.Fatalf("summary %q", line)
	}
	byID := map[string]postSnap{}
	for _, p := range snapPosts(t, db) {
		byID[p.ID] = p
	}
	known := byID[idKnown]
	if !known.Impr.Valid || known.Impr.Int64 != 40 || !known.Replies.Valid || known.Replies.Int64 != 2 || known.Likes != 7 || known.Source != "live" {
		t.Fatalf("live known %+v", known)
	}
	nulls := byID[idNull]
	if nulls.Impr.Valid || !nulls.Replies.Valid || nulls.Replies.Int64 != 0 || nulls.Likes != 1 || nulls.Source != "live" {
		t.Fatalf("live null %+v", nulls)
	}
	arch := byID[idArch]
	if !arch.Impr.Valid || arch.Impr.Int64 != 500 || !arch.Replies.Valid || arch.Replies.Int64 != 7 || arch.Likes != 4 {
		t.Fatalf("archive %+v", arch)
	}
	again, err := applyAnalytics(db, path, "example_live")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again, "0 updates") || !strings.Contains(again, "2 left live") {
		t.Fatalf("rerun %q", again)
	}
	byID = map[string]postSnap{}
	for _, p := range snapPosts(t, db) {
		byID[p.ID] = p
	}
	if byID[idKnown].Impr.Int64 != 40 || byID[idNull].Impr.Valid || byID[idArch].Impr.Int64 != 500 {
		t.Fatalf("rerun changed %+v", byID)
	}
}

func TestAnalyticsBadValueWritesNothing(t *testing.T) {
	db := openTestDB(t)
	insertMetric(t, db, "930000000000000040", "example_posts", "2026-10-22", "archive", nil, nil, 4, 3)
	before := snapPosts(t, db)
	headers := splitHeader(postsHeaderLiteral)
	badDate := postsValues("930000000000000040", "500", "7", "1")
	badDate["Date"] = "2026-10-20"
	path := writeAnalyticsCSV(t, headers, [][]string{
		rowByHeader(headers, badDate),
		rowByHeader(headers, postsValues("930000000000000041", "500", "7", "1")),
	}, false, false)
	_, err := applyAnalytics(db, path, "example_posts")
	if err == nil || !strings.Contains(err.Error(), `want a timestamp like "2026-10-22 03:30:00 +0000"`) {
		t.Fatalf("date error %v", err)
	}
	if !reflectPostsEqual(t, before, snapPosts(t, db)) {
		t.Fatal("bad date wrote")
	}

	comma := postsValues("930000000000000040", "1,234", "7", "1")
	path = writeAnalyticsCSV(t, headers, [][]string{rowByHeader(headers, comma)}, false, false)
	_, err = applyAnalytics(db, path, "example_posts")
	if err == nil || !strings.Contains(err.Error(), "not an integer") {
		t.Fatalf("count error %v", err)
	}
	if !reflectPostsEqual(t, before, snapPosts(t, db)) {
		t.Fatal("bad count wrote")
	}
	assertNoFollows(t, db)
}

func TestAnalyticsOverviewTimezone(t *testing.T) {
	headers := splitHeader(overviewHeaderLiteral)
	// 03:30 UTC is still 21:30 the previous evening in Denver (MDT, UTC-6).
	const stamp = "2026-10-22 03:30:00 +0000"
	hdr := append(append([]string{}, headers...), "Notes")
	row := func(cols []string, date, follows, unfollows string) []string {
		return rowByHeader(cols, map[string]string{
			"Date": date, "Impressions": "10", "Likes": "1", "Engagements": "2",
			"Bookmarks": "0", "Shares": "0", "New follows": follows, "Unfollows": unfollows,
			"Replies": "0", "Reposts": "0", "Profile visits": "0", "Create Post": "1",
			"Video views": "0", "Media views": "0", "Notes": "ignore-me",
		})
	}

	t.Run("denver", func(t *testing.T) {
		db := openTestDB(t)
		t.Setenv("XSTATS_TZ", "America/Denver")
		path := writeAnalyticsCSV(t, hdr, [][]string{row(hdr, stamp, "4", "1")}, false, false)
		_, err := applyAnalytics(db, path, "")
		if err == nil || !strings.Contains(err.Error(), "--account") {
			t.Fatalf("missing account %v", err)
		}
		assertNoFollows(t, db)
		var accounts int
		if err := db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accounts); err != nil {
			t.Fatal(err)
		}
		if accounts != 0 {
			t.Fatal("missing account created a row")
		}

		line, err := applyAnalytics(db, path, "@Example_Follows")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(line, "1 updates") || !strings.Contains(line, "@example_follows") || !strings.Contains(line, "ignored columns: Notes") {
			t.Fatalf("summary %q", line)
		}
		got := followRows(t, db)
		if len(got) != 1 || got[0].account != "example_follows" || got[0].date != "2026-10-21" || got[0].newFollows != 4 || got[0].unfollows != 1 || got[0].source != "analytics" {
			t.Fatalf("denver row %+v", got)
		}
		var last sql.NullString
		if err := db.QueryRow(`SELECT last_ingest FROM accounts WHERE handle='example_follows'`).Scan(&last); err != nil {
			t.Fatal(err)
		}
		if last.Valid {
			t.Fatalf("overview set last_ingest %q", last.String)
		}

		again, err := applyAnalytics(db, path, "example_follows")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(again, "0 updates") || !strings.Contains(again, "1 already stored") {
			t.Fatalf("rerun %q", again)
		}
		bumped := writeAnalyticsCSV(t, hdr, [][]string{row(hdr, stamp, "50", "9")}, false, false)
		third, err := applyAnalytics(db, bumped, "example_follows")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(third, "0 updates") {
			t.Fatalf("changed rerun %q", third)
		}
		got = followRows(t, db)
		if len(got) != 1 || got[0].newFollows != 4 || got[0].unfollows != 1 || got[0].date != "2026-10-21" {
			t.Fatalf("rerun overwrote %+v", got)
		}
	})

	t.Run("utc", func(t *testing.T) {
		db := openTestDB(t)
		t.Setenv("XSTATS_TZ", "UTC")
		path := writeAnalyticsCSV(t, headers, [][]string{row(headers, stamp, "4", "1")}, false, false)
		if _, err := applyAnalytics(db, path, "example_follows"); err != nil {
			t.Fatal(err)
		}
		got := followRows(t, db)
		if len(got) != 1 || got[0].date != "2026-10-22" {
			t.Fatalf("utc row %+v", got)
		}
	})

	// The cell's offset is applied before XSTATS_TZ. The date prefix alone is
	// 2026-10-22, but 20:00 at -0600 is 02:00 the next day in UTC.
	t.Run("cell offset", func(t *testing.T) {
		db := openTestDB(t)
		t.Setenv("XSTATS_TZ", "UTC")
		path := writeAnalyticsCSV(t, headers, [][]string{row(headers, "2026-10-22 20:00:00 -0600", "2", "0")}, false, false)
		if _, err := applyAnalytics(db, path, "example_follows"); err != nil {
			t.Fatal(err)
		}
		got := followRows(t, db)
		if len(got) != 1 || got[0].date != "2026-10-23" || got[0].newFollows != 2 {
			t.Fatalf("offset row %+v", got)
		}
	})
}

func TestAnalyticsOverviewSkipsLiveDays(t *testing.T) {
	db := openTestDB(t)
	t.Setenv("XSTATS_TZ", "UTC")
	if _, err := db.Exec(`INSERT INTO accounts(handle,user_id,name,avatar,last_ingest) VALUES('example_follows','','Example','','2026-10-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO follower_snapshots(account,date,followers,following,posts) VALUES('example_follows','2026-10-20',10,1,1)`); err != nil {
		t.Fatal(err)
	}
	headers := splitHeader(overviewHeaderLiteral)
	row := func(date, follows, unfollows string) []string {
		return rowByHeader(headers, map[string]string{
			"Date": date, "Impressions": "0", "Likes": "0", "Engagements": "0",
			"Bookmarks": "0", "Shares": "0", "New follows": follows, "Unfollows": unfollows,
			"Replies": "0", "Reposts": "0", "Profile visits": "0", "Create Post": "0",
			"Video views": "0", "Media views": "0",
		})
	}
	// A bad later row must not keep the valid day, and a live day must not be stored.
	bad := writeAnalyticsCSV(t, headers, [][]string{
		row("2026-10-21 12:00:00 +0000", "3", "1"),
		row("not-a-date", "9", "9"),
	}, false, false)
	_, err := applyAnalytics(db, bad, "example_follows")
	if err == nil || !strings.Contains(err.Error(), "data row 2") {
		t.Fatalf("bad overview %v", err)
	}
	assertNoFollows(t, db)

	path := writeAnalyticsCSV(t, headers, [][]string{
		row("2026-10-20 12:00:00 +0000", "8", "2"),
		row("2026-10-21 12:00:00 +0000", "3", "1"),
	}, false, false)
	line, err := applyAnalytics(db, path, "example_follows")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "1 updates") || !strings.Contains(line, "1 left live") {
		t.Fatalf("summary %q", line)
	}
	got := followRows(t, db)
	if len(got) != 1 || got[0].date != "2026-10-21" || got[0].newFollows != 3 || got[0].unfollows != 1 || got[0].source != "analytics" {
		t.Fatalf("rows %+v", got)
	}
	var keptIngest string
	if err := db.QueryRow(`SELECT last_ingest FROM accounts WHERE handle='example_follows'`).Scan(&keptIngest); err != nil {
		t.Fatal(err)
	}
	if keptIngest != "2026-10-20T00:00:00Z" {
		t.Fatalf("last_ingest changed to %q", keptIngest)
	}
	var snaps int
	if err := db.QueryRow(`SELECT COUNT(*) FROM follower_snapshots`).Scan(&snaps); err != nil {
		t.Fatal(err)
	}
	if snaps != 1 {
		t.Fatalf("snapshots %d", snaps)
	}
	again, err := applyAnalytics(db, path, "example_follows")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again, "0 updates") || !strings.Contains(again, "1 already stored") || !strings.Contains(again, "1 left live") {
		t.Fatalf("rerun %q", again)
	}
	if len(followRows(t, db)) != 1 {
		t.Fatal("rerun added a row")
	}
}

func TestAnalyticsOtherAccountUntouched(t *testing.T) {
	db := openTestDB(t)
	insertMetric(t, db, "930000000000000050", "example_other", "2026-10-22", "archive", nil, nil, 4, 3)
	headers := splitHeader(postsHeaderLiteral)
	path := writeAnalyticsCSV(t, headers, [][]string{
		rowByHeader(headers, postsValues("930000000000000050", "500", "7", "1")),
	}, false, false)
	line, err := applyAnalytics(db, path, "example_posts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "1 not in db") || !strings.Contains(line, "0 updates") {
		t.Fatalf("summary %q", line)
	}
	got := snapPosts(t, db)[0]
	if got.Impr.Valid || got.Replies.Valid || got.Account != "example_other" {
		t.Fatalf("other account %+v", got)
	}
}

type followSnap struct {
	account, date, source string
	newFollows, unfollows int64
}

func followRows(t *testing.T, db *sql.DB) []followSnap {
	t.Helper()
	rows, err := db.Query(`SELECT account, date, new_follows, unfollows, source FROM daily_follows ORDER BY account, date`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []followSnap
	for rows.Next() {
		var s followSnap
		if err := rows.Scan(&s.account, &s.date, &s.newFollows, &s.unfollows, &s.source); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertNoFollows(t *testing.T, db *sql.DB) {
	t.Helper()
	if n := len(followRows(t, db)); n != 0 {
		t.Fatalf("daily_follows rows %d", n)
	}
}

func reflectPostsEqual(t *testing.T, a, b []postSnap) bool {
	t.Helper()
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			t.Logf("diff %d %+v vs %+v", i, a[i], b[i])
			return false
		}
	}
	return true
}
