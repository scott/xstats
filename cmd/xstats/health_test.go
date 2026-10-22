package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestHealthAuthShapeAndLastIngest(t *testing.T) {
	// A Denver process zone must not leak into server_time or last_ingest.
	origLocal := time.Local
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}
	time.Local = denver
	t.Cleanup(func() { time.Local = origLocal })

	path := filepath.Join(t.TempDir(), "legacy.db")
	writeLegacyDB(t, path)
	db := openDB(path)
	t.Cleanup(func() { db.Close() })

	var snaps int
	var postText string
	if err := db.QueryRow(`SELECT COUNT(*), (SELECT text FROM post_metrics WHERE account='kept') FROM follower_snapshots WHERE account='kept'`).Scan(&snaps, &postText); err != nil {
		t.Fatal(err)
	}
	if snaps != 2 || postText != "legacy-post-text-should-stay" {
		t.Fatalf("migration dropped rows: snaps %d text %q", snaps, postText)
	}
	var last sql.NullString
	if err := db.QueryRow(`SELECT last_ingest FROM accounts WHERE handle='kept'`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last.Valid {
		t.Fatalf("legacy account last_ingest = %q, want null", last.String)
	}

	// Second startup is a no-op migration and keeps the rows.
	db.Close()
	db = openDB(path)
	if err := db.QueryRow(`SELECT COUNT(*) FROM follower_snapshots WHERE account='kept'`).Scan(&snaps); err != nil {
		t.Fatal(err)
	}
	if snaps != 2 {
		t.Fatalf("reopen dropped snapshots: %d", snaps)
	}

	const token = "dummy-token"
	mux := newMux(db, token)
	closed := newMux(db, "")

	if w := healthReq(t, closed, http.MethodGet, "/api/health", "", ""); w.Code != 401 || w.Body.String() != "unauthorized\n" {
		t.Fatalf("unset token, no header: %d %s", w.Code, w.Body)
	}
	if w := healthReq(t, closed, http.MethodGet, "/api/health", token, ""); w.Code != 401 {
		t.Fatalf("unset token, bearer presented: %d %s", w.Code, w.Body)
	}

	for _, hdr := range []string{"", "Bearer wrong-token", "Bearer", "Basic " + token} {
		w := healthReqHeader(t, mux, http.MethodGet, "/api/health", hdr, "")
		if w.Code != 401 || w.Body.String() != "unauthorized\n" {
			t.Fatalf("header %q: %d %s", hdr, w.Code, w.Body)
		}
		w = healthReqHeader(t, mux, http.MethodPost, "/api/ingest", hdr, `{"date":"2026-04-01","accounts":{"kept":{"user":{"id":"9","public_metrics":{"followers_count":1}}}}}`)
		if w.Code != 401 {
			t.Fatalf("ingest header %q: %d %s", hdr, w.Code, w.Body)
		}
	}
	assertLastIngest(t, db, "kept", "")

	w := healthReq(t, mux, http.MethodGet, "/api/health", token, "")
	if w.Code != 200 {
		t.Fatalf("health: %d %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control %q", cc)
	}
	body := decodeHealth(t, w.Body.Bytes())
	ver, rev := versionStamp()
	if body.Version != ver || body.Commit != rev {
		t.Fatalf("stamp %s %s want %s %s", body.Version, body.Commit, ver, rev)
	}
	assertUTCTime(t, "server_time", body.ServerTime, time.Now())
	if _, ok := body.Accounts["demo"]; ok {
		t.Fatal("demo exists before ingest")
	}
	kept := body.Accounts["kept"]
	if kept.LatestSnapshotDate == nil || *kept.LatestSnapshotDate != "2026-03-04" {
		t.Fatalf("latest snapshot %#v", kept.LatestSnapshotDate)
	}
	if kept.LastIngest != nil {
		t.Fatalf("last_ingest before ingest %#v", kept.LastIngest)
	}
	assertNoLeak(t, w.Body.String())

	if w := healthReq(t, mux, http.MethodPost, "/api/health", token, ""); w.Code != 405 {
		t.Fatalf("POST health: %d", w.Code)
	}
	if w := healthReq(t, mux, http.MethodPost, "/api/ingest", token, "{"); w.Code != 400 {
		t.Fatalf("bad ingest: %d %s", w.Code, w.Body)
	}
	assertLastIngest(t, db, "kept", "")

	bundle := `{"date":"2026-10-15","accounts":{"Demo":{"user":{"id":"1","username":"demo","name":"Demo Secret Name","profile_image_url":"https://avatar.example/demo.png","public_metrics":{"followers_count":111222,"following_count":333,"tweet_count":444}},"posts":[{"id":"111","text":"post-text-should-not-leak","created_at":"2026-10-15T12:00:00.000Z","public_metrics":{"impression_count":555,"like_count":666,"reply_count":7,"retweet_count":8,"quote_count":9}}]}}}`
	if w := healthReq(t, mux, http.MethodPost, "/api/ingest", token, bundle); w.Code != 200 {
		t.Fatalf("ingest: %d %s", w.Code, w.Body)
	}
	data := get(mux, "/api/data").Body.String()
	if !strings.Contains(data, "post-text-should-not-leak") || !strings.Contains(data, "111222") {
		t.Fatal("ingest did not store the post or follower count")
	}

	w = healthReq(t, mux, http.MethodGet, "/api/health", token, "")
	body = decodeHealth(t, w.Body.Bytes())
	assertNoLeak(t, w.Body.String())
	kept = body.Accounts["kept"]
	if kept.LatestSnapshotDate == nil || *kept.LatestSnapshotDate != "2026-03-04" || kept.LastIngest != nil {
		t.Fatalf("untouched account changed: %#v", kept)
	}
	demo := body.Accounts["demo"]
	if demo.LatestSnapshotDate == nil || *demo.LatestSnapshotDate != "2026-10-15" {
		t.Fatalf("demo snapshot %#v", demo.LatestSnapshotDate)
	}
	if demo.LastIngest == nil {
		t.Fatal("demo last_ingest is null after ingest")
	}
	first := *demo.LastIngest
	assertUTCTime(t, "last_ingest", first, time.Now())

	if _, err := db.Exec(`UPDATE accounts SET last_ingest='1999-01-01T00:00:00Z' WHERE handle='demo'`); err != nil {
		t.Fatal(err)
	}
	if w := healthReq(t, mux, http.MethodPost, "/api/ingest", token, bundle); w.Code != 200 {
		t.Fatalf("second ingest: %d %s", w.Code, w.Body)
	}
	w = healthReq(t, mux, http.MethodGet, "/api/health", token, "")
	body = decodeHealth(t, w.Body.Bytes())
	assertNoLeak(t, w.Body.String())
	updated := body.Accounts["demo"].LastIngest
	if updated == nil || *updated == "1999-01-01T00:00:00Z" {
		t.Fatalf("last_ingest did not update: %#v", updated)
	}
	assertUTCTime(t, "last_ingest", *updated, time.Now())
	if body.Accounts["kept"].LastIngest != nil {
		t.Fatal("kept gained a last_ingest")
	}

	// Restart keeps the timestamp and the pre-change snapshot.
	db.Close()
	db = openDB(path)
	mux = newMux(db, token)
	w = healthReq(t, mux, http.MethodGet, "/api/health", token, "")
	body = decodeHealth(t, w.Body.Bytes())
	if body.Accounts["demo"].LastIngest == nil || *body.Accounts["demo"].LastIngest != *updated {
		t.Fatalf("last_ingest lost on reopen: %#v", body.Accounts["demo"].LastIngest)
	}
	if body.Accounts["kept"].LatestSnapshotDate == nil || *body.Accounts["kept"].LatestSnapshotDate != "2026-03-04" || body.Accounts["kept"].LastIngest != nil {
		t.Fatalf("kept after reopen: %#v", body.Accounts["kept"])
	}
	var still int
	if err := db.QueryRow(`SELECT followers FROM follower_snapshots WHERE account='kept' AND date='2026-01-02'`).Scan(&still); err != nil || still != 424242 {
		t.Fatalf("legacy follower row: %d %v", still, err)
	}
}

func writeLegacyDB(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.Exec(`
		CREATE TABLE accounts(handle TEXT PRIMARY KEY, user_id TEXT, name TEXT, avatar TEXT);
		CREATE TABLE follower_snapshots(account TEXT, date TEXT, followers INT, following INT, posts INT, PRIMARY KEY(account,date));
		CREATE TABLE post_metrics(post_id TEXT, account TEXT, date TEXT, text TEXT, created_at TEXT, impressions INT, likes INT, replies INT, reposts INT, PRIMARY KEY(post_id,date));
		CREATE TABLE clickthroughs(date TEXT PRIMARY KEY, visits INT);
		INSERT INTO accounts VALUES('kept','9','Kept Name','https://avatar.example/kept.png');
		INSERT INTO follower_snapshots VALUES('kept','2026-01-02',424242,7,3);
		INSERT INTO follower_snapshots VALUES('kept','2026-03-04',100,1,1);
		INSERT INTO post_metrics(post_id,account,date,text,created_at,impressions,likes,replies,reposts) VALUES('99','kept','2026-03-04','legacy-post-text-should-stay','2026-03-04T00:00:00Z',1,2,3,4);
	`)
	if err != nil {
		t.Fatal(err)
	}
}

func healthReq(t *testing.T, mux *http.ServeMux, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	hdr := ""
	if token != "" {
		hdr = "Bearer " + token
	}
	return healthReqHeader(t, mux, method, path, hdr, body)
}

func healthReqHeader(t *testing.T, mux *http.ServeMux, method, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	var req *http.Request
	if rdr == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, rdr)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func decodeHealth(t *testing.T, raw []byte) healthBody {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var body healthBody
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if keys := mapKeys(top); !reflect.DeepEqual(keys, []string{"accounts", "commit", "server_time", "version"}) {
		t.Fatalf("top-level keys %v", keys)
	}
	var accounts map[string]json.RawMessage
	if err := json.Unmarshal(top["accounts"], &accounts); err != nil {
		t.Fatalf("accounts: %v", err)
	}
	for handle, rawAcc := range accounts {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawAcc, &fields); err != nil {
			t.Fatalf("%s: %v", handle, err)
		}
		if keys := mapKeys(fields); !reflect.DeepEqual(keys, []string{"last_ingest", "latest_snapshot_date"}) {
			t.Fatalf("%s keys %v", handle, keys)
		}
	}
	return body
}

func mapKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertUTCTime(t *testing.T, name, got string, now time.Time) {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("%s %q: %v", name, got, err)
	}
	if parsed.Location() != time.UTC || !strings.HasSuffix(got, "Z") {
		t.Fatalf("%s %q is not UTC", name, got)
	}
	if parsed.Before(now.Add(-2*time.Minute)) || parsed.After(now.Add(2*time.Minute)) {
		t.Fatalf("%s %s is not near %s", name, got, now.UTC().Format(time.RFC3339))
	}
}

func assertLastIngest(t *testing.T, db *sql.DB, handle, want string) {
	t.Helper()
	var last sql.NullString
	if err := db.QueryRow(`SELECT last_ingest FROM accounts WHERE handle=?`, handle).Scan(&last); err != nil {
		t.Fatal(err)
	}
	got := ""
	if last.Valid {
		got = last.String
	}
	if got != want {
		t.Fatalf("%s last_ingest %q want %q", handle, got, want)
	}
}

func assertNoLeak(t *testing.T, body string) {
	t.Helper()
	for _, s := range []string{
		"legacy-post-text-should-stay",
		"post-text-should-not-leak",
		"Kept Name",
		"Demo Secret Name",
		"avatar.example",
		"111222",
		"424242",
		"followers",
		"impressions",
		"likes",
		"replies",
		"reposts",
	} {
		if strings.Contains(body, s) {
			t.Fatalf("health leaked %q in %s", s, body)
		}
	}
}
