package main

import (
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed web
var webFS embed.FS

const schema = `
CREATE TABLE IF NOT EXISTS accounts(handle TEXT PRIMARY KEY, user_id TEXT, name TEXT, avatar TEXT);
CREATE TABLE IF NOT EXISTS follower_snapshots(account TEXT, date TEXT, followers INT, following INT, posts INT, PRIMARY KEY(account,date));
CREATE TABLE IF NOT EXISTS post_metrics(post_id TEXT, account TEXT, date TEXT, text TEXT, created_at TEXT, impressions INT, likes INT, replies INT, reposts INT, PRIMARY KEY(post_id,date));
CREATE TABLE IF NOT EXISTS clickthroughs(date TEXT PRIMARY KEY, visits INT);`

func openDB(p string) *sql.DB {
	db, err := sql.Open("sqlite", p)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		log.Fatal(err)
	}
	db.Exec(`ALTER TABLE post_metrics ADD COLUMN is_reply INT DEFAULT 0`) // no-op if present
	db.Exec(`ALTER TABLE accounts ADD COLUMN last_ingest TEXT`)           // no-op if present
	db.Exec(`UPDATE post_metrics SET is_reply=1 WHERE text LIKE '@%'`)
	// impressions has always been INT without NOT NULL, so existing rows can
	// already hold NULL. Nothing is rewritten: a stored 0 stays 0.
	if err := migrateImpressions(db); err != nil {
		log.Fatal(err)
	}
	return db
}

// migrateImpressions confirms post_metrics.impressions can store NULL.
// Databases created by this app already can. Rows are not copied or updated.
func migrateImpressions(db *sql.DB) error {
	var notnull int
	err := db.QueryRow(`SELECT "notnull" FROM pragma_table_info('post_metrics') WHERE name='impressions'`).Scan(&notnull)
	if err != nil {
		return fmt.Errorf("post_metrics.impressions: %w", err)
	}
	if notnull != 0 {
		return fmt.Errorf("post_metrics.impressions is NOT NULL")
	}
	return nil
}

// Raw bundle: x tool results as returned, grouped per account.
type rawBundle struct {
	Date     string `json:"date"`
	Accounts map[string]struct {
		User struct {
			ID, Name, Username string
			Avatar             string `json:"profile_image_url"`
			PM                 struct {
				Followers int `json:"followers_count"`
				Following int `json:"following_count"`
				Tweets    int `json:"tweet_count"`
			} `json:"public_metrics"`
		} `json:"user"`
		Posts []struct {
			ID, Text  string
			CreatedAt string `json:"created_at"`
			InReplyTo string `json:"in_reply_to_user_id"`
			PM        struct {
				// Nil when impression_count is omitted. A JSON 0 stays 0.
				Impr    *int `json:"impression_count"`
				Likes   int  `json:"like_count"`
				Replies int  `json:"reply_count"`
				RT      int  `json:"retweet_count"`
				Quotes  int  `json:"quote_count"`
			} `json:"public_metrics"`
		} `json:"posts"`
	} `json:"accounts"`
	Clickthroughs *struct {
		Date   string `json:"date"`
		Visits int    `json:"visits"`
	} `json:"clickthroughs"`
}

func ingest(db *sql.DB, file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return ingestBytes(db, b)
}

func ingestBytes(db *sql.DB, b []byte) error {
	var r rawBundle
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	if r.Date == "" {
		r.Date = time.Now().Format("2006-01-02")
	}
	ingestedAt := time.Now().UTC().Format(time.RFC3339)
	tx, _ := db.Begin()
	defer tx.Rollback()
	n := 0
	for h, a := range r.Accounts {
		h = strings.ToLower(h)
		u := a.User
		if _, err := tx.Exec(`INSERT INTO accounts(handle,user_id,name,avatar,last_ingest) VALUES(?,?,?,?,?) ON CONFLICT(handle) DO UPDATE SET user_id=excluded.user_id,name=excluded.name,avatar=excluded.avatar,last_ingest=excluded.last_ingest`, h, u.ID, u.Name, u.Avatar, ingestedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO follower_snapshots VALUES(?,?,?,?,?) ON CONFLICT(account,date) DO UPDATE SET followers=excluded.followers,following=excluded.following,posts=excluded.posts`, h, r.Date, u.PM.Followers, u.PM.Following, u.PM.Tweets); err != nil {
			return err
		}
		// Re-pushing a date fully replaces that account's post rows for the date.
		// An empty posts list is treated as "no data" and leaves existing rows alone.
		if len(a.Posts) > 0 {
			if _, err := tx.Exec(`DELETE FROM post_metrics WHERE account=? AND date=?`, h, r.Date); err != nil {
				return err
			}
		}
		for _, p := range a.Posts {
			if strings.HasPrefix(p.Text, "RT @") {
				continue // retweets are not stored
			}
			reply := 0
			if strings.HasPrefix(p.Text, "@") || p.InReplyTo != "" {
				reply = 1
			}
			if _, err := tx.Exec(`INSERT INTO post_metrics(post_id,account,date,text,created_at,impressions,likes,replies,reposts,is_reply) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(post_id,date) DO UPDATE SET text=excluded.text,impressions=excluded.impressions,likes=excluded.likes,replies=excluded.replies,reposts=excluded.reposts,is_reply=excluded.is_reply`,
				p.ID, h, r.Date, p.Text, p.CreatedAt, p.PM.Impr, p.PM.Likes, p.PM.Replies, p.PM.RT+p.PM.Quotes, reply); err != nil {
				return err
			}
			n++
		}
	}
	if c := r.Clickthroughs; c != nil {
		d := c.Date
		if d == "" {
			d = r.Date
		}
		tx.Exec(`INSERT INTO clickthroughs VALUES(?,?) ON CONFLICT(date) DO UPDATE SET visits=excluded.visits`, d, c.Visits)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("ingested %s: %d accounts, %d post rows\n", r.Date, len(r.Accounts), n)
	return nil
}

func apiData(db *sql.DB) (any, error) {
	out := map[string]any{
		"generated":          time.Now().Format(time.RFC3339),
		"engagement_weights": engagementWeights(),
	}
	loc, err := statsLocation()
	if err != nil {
		return nil, err
	}
	today := time.Now().In(loc).Format("2006-01-02")
	accts := map[string]any{}
	rows, err := db.Query(`SELECT handle,name,avatar FROM accounts`)
	if err != nil {
		return nil, err
	}
	type acc struct{ h, n, a string }
	var list []acc
	for rows.Next() {
		var a acc
		rows.Scan(&a.h, &a.n, &a.a)
		list = append(list, a)
	}
	rows.Close()
	for _, a := range list {
		var hist []map[string]any
		r, _ := db.Query(`SELECT date,followers,following,posts FROM follower_snapshots WHERE account=? ORDER BY date`, a.h)
		for r.Next() {
			var d string
			var f, g, p int
			r.Scan(&d, &f, &g, &p)
			hist = append(hist, map[string]any{"date": d, "followers": f, "following": g, "posts": p})
		}
		r.Close()
		if len(hist) > 0 {
			postsForScore, err := loadLatestPosts(db, a.h)
			if err != nil {
				return nil, err
			}
			snaps := make([]histDay, len(hist))
			for i, row := range hist {
				snaps[i] = histDay{Date: row["date"].(string), Followers: row["followers"].(int)}
			}
			for i, e := range aggregateDays(snaps, postsForScore, today, loc) {
				hist[i]["engagement"] = engagementJSON(a.h, e)
			}
		}
		var posts []map[string]any
		r, _ = db.Query(`SELECT post_id,text,created_at,impressions,likes,replies,reposts,is_reply FROM post_metrics m WHERE account=? AND created_at>=? AND date=(SELECT MAX(date) FROM post_metrics WHERE post_id=m.post_id) ORDER BY created_at DESC LIMIT 100`, a.h, time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339))
		for r.Next() {
			var id, t, c string
			var i *int
			var l, rp, rt, ir int
			r.Scan(&id, &t, &c, &i, &l, &rp, &rt, &ir)
			posts = append(posts, map[string]any{"is_reply": ir == 1, "id": id, "text": t, "created_at": c, "impressions": i, "likes": l, "replies": rp, "reposts": rt,
				"url": "https://x.com/" + a.h + "/status/" + id})
		}
		r.Close()
		accts[a.h] = map[string]any{"name": a.n, "avatar": a.a, "history": hist, "posts": posts}
	}
	out["accounts"] = accts
	// Optional display order, e.g. XSTATS_ACCOUNTS=alice,bob (first is the default tab).
	if v := strings.TrimSpace(os.Getenv("XSTATS_ACCOUNTS")); v != "" {
		var order []string
		for _, h := range strings.Split(v, ",") {
			if h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "@"))); accts[h] != nil {
				order = append(order, h)
			}
		}
		out["order"] = order
	}
	var ct []map[string]any
	r, _ := db.Query(`SELECT date,visits FROM clickthroughs ORDER BY date`)
	for r.Next() {
		var d string
		var v int
		r.Scan(&d, &v)
		ct = append(ct, map[string]any{"date": d, "visits": v})
	}
	r.Close()
	out["clickthroughs"] = ct
	return out, nil
}

func postHistory(db *sql.DB, id string) (map[string]any, error) {
	rows, err := db.Query(`SELECT account,date,text,created_at,impressions,likes,replies,reposts FROM post_metrics WHERE post_id=? ORDER BY date`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hist []map[string]any
	out := map[string]any{"id": id}
	for rows.Next() {
		var acct, d, t, c string
		var i *int
		var l, rp, rt int
		rows.Scan(&acct, &d, &t, &c, &i, &l, &rp, &rt)
		out["account"], out["text"], out["created_at"] = acct, t, c
		out["url"] = "https://x.com/" + acct + "/status/" + id
		hist = append(hist, map[string]any{"date": d, "impressions": i, "likes": l, "replies": rp, "reposts": rt})
	}
	if hist == nil {
		return nil, nil
	}
	out["history"] = hist
	return out, nil
}

func newMux(db *sql.DB, tok string) *http.ServeMux {
	sub, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	registerIcons(mux, sub)
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		d, err := apiData(db)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(d)
	})
	mux.HandleFunc("/api/post/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/post/")
		for _, ch := range id {
			if ch < '0' || ch > '9' {
				id = ""
				break
			}
		}
		if id == "" {
			http.Error(w, "bad id", 400)
			return
		}
		d, err := postHistory(db, id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if d == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(d)
	})
	mux.HandleFunc("/api/ingest", func(w http.ResponseWriter, r *http.Request) {
		if !bearerAuthorized(r, tok) {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, 20<<20))
		if err == nil {
			err = ingestBytes(db, b)
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if !bearerAuthorized(r, tok) {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", 405)
			return
		}
		serveHealth(w, db)
	})
	return mux
}

// bearerAuthorized is the ingest token check. An unset token matches nothing,
// so /api/ingest and /api/health both stay closed.
func bearerAuthorized(r *http.Request, tok string) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
		return false
	}
	return true
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: xstats serve|ingest|version [flags]")
		os.Exit(2)
	}
	if os.Args[1] == "version" {
		fmt.Print(versionOutput())
		return
	}
	fsFlags := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	dbp := fsFlags.String("db", "xstats.db", "SQLite path")
	addr := fsFlags.String("addr", "127.0.0.1:8080", "listen address")
	fsFlags.Parse(os.Args[2:])
	if _, err := statsLocation(); err != nil {
		log.Fatal(err)
	}
	db := openDB(*dbp)
	switch os.Args[1] {
	case "ingest":
		for _, f := range fsFlags.Args() {
			if err := ingest(db, f); err != nil {
				log.Fatalf("%s: %v", f, err)
			}
		}
	case "serve":
		ver, rev := versionStamp()
		mux := newMux(db, strings.TrimSpace(os.Getenv("XSTATS_INGEST_TOKEN")))
		log.Printf("xstats version %s commit %s listening on %s", ver, rev, *addr)
		log.Fatal(http.ListenAndServe(*addr, mux))
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}
