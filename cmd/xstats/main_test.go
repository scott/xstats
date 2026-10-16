package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
