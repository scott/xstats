package main

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func xStamp(t time.Time) string {
	return t.UTC().Format("Mon Jan 02 15:04:05 -0700 2006")
}

func ytd(object string, part int, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return append([]byte("window.YTD."+object+".part"+itoaN(part)+" = "), b...)
}

func writeArchive(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func zipTree(t *testing.T, dir, dest, prefix string) {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	err = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if prefix != "" {
			rel = prefix + "/" + rel
		}
		wr, err := w.Create(rel)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = wr.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openDB(filepath.Join(t.TempDir(), "t.db"))
	t.Cleanup(func() { db.Close() })
	return db
}

type postSnap struct {
	ID, Account, Date, Text, Created, Source string
	Impr, Replies                            sql.NullInt64
	Likes, Reposts, IsReply                  int
}

type acctSnap struct {
	Handle, ID, Name, Avatar string
}

func snapPosts(t *testing.T, db *sql.DB) []postSnap {
	t.Helper()
	rows, err := db.Query(`SELECT post_id,account,date,text,created_at,impressions,likes,replies,reposts,is_reply,source FROM post_metrics ORDER BY post_id, date, source`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []postSnap
	for rows.Next() {
		var s postSnap
		if err := rows.Scan(&s.ID, &s.Account, &s.Date, &s.Text, &s.Created, &s.Impr, &s.Likes, &s.Replies, &s.Reposts, &s.IsReply, &s.Source); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func snapAccounts(t *testing.T, db *sql.DB) []acctSnap {
	t.Helper()
	rows, err := db.Query(`SELECT handle,user_id,name,avatar FROM accounts ORDER BY handle`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []acctSnap
	for rows.Next() {
		var s acctSnap
		if err := rows.Scan(&s.Handle, &s.ID, &s.Name, &s.Avatar); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertNoWrites(t *testing.T, db *sql.DB) {
	t.Helper()
	var posts, accts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM post_metrics`).Scan(&posts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accts); err != nil {
		t.Fatal(err)
	}
	if posts != 0 || accts != 0 {
		t.Fatalf("import wrote posts=%d accounts=%d", posts, accts)
	}
}

func postBody(t *testing.T, mux *http.ServeMux, id string) map[string]any {
	t.Helper()
	w := get(mux, "/api/post/"+id)
	if w.Code != 200 {
		t.Fatalf("/api/post/%s %d %s", id, w.Code, w.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func historyRow(t *testing.T, post map[string]any) map[string]any {
	t.Helper()
	h, ok := post["history"].([]any)
	if !ok || len(h) != 1 {
		t.Fatalf("history %#v", post["history"])
	}
	return h[0].(map[string]any)
}

func baseTweet(id, created, text, likes, rts string) map[string]any {
	return map[string]any{
		"id_str": id, "created_at": created, "full_text": text,
		"favorite_count": likes, "retweet_count": rts,
		"lang": "en", "retweeted": false,
		"entities": map[string]any{"hashtags": []any{}},
	}
}

func accountJS(username string) []byte {
	return ytd("account", 0, []any{map[string]any{"account": map[string]any{
		"email": "not-stored@example.test", "username": username,
		"accountId": "4242", "accountDisplayName": "Example Archive",
	}}})
}

const (
	idOld   = "910000000000000001"
	idReply = "910000000000000002"
	idAt    = "910000000000000003"
	idRT    = "910000000000000004"
	idZero  = "910000000000000005"
)

func buildFakeArchive(t *testing.T, recent time.Time) string {
	t.Helper()
	old := time.Date(2018, 10, 10, 20, 19, 24, 0, time.UTC)
	if got := xStamp(old); got != "Wed Oct 10 20:19:24 +0000 2018" {
		t.Fatalf("documented stamp %q", got)
	}
	main := baseTweet(idOld, xStamp(old), `Fake archive post: "hello".`, "12", "3")
	main["in_reply_to_status_id"] = nil
	reply := baseTweet(idReply, xStamp(recent), "A reply in the fake archive.", "1", "0")
	reply["in_reply_to_user_id_str"] = "424242"
	reply["in_reply_to_screen_name"] = "example_friend"
	at := baseTweet(idAt, xStamp(recent), "@example_friend hi from the fake archive", "2", "1")
	rt := baseTweet(idRT, xStamp(recent), "RT @example_friend: skip this fake retweet", "9", "9")
	zero := baseTweet(idZero, xStamp(recent), "A fake archive post with zero likes.", "0", "0")
	dir := t.TempDir()
	writeArchive(t, dir, map[string][]byte{
		"data/account.js": accountJS("Example_Archive"),
		"data/tweets.js": ytd("tweets", 0, []any{
			map[string]any{"tweet": main},
			map[string]any{"tweet": reply},
			map[string]any{"tweet": rt},
		}),
		"data/tweets-part1.js": ytd("tweets", 1, []any{
			map[string]any{"tweet": at},
			map[string]any{"tweet": zero},
		}),
		// Present on purpose. The importer must not read it.
		"data/note-tweet.js": []byte(`window.YTD.note_tweet.part0 = [{"noteTweet":{"core":{"text":"THIS SHOULD NOT REPLACE FULL TEXT"}}}]`),
		"data/like.js":       []byte("not a tweet file"),
	})
	return dir
}

func TestImportArchiveDirAndZip(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	recent := time.Now().UTC().AddDate(0, 0, -2).Truncate(time.Second)
	when := time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC)
	dir := buildFakeArchive(t, recent)
	zipPath := filepath.Join(t.TempDir(), "fake-archive.zip")
	zipTree(t, dir, zipPath, "")

	db := openTestDB(t)
	if err := importArchiveAt(db, dir, "", when); err != nil {
		t.Fatal(err)
	}
	posts := snapPosts(t, db)
	if len(posts) != 4 {
		t.Fatalf("stored %d posts, want 4 (retweet skipped)", len(posts))
	}
	for _, p := range posts {
		if p.ID == idRT {
			t.Fatal("retweet was stored")
		}
		if p.Source != "archive" || p.Account != "example_archive" || p.Date != "2026-10-22" || p.Replies.Valid || p.Impr.Valid {
			t.Fatalf("row %+v", p)
		}
	}
	byID := map[string]postSnap{}
	for _, p := range posts {
		byID[p.ID] = p
	}
	if byID[idOld].Likes != 12 || byID[idOld].Reposts != 3 || byID[idOld].IsReply != 0 || byID[idOld].Created != "2018-10-10T20:19:24Z" || byID[idOld].Text != `Fake archive post: "hello".` {
		t.Fatalf("old post %+v", byID[idOld])
	}
	if byID[idReply].IsReply != 1 || byID[idReply].Likes != 1 || byID[idReply].Text != "A reply in the fake archive." {
		t.Fatalf("reply %+v", byID[idReply])
	}
	if byID[idAt].IsReply != 1 || byID[idAt].Reposts != 1 {
		t.Fatalf("@ reply %+v", byID[idAt])
	}
	if byID[idZero].Likes != 0 || byID[idZero].Reposts != 0 || byID[idZero].IsReply != 0 {
		t.Fatalf("zero %+v", byID[idZero])
	}
	if strings.Contains(byID[idOld].Text, "THIS SHOULD NOT REPLACE") {
		t.Fatal("note-tweet.js replaced full_text")
	}
	accts := snapAccounts(t, db)
	if len(accts) != 1 || accts[0].Handle != "example_archive" || accts[0].ID != "4242" || accts[0].Name != "Example Archive" || accts[0].Avatar != "" {
		t.Fatalf("account %+v", accts)
	}
	var leaked int
	if err := db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE user_id LIKE '%not-stored%' OR name LIKE '%not-stored%' OR avatar LIKE '%not-stored%'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("account email was stored")
	}

	mux := newMux(db, "secret")
	old := postBody(t, mux, idOld)
	row := historyRow(t, old)
	if old["created_at"] != "2018-10-10T20:19:24Z" || row["likes"].(float64) != 12 || row["reposts"].(float64) != 3 || row["replies"] != nil || row["impressions"] != nil {
		t.Fatalf("drill-in %#v hist %#v", old, row)
	}
	if get(mux, "/api/post/"+idRT).Code != 404 {
		t.Fatal("retweet visible")
	}
	zero := historyRow(t, postBody(t, mux, idZero))
	if zero["likes"].(float64) != 0 || zero["impressions"] != nil {
		t.Fatalf("zero drill-in %#v", zero)
	}
	root := apiRoot(t, mux)
	acct := root["accounts"].(map[string]any)["example_archive"].(map[string]any)
	if _, ok := acct["history"].([]any); !ok || len(acct["history"].([]any)) != 0 {
		t.Fatalf("history %#v", acct["history"])
	}
	listed := map[string]map[string]any{}
	for _, raw := range acct["posts"].([]any) {
		p := raw.(map[string]any)
		listed[p["id"].(string)] = p
	}
	if _, ok := listed[idOld]; ok {
		t.Fatal("2018 post is outside the 30-day list")
	}
	if listed[idReply]["is_reply"] != true || listed[idAt]["is_reply"] != true || listed[idZero]["is_reply"] != false {
		t.Fatalf("flags %#v", listed)
	}
	if listed[idReply]["impressions"] != nil || listed[idReply]["replies"] != nil || listed[idZero]["likes"].(float64) != 0 || listed[idReply]["source"] != "archive" {
		t.Fatalf("list metrics %#v", listed)
	}
	if !strings.Contains(get(mux, "/").Body.String(), "No follower snapshots yet") {
		t.Fatal("empty follower history would throw in the page")
	}

	again := snapPosts(t, db)
	acctsAgain := snapAccounts(t, db)
	if err := importArchiveAt(db, dir, "", when); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapPosts(t, db), again) || !reflect.DeepEqual(snapAccounts(t, db), acctsAgain) {
		t.Fatal("rerun changed rows")
	}
	later := when.AddDate(0, 0, 11)
	if err := importArchiveAt(db, dir, "", later); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapPosts(t, db), again) || !reflect.DeepEqual(snapAccounts(t, db), acctsAgain) {
		t.Fatal("later import added or changed an archive row")
	}
	tweetsPath := filepath.Join(dir, "data", "tweets.js")
	body, err := os.ReadFile(tweetsPath)
	if err != nil {
		t.Fatal(err)
	}
	swapped := strings.Replace(string(body), `"favorite_count":"12"`, `"favorite_count":"99"`, 1)
	if swapped == string(body) {
		t.Fatal("fixture had no favorite_count 12")
	}
	if err := os.WriteFile(tweetsPath, []byte(swapped), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := importArchiveAt(db, dir, "", later); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapPosts(t, db), again) {
		t.Fatal("reimport with a new like count changed the stored row")
	}

	db2 := openTestDB(t)
	if err := importArchiveAt(db2, zipPath, "@Example_Archive", when); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapPosts(t, db2), again) || !reflect.DeepEqual(snapAccounts(t, db2), acctsAgain) {
		t.Fatalf("zip import mismatch\nzip %#v\ndir %#v", snapPosts(t, db2), again)
	}

	nested := filepath.Join(t.TempDir(), "nested.zip")
	zipTree(t, dir, nested, "export")
	// The mutated tweets.js is what gets zipped here. First import of this zip
	// still has to load, and the like count is 99 because the file was edited.
	db3 := openTestDB(t)
	if err := importArchiveAt(db3, nested, "", when); err != nil {
		t.Fatal(err)
	}
	got := snapPosts(t, db3)
	if len(got) != 4 || !hasLikes(got, idOld, 99) {
		t.Fatalf("nested zip %+v", got)
	}
}

func hasLikes(posts []postSnap, id string, likes int) bool {
	for _, p := range posts {
		if p.ID == id {
			return p.Likes == likes
		}
	}
	return false
}

func TestImportArchivePartFileOnly(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	dir := t.TempDir()
	when := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	tw := baseTweet("910000000000000020", xStamp(when), "Only in tweets-part0.js.", "4", "1")
	writeArchive(t, dir, map[string][]byte{
		"data/account.js":      accountJS("example_archive"),
		"data/tweets-part0.js": ytd("tweets", 0, []any{map[string]any{"tweet": tw}}),
	})
	db := openTestDB(t)
	if err := importArchiveAt(db, dir, "", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	got := snapPosts(t, db)
	if len(got) != 1 || got[0].Text != "Only in tweets-part0.js." || got[0].Likes != 4 || got[0].Source != "archive" {
		t.Fatalf("%+v", got)
	}
}

func TestImportArchiveAccountFlag(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	when := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	tw := baseTweet("910000000000000030", xStamp(when), "Flagged account.", "1", "0")
	dir := t.TempDir()
	writeArchive(t, dir, map[string][]byte{
		"data/tweets.js": ytd("tweets", 0, []any{map[string]any{"tweet": tw}}),
	})
	db := openTestDB(t)
	if err := importArchiveAt(db, dir, "", when); err == nil || !strings.Contains(err.Error(), "--account") {
		t.Fatalf("missing account: %v", err)
	}
	assertNoWrites(t, db)
	if err := importArchiveAt(db, dir, "@Example_Only", when); err != nil {
		t.Fatal(err)
	}
	accts := snapAccounts(t, db)
	if len(accts) != 1 || accts[0].Handle != "example_only" || accts[0].Name != "" {
		t.Fatalf("%+v", accts)
	}
	posts := snapPosts(t, db)
	if len(posts) != 1 || posts[0].Account != "example_only" {
		t.Fatalf("%+v", posts)
	}

	mismatch := t.TempDir()
	writeArchive(t, mismatch, map[string][]byte{
		"data/account.js": accountJS("example_archive"),
		"data/tweets.js":  ytd("tweets", 0, []any{map[string]any{"tweet": tw}}),
	})
	db2 := openTestDB(t)
	err := importArchiveAt(db2, mismatch, "other_person", when)
	if err == nil || !strings.Contains(err.Error(), "data/account.js: item 0: field username:") || !strings.Contains(err.Error(), "does not match") {
		t.Fatal(err)
	}
	assertNoWrites(t, db2)
}

func TestImportArchiveStrictWritesNothing(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	when := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	created := xStamp(when)
	good := baseTweet("910000000000000040", created, "ok", "1", "2")
	cases := []struct {
		name string
		file string
		body []byte
		want string
	}{
		{
			name: "missing favorite_count",
			file: "data/tweets.js",
			body: ytd("tweets", 0, []any{map[string]any{"tweet": map[string]any{
				"id_str": "910000000000000040", "created_at": created, "full_text": "x", "retweet_count": "1",
			}}}),
			want: "data/tweets.js: item 0: field favorite_count: missing",
		},
		{
			name: "numeric favorite_count",
			file: "data/tweets.js",
			body: ytd("tweets", 0, []any{map[string]any{"tweet": map[string]any{
				"id_str": "910000000000000040", "created_at": created, "full_text": "x",
				"favorite_count": 4, "retweet_count": "1",
			}}}),
			want: "data/tweets.js: item 0: field favorite_count: want string, got number",
		},
		{
			name: "bad retweet_count",
			file: "data/tweets.js",
			body: ytd("tweets", 0, []any{map[string]any{"tweet": map[string]any{
				"id_str": "910000000000000040", "created_at": created, "full_text": "x",
				"favorite_count": "1", "retweet_count": "-1",
			}}}),
			want: "data/tweets.js: item 0: field retweet_count: not an integer",
		},
		{
			name: "unknown prefix",
			file: "data/tweets.js",
			body: []byte("window.YTD.tweet.part0 = []"),
			want: "data/tweets.js: unknown prefix",
		},
		{
			name: "bad created_at",
			file: "data/tweets.js",
			body: ytd("tweets", 0, []any{map[string]any{"tweet": map[string]any{
				"id_str": "910000000000000040", "created_at": "2018-10-10T20:19:24Z", "full_text": "x",
				"favorite_count": "1", "retweet_count": "0",
			}}}),
			want: "data/tweets.js: item 0: field created_at:",
		},
		{
			name: "second item",
			file: "data/tweets.js",
			body: ytd("tweets", 0, []any{
				map[string]any{"tweet": good},
				map[string]any{"tweet": map[string]any{"id_str": "910000000000000041", "created_at": created, "full_text": "nope", "retweet_count": "0"}},
			}),
			want: "data/tweets.js: item 1: field favorite_count: missing",
		},
		{
			name: "reply field type",
			file: "data/tweets.js",
			body: ytd("tweets", 0, []any{map[string]any{"tweet": map[string]any{
				"id_str": "910000000000000040", "created_at": created, "full_text": "x",
				"favorite_count": "1", "retweet_count": "0", "in_reply_to_user_id": 5,
			}}}),
			want: "data/tweets.js: item 0: field in_reply_to_user_id: want string, got number",
		},
		{
			name: "part file",
			file: "data/tweets-part1.js",
			body: ytd("tweets", 1, []any{map[string]any{"tweet": map[string]any{
				"id_str": "910000000000000041", "created_at": created, "favorite_count": "1", "retweet_count": "0",
			}}}),
			want: "data/tweets-part1.js: item 0: field full_text: missing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string][]byte{
				"data/account.js": accountJS("example_archive"),
				tc.file:           tc.body,
			}
			if tc.file != "data/tweets.js" {
				files["data/tweets.js"] = ytd("tweets", 0, []any{map[string]any{"tweet": good}})
			}
			writeArchive(t, dir, files)
			db := openTestDB(t)
			err := importArchiveAt(db, dir, "", when)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
			assertNoWrites(t, db)
		})
	}

	// A failed import must not change rows already stored.
	dir := buildFakeArchive(t, when)
	db := openTestDB(t)
	if err := importArchiveAt(db, dir, "", when); err != nil {
		t.Fatal(err)
	}
	before := snapPosts(t, db)
	bad := t.TempDir()
	writeArchive(t, bad, map[string][]byte{
		"data/account.js": accountJS("example_archive"),
		"data/tweets.js":  []byte("window.YTD.tweets.part0 = ["),
	})
	err := importArchiveAt(db, bad, "", when)
	if err == nil || !strings.Contains(err.Error(), "data/tweets.js: not a JSON array") {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapPosts(t, db), before) {
		t.Fatal("failed import changed stored rows")
	}
}

func TestImportArchiveDuplicatesAndPaths(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	when := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	created := xStamp(when)
	tw := baseTweet("910000000000000050", created, "once", "1", "0")
	dir := t.TempDir()
	writeArchive(t, dir, map[string][]byte{
		"data/account.js": accountJS("example_archive"),
		"data/tweets.js":  ytd("tweets", 0, []any{map[string]any{"tweet": tw}}),
		"data/tweets-part1.js": ytd("tweets", 1, []any{map[string]any{"tweet": baseTweet(
			"910000000000000050", created, "twice", "2", "0")}}),
	})
	db := openTestDB(t)
	err := importArchiveAt(db, dir, "", when)
	if err == nil || !strings.Contains(err.Error(), "data/tweets-part1.js: item 0: field id_str: duplicate of data/tweets.js item 0") {
		t.Fatal(err)
	}
	assertNoWrites(t, db)

	empty := t.TempDir()
	err = importArchiveAt(db, empty, "example_archive", when)
	if err == nil || !strings.Contains(err.Error(), "no data/tweets.js") {
		t.Fatal(err)
	}
	assertNoWrites(t, db)

	notZip := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notZip, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = importArchiveAt(db, notZip, "example_archive", when)
	if err == nil || !strings.Contains(err.Error(), "not a zip") {
		t.Fatal(err)
	}

	slip := filepath.Join(t.TempDir(), "slip.zip")
	f, err := os.Create(slip)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	wr, err := zw.Create("../data/tweets.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wr.Write(ytd("tweets", 0, []any{map[string]any{"tweet": tw}})); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err = importArchiveAt(db, slip, "example_archive", when)
	if err == nil || !strings.Contains(err.Error(), "bad path") {
		t.Fatal(err)
	}
	assertNoWrites(t, db)
}

func TestImportDayUsesStatsTZ(t *testing.T) {
	now := time.Date(2026, 10, 22, 5, 30, 0, 0, time.UTC)
	tw := baseTweet("910000000000000060", xStamp(now), "tz", "1", "0")
	dir := t.TempDir()
	writeArchive(t, dir, map[string][]byte{
		"data/tweets.js": ytd("tweets", 0, []any{map[string]any{"tweet": tw}}),
	})
	t.Setenv("XSTATS_TZ", "UTC")
	db := openTestDB(t)
	if err := importArchiveAt(db, dir, "example_archive", now); err != nil {
		t.Fatal(err)
	}
	if got := snapPosts(t, db); len(got) != 1 || got[0].Date != "2026-10-22" {
		t.Fatalf("utc %+v", got)
	}
	t.Setenv("XSTATS_TZ", "America/Denver")
	db2 := openTestDB(t)
	if err := importArchiveAt(db2, dir, "example_archive", now); err != nil {
		t.Fatal(err)
	}
	// 05:30 UTC is still the previous evening in Denver.
	if got := snapPosts(t, db2); len(got) != 1 || got[0].Date != "2026-10-21" {
		t.Fatalf("denver %+v", got)
	}
}

func liveBundle(id, text, created, date string, likes, replies, rts, quotes, impr int) string {
	b, err := json.Marshal(map[string]any{
		"date": date,
		"accounts": map[string]any{
			"example_archive": map[string]any{
				"user": map[string]any{
					"id": "7", "username": "example_archive", "name": "Live Name",
					"public_metrics": map[string]any{"followers_count": 10, "following_count": 1, "tweet_count": 2},
				},
				"posts": []any{map[string]any{
					"id": id, "text": text, "created_at": created,
					"public_metrics": map[string]any{
						"impression_count": impr, "like_count": likes, "reply_count": replies,
						"retweet_count": rts, "quote_count": quotes,
					},
				}},
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func ingestString(t *testing.T, db *sql.DB, body string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ingest(db, f); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveLiveWins(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	const id = "910000000000000070"
	created := time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)
	createdRFC := "2026-10-01T18:00:00Z"
	archiveDir := func(likes, rts, text string) string {
		t.Helper()
		dir := t.TempDir()
		tw := baseTweet(id, xStamp(created), text, likes, rts)
		writeArchive(t, dir, map[string][]byte{
			"data/account.js": accountJS("example_archive"),
			"data/tweets.js":  ytd("tweets", 0, []any{map[string]any{"tweet": tw}}),
		})
		return dir
	}
	importDay := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	t.Run("live then archive", func(t *testing.T) {
		db := openTestDB(t)
		ingestString(t, db, liveBundle(id, "live text", createdRFC, "2026-10-01", 9, 1, 3, 4, 100))
		if err := importArchiveAt(db, archiveDir("100", "5", "archive text"), "", importDay); err != nil {
			t.Fatal(err)
		}
		got := snapPosts(t, db)
		if len(got) != 1 || got[0].Source != "live" || got[0].Likes != 9 || got[0].Reposts != 7 || !got[0].Replies.Valid || got[0].Replies.Int64 != 1 || !got[0].Impr.Valid || got[0].Impr.Int64 != 100 || got[0].Text != "live text" {
			t.Fatalf("%+v", got)
		}
		if accts := snapAccounts(t, db); len(accts) != 1 || accts[0].Name != "Live Name" {
			t.Fatalf("account overwritten %+v", accts)
		}
		row := historyRow(t, postBody(t, newMux(db, "secret"), id))
		if row["likes"].(float64) != 9 || row["reposts"].(float64) != 7 || row["impressions"].(float64) != 100 {
			t.Fatalf("api %#v", row)
		}
		_, e := histEng(t, apiRoot(t, newMux(db, "secret")), "example_archive", "2026-10-01")
		if asInt(e["likes"]) != 9 {
			t.Fatalf("score used archive likes: %+v", e)
		}
	})

	t.Run("archive then earlier live", func(t *testing.T) {
		db := openTestDB(t)
		if err := importArchiveAt(db, archiveDir("100", "5", "archive text"), "", importDay); err != nil {
			t.Fatal(err)
		}
		ingestString(t, db, liveBundle(id, "live text", createdRFC, "2026-10-01", 9, 1, 3, 4, 100))
		got := snapPosts(t, db)
		if len(got) != 2 {
			t.Fatalf("want archive row kept beside the live row, got %+v", got)
		}
		row := historyRow(t, postBody(t, newMux(db, "secret"), id))
		if row["date"] != "2026-10-01" || row["likes"].(float64) != 9 || row["reposts"].(float64) != 7 || row["impressions"].(float64) != 100 {
			t.Fatalf("archive shadowed live: %#v", row)
		}
		_, e := histEng(t, apiRoot(t, newMux(db, "secret")), "example_archive", "2026-10-01")
		if asInt(e["likes"]) != 9 || e["impressions"] == nil {
			t.Fatalf("engagement shadowed: %+v", e)
		}
	})

	t.Run("same day live keeps other archive post", func(t *testing.T) {
		db := openTestDB(t)
		day := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
		other := "910000000000000071"
		dir := t.TempDir()
		writeArchive(t, dir, map[string][]byte{
			"data/account.js": accountJS("example_archive"),
			"data/tweets.js": ytd("tweets", 0, []any{
				map[string]any{"tweet": baseTweet(id, xStamp(created), "will be live", "2", "1")},
				map[string]any{"tweet": baseTweet(other, xStamp(created), "archive only", "4", "1")},
			}),
		})
		if err := importArchiveAt(db, dir, "", day); err != nil {
			t.Fatal(err)
		}
		ingestString(t, db, liveBundle(id, "live text", createdRFC, "2026-10-20", 9, 1, 3, 4, 100))
		got := snapPosts(t, db)
		if len(got) != 2 {
			t.Fatalf("%+v", got)
		}
		by := map[string]postSnap{}
		for _, p := range got {
			by[p.ID] = p
		}
		if by[id].Source != "live" || by[id].Likes != 9 || by[id].Date != "2026-10-20" || !by[id].Impr.Valid {
			t.Fatalf("same-day live %+v", by[id])
		}
		if by[other].Source != "archive" || by[other].Likes != 4 || by[other].Impr.Valid || by[other].Date != "2026-10-20" {
			t.Fatalf("archive-only post lost %+v", by[other])
		}
		row := historyRow(t, postBody(t, newMux(db, "secret"), id))
		if row["likes"].(float64) != 9 {
			t.Fatalf("api %#v", row)
		}
		otherRow := historyRow(t, postBody(t, newMux(db, "secret"), other))
		if otherRow["likes"].(float64) != 4 || otherRow["impressions"] != nil {
			t.Fatalf("other api %#v", otherRow)
		}
	})
}

func TestExistingRowsCountAsLive(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE post_metrics(post_id TEXT, account TEXT, date TEXT, text TEXT, created_at TEXT, impressions INT, likes INT, replies INT, reposts INT, PRIMARY KEY(post_id,date))`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO post_metrics(post_id,account,date,text,created_at,impressions,likes,replies,reposts) VALUES('42','demo','2020-01-01','old live post','2020-01-01T00:00:00Z',5,8,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db := openDB(path)
	t.Cleanup(func() { db.Close() })
	db2 := openDB(path)
	db2.Close()
	var source string
	var likes int
	var impr, replies sql.NullInt64
	if err := db.QueryRow(`SELECT source, likes, impressions, replies FROM post_metrics WHERE post_id='42'`).Scan(&source, &likes, &impr, &replies); err != nil {
		t.Fatal(err)
	}
	if source != "live" || likes != 8 || !impr.Valid || impr.Int64 != 5 || !replies.Valid || replies.Int64 != 0 {
		t.Fatalf("migrated source %q likes %d impr %+v replies %+v", source, likes, impr, replies)
	}
	arch := t.TempDir()
	tw := baseTweet("42", "Wed Oct 10 20:19:24 +0000 2018", "archive copy", "99", "1")
	writeArchive(t, arch, map[string][]byte{
		"data/tweets.js": ytd("tweets", 0, []any{map[string]any{"tweet": tw}}),
	})
	if err := importArchiveAt(db, arch, "demo", time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	got := snapPosts(t, db)
	if len(got) != 1 || got[0].Source != "live" || got[0].Likes != 8 || got[0].Text != "old live post" || !got[0].Impr.Valid || got[0].Impr.Int64 != 5 || !got[0].Replies.Valid || got[0].Replies.Int64 != 0 {
		t.Fatalf("legacy row overwritten %+v", got)
	}
}

// An archive reply has no reply count. The day's score is reshares and likes
// only. The reply flag must not add weightComments.
func TestArchiveScoreSkipsUnknownReplies(t *testing.T) {
	t.Setenv("XSTATS_TZ", "UTC")
	created := time.Now().UTC().AddDate(0, 0, -2).Truncate(24 * time.Hour).Add(18 * time.Hour)
	day := created.Format("2006-01-02")
	const likes, reposts = 5, 4
	dir := t.TempDir()
	tw := baseTweet("910000000000000080", xStamp(created), "Scored from the fake archive.", itoaN(likes), itoaN(reposts))
	tw["in_reply_to_user_id_str"] = "424242"
	writeArchive(t, dir, map[string][]byte{
		"data/account.js": accountJS("example_archive"),
		"data/tweets.js":  ytd("tweets", 0, []any{map[string]any{"tweet": tw}}),
	})
	db := openTestDB(t)
	if err := importArchiveAt(db, dir, "", time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	ingestString(t, db, `{"date":"`+day+`","accounts":{"example_archive":{"user":{"id":"4242","username":"example_archive","name":"Example Archive","public_metrics":{"followers_count":10,"following_count":1,"tweet_count":1}},"posts":[]}}}`)
	mux := newMux(db, "secret")
	_, e := histEng(t, apiRoot(t, mux), "example_archive", day)
	want := weightReshares*reposts + weightLikes*likes
	if asInt(e["score"]) != want || asInt(e["points"]) != want || asInt(e["reshares"]) != reposts || asInt(e["likes"]) != likes || asInt(e["new_followers"]) != 0 {
		t.Fatalf("score %+v want %d", e, want)
	}
	if e["comments"] != nil {
		t.Fatalf("unknown replies counted as %#v", e["comments"])
	}
	if asInt(e["score"]) != want || want+weightComments == want {
		t.Fatal("reply weight would not have changed the score")
	}
	posts := apiRoot(t, mux)["accounts"].(map[string]any)["example_archive"].(map[string]any)["posts"].([]any)
	if len(posts) != 1 {
		t.Fatalf("posts %#v", posts)
	}
	p := posts[0].(map[string]any)
	if p["replies"] != nil || p["source"] != "archive" || p["is_reply"] != true || p["impressions"] != nil {
		t.Fatalf("post %#v", p)
	}
}

func TestArchiveFooterCopy(t *testing.T) {
	body := get(newMux(openTestDB(t), "secret"), "/").Body.String()
	for _, s := range []string{
		"if(allLive) note+=' Reposts include quotes.'",
		"if(anyArchive) note+=' Archive rows have no reply, quote or impression counts.'",
		"p.source==='archive'",
		"v==null?'-':fmt(v)",
	} {
		if !strings.Contains(body, s) {
			t.Fatalf("page missing %q", s)
		}
	}
	if strings.Contains("Archive rows have no reply, quote or impression counts.", "\u2014") {
		t.Fatal("em dash in footer copy")
	}
}
