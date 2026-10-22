package main

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Analytics CSVs are the posts export and the account overview export from
// x.com/i/account_analytics. Columns are matched by header name.
//
// analyticsDateLayout is the only Date text this importer accepts. The offset
// is part of the value so the instant is unambiguous. The stored overview day
// is that instant's calendar date in XSTATS_TZ (see statsLocation), not a
// hardcoded zone. This layout has not been checked against a real export.
const analyticsDateLayout = "2006-01-02 15:04:05 -0700"

const (
	layoutPosts    = "posts"
	layoutOverview = "overview"
)

// Canonical headers, in the order an X analytics export writes them.
// Every name is required. Extra columns beside a full set are ignored.
var postsColumns = []string{
	"Post id", "Date", "Post text", "Post Link", "Impressions", "Likes",
	"Engagements", "Bookmarks", "Shares", "New follows", "Replies", "Reposts",
	"Profile visits", "Detail Expands", "URL Clicks", "Hashtag Clicks", "Permalink Clicks",
}

var overviewColumns = []string{
	"Date", "Impressions", "Likes", "Engagements", "Bookmarks", "Shares",
	"New follows", "Unfollows", "Replies", "Reposts", "Profile visits",
	"Create Post", "Video views", "Media views",
}

// Columns that appear in only one export. They decide which layout a header is.
var postsOnly = map[string]bool{
	"Post id": true, "Post text": true, "Post Link": true,
	"Detail Expands": true, "URL Clicks": true, "Hashtag Clicks": true, "Permalink Clicks": true,
}

var overviewOnly = map[string]bool{
	"Unfollows": true, "Create Post": true, "Video views": true, "Media views": true,
}

// importAnalytics reads one analytics CSV and writes it. A second call with
// the same database changes nothing.
func importAnalytics(db *sql.DB, csvPath, accountFlag string) error {
	line, err := applyAnalytics(db, csvPath, accountFlag)
	if err != nil {
		return err
	}
	fmt.Println(line)
	return nil
}

func applyAnalytics(db *sql.DB, csvPath, accountFlag string) (string, error) {
	// Fail before any write when the zone is unusable. Overview days need it,
	// and a posts file is rejected the same way so a bad env does not half-apply.
	if _, err := statsLocation(); err != nil {
		return "", err
	}
	headers, records, err := readAnalyticsCSV(csvPath)
	if err != nil {
		return "", err
	}
	layout, ignored, err := classifyAnalyticsHeader(headers)
	if err != nil {
		return "", err
	}
	switch layout {
	case layoutPosts:
		return applyPostsAnalytics(db, headers, records, normHandle(accountFlag), ignored)
	case layoutOverview:
		handle := normHandle(accountFlag)
		if handle == "" {
			return "", fmt.Errorf("analytics csv: account handle missing: pass --account for an account overview export")
		}
		return applyOverviewAnalytics(db, headers, records, handle, ignored)
	default:
		return "", fmt.Errorf("analytics csv: unknown layout %q", layout)
	}
}

func readAnalyticsCSV(path string) (headers []string, records [][]string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\uFEFF"))
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil, fmt.Errorf("analytics csv: empty file")
	}
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	raw, err := r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("analytics csv: %w", err)
	}
	headers = make([]string, len(raw))
	seen := map[string]bool{}
	for i, h := range raw {
		name := cleanAnalyticsHeader(h)
		if name == "" {
			return nil, nil, analyticsHeaderError("empty column name", headersSoFar(raw, i+1), nil)
		}
		if seen[name] {
			return nil, nil, analyticsHeaderError("duplicate column "+quoteCol(name), trimHeaders(raw), nil)
		}
		seen[name] = true
		headers[i] = name
	}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("analytics csv: %w", err)
		}
		if len(rec) != len(headers) {
			return nil, nil, fmt.Errorf("analytics csv: data row %d: got %d columns, header has %d", len(records)+1, len(rec), len(headers))
		}
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}
		records = append(records, rec)
	}
	return headers, records, nil
}

func headersSoFar(raw []string, n int) []string {
	if n > len(raw) {
		n = len(raw)
	}
	return trimHeaders(raw[:n])
}

func trimHeaders(raw []string) []string {
	out := make([]string, len(raw))
	for i, h := range raw {
		out[i] = cleanAnalyticsHeader(h)
	}
	return out
}

func cleanAnalyticsHeader(s string) string {
	s = strings.TrimPrefix(s, "\uFEFF")
	return strings.TrimSpace(s)
}

func classifyAnalyticsHeader(headers []string) (layout string, ignored []string, err error) {
	set := map[string]bool{}
	for _, h := range headers {
		set[h] = true
	}
	postsSig, overviewSig := false, false
	for h := range set {
		if postsOnly[h] {
			postsSig = true
		}
		if overviewOnly[h] {
			overviewSig = true
		}
	}
	switch {
	case postsSig && overviewSig:
		return "", nil, analyticsHeaderError("header matches both exports", headers, []string{layoutPosts, layoutOverview})
	case !postsSig && !overviewSig:
		return "", nil, analyticsHeaderError("header matches neither export", headers, []string{layoutPosts, layoutOverview})
	case postsSig:
		layout = layoutPosts
	default:
		layout = layoutOverview
	}
	want := columnsFor(layout)
	var missing []string
	for _, c := range want {
		if !set[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return "", nil, analyticsHeaderError("missing required columns: "+strings.Join(missing, ", "), headers, []string{layout})
	}
	wantSet := map[string]bool{}
	for _, c := range want {
		wantSet[c] = true
	}
	for _, h := range headers {
		if !wantSet[h] {
			ignored = append(ignored, h)
		}
	}
	return layout, ignored, nil
}

func columnsFor(layout string) []string {
	if layout == layoutPosts {
		return postsColumns
	}
	return overviewColumns
}

// analyticsHeaderError lists the columns in the file and the columns the
// importer was willing to accept. layouts names which export's columns to
// print. An empty list prints both, which is the "neither export" case.
func analyticsHeaderError(msg string, found []string, layouts []string) error {
	var b strings.Builder
	b.WriteString("analytics csv: ")
	b.WriteString(msg)
	b.WriteString("\nfound: ")
	b.WriteString(formatCols(found))
	if len(layouts) == 0 {
		layouts = []string{layoutPosts, layoutOverview}
	}
	both := len(layouts) > 1
	for _, layout := range layouts {
		label := "expected: "
		cols := overviewColumns
		if layout == layoutPosts {
			cols = postsColumns
			if both {
				label = "expected posts: "
			}
		} else if both {
			label = "expected overview: "
		}
		b.WriteString("\n")
		b.WriteString(label)
		b.WriteString(formatCols(cols))
	}
	return fmt.Errorf("%s", b.String())
}

func formatCols(cols []string) string {
	if len(cols) == 0 {
		return "(none)"
	}
	return strings.Join(cols, ", ")
}

func quoteCol(s string) string {
	return `"` + s + `"`
}

type postsRecord struct {
	row                  int
	id                   string
	impressions, replies int64
}

func applyPostsAnalytics(db *sql.DB, headers []string, records [][]string, account string, ignored []string) (string, error) {
	idx := indexHeaders(headers)
	parsed := make([]postsRecord, 0, len(records))
	seen := map[string]int{}
	for i, rec := range records {
		row := i + 1
		id := rec[idx["Post id"]]
		if id == "" {
			return "", fmt.Errorf("analytics csv: data row %d: column Post id: missing", row)
		}
		if !idPattern.MatchString(id) {
			return "", fmt.Errorf("analytics csv: data row %d: column Post id: %q: not a numeric id", row, id)
		}
		if prev, ok := seen[id]; ok {
			return "", fmt.Errorf("analytics csv: data row %d: column Post id: duplicate of data row %d", row, prev)
		}
		seen[id] = row
		if _, err := parseAnalyticsInstant(rec[idx["Date"]]); err != nil {
			return "", fmt.Errorf("analytics csv: data row %d: column Date: %w", row, err)
		}
		impr, err := parseAnalyticsCount(rec[idx["Impressions"]])
		if err != nil {
			return "", fmt.Errorf("analytics csv: data row %d: column Impressions: %w", row, err)
		}
		replies, err := parseAnalyticsCount(rec[idx["Replies"]])
		if err != nil {
			return "", fmt.Errorf("analytics csv: data row %d: column Replies: %w", row, err)
		}
		parsed = append(parsed, postsRecord{row: row, id: id, impressions: impr, replies: replies})
	}

	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	updated, filled, missing, live := 0, 0, 0, 0
	for _, p := range parsed {
		kind, slots, err := lookupPost(tx, p.id, account)
		if err != nil {
			return "", err
		}
		switch kind {
		case "missing":
			missing++
			continue
		case "live":
			// A live row is a snapshot. NULL impressions there mean the API
			// omitted the count, which is still that snapshot's value. The
			// CSV number is one total for the whole export, so it is not
			// written onto a live row or onto an archive row hidden behind one.
			live++
			continue
		}
		changed := false
		for _, slot := range slots {
			if !slot.imprNull && !slot.repliesNull {
				continue
			}
			if _, err := tx.Exec(`UPDATE post_metrics
				SET impressions=COALESCE(impressions, ?), replies=COALESCE(replies, ?)
				WHERE post_id=? AND account=? AND date=? AND source='archive'
				  AND (impressions IS NULL OR replies IS NULL)`,
				p.impressions, p.replies, p.id, slot.account, slot.date); err != nil {
				return "", err
			}
			changed = true
		}
		if changed {
			updated++
		} else {
			filled++
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return postsSummary(account, updated, filled, missing, live, ignored), nil
}

type archiveSlot struct {
	account, date         string
	imprNull, repliesNull bool
}

// lookupPost reports "missing", "live", or "archive". "live" means any
// non-archive row exists for the post (within the account filter). Archive
// slots are returned only when every matching row is source archive.
func lookupPost(tx *sql.Tx, id, account string) (string, []archiveSlot, error) {
	q := `SELECT account, date, COALESCE(source,'live'), impressions IS NULL, replies IS NULL FROM post_metrics WHERE post_id=?`
	args := []any{id}
	if account != "" {
		q += ` AND account=?`
		args = append(args, account)
	}
	rows, err := tx.Query(q, args...)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var slots []archiveSlot
	protected := false
	n := 0
	for rows.Next() {
		var slot archiveSlot
		var source string
		var imprNull, repliesNull int
		if err := rows.Scan(&slot.account, &slot.date, &source, &imprNull, &repliesNull); err != nil {
			return "", nil, err
		}
		n++
		slot.imprNull = imprNull == 1
		slot.repliesNull = repliesNull == 1
		if source != "archive" {
			protected = true
			continue
		}
		slots = append(slots, slot)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	switch {
	case n == 0:
		return "missing", nil, nil
	case protected:
		return "live", nil, nil
	default:
		return "archive", slots, nil
	}
}

func postsSummary(account string, updated, filled, missing, live int, ignored []string) string {
	who := ""
	if account != "" {
		who = " @" + account
	}
	s := fmt.Sprintf("imported analytics posts%s: %d updates, %d already filled, %d not in db, %d left live", who, updated, filled, missing, live)
	return appendIgnored(s, ignored)
}

type overviewRecord struct {
	row                   int
	day                   string
	newFollows, unfollows int64
}

func applyOverviewAnalytics(db *sql.DB, headers []string, records [][]string, account string, ignored []string) (string, error) {
	loc, err := statsLocation()
	if err != nil {
		return "", err
	}
	idx := indexHeaders(headers)
	parsed := make([]overviewRecord, 0, len(records))
	seen := map[string]int{}
	for i, rec := range records {
		row := i + 1
		instant, err := parseAnalyticsInstant(rec[idx["Date"]])
		if err != nil {
			return "", fmt.Errorf("analytics csv: data row %d: column Date: %w", row, err)
		}
		day := instant.In(loc).Format("2006-01-02")
		if prev, ok := seen[day]; ok {
			return "", fmt.Errorf("analytics csv: data row %d: column Date: same day as data row %d (%s)", row, prev, day)
		}
		seen[day] = row
		nf, err := parseAnalyticsCount(rec[idx["New follows"]])
		if err != nil {
			return "", fmt.Errorf("analytics csv: data row %d: column New follows: %w", row, err)
		}
		uf, err := parseAnalyticsCount(rec[idx["Unfollows"]])
		if err != nil {
			return "", fmt.Errorf("analytics csv: data row %d: column Unfollows: %w", row, err)
		}
		parsed = append(parsed, overviewRecord{row: row, day: day, newFollows: nf, unfollows: uf})
	}

	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	updated, stored, live := 0, 0, 0
	for _, p := range parsed {
		var hasLive int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM follower_snapshots WHERE account=? AND date=?)`, account, p.day).Scan(&hasLive); err != nil {
			return "", err
		}
		if hasLive == 1 {
			live++
			continue
		}
		var hasRow int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM daily_follows WHERE account=? AND date=?)`, account, p.day).Scan(&hasRow); err != nil {
			return "", err
		}
		if hasRow == 1 {
			// First import wins, including when a later file has different counts.
			stored++
			continue
		}
		if updated == 0 {
			if _, err := tx.Exec(`INSERT INTO accounts(handle,user_id,name,avatar) VALUES(?,?,?,?) ON CONFLICT(handle) DO NOTHING`, account, "", "", ""); err != nil {
				return "", err
			}
		}
		if _, err := tx.Exec(`INSERT INTO daily_follows(account, date, new_follows, unfollows, source) VALUES(?,?,?,?, 'analytics')`, account, p.day, p.newFollows, p.unfollows); err != nil {
			return "", err
		}
		updated++
	}
	if updated == 0 {
		// Roll back so a file that only hits live days does not create an account.
		return followsSummary(account, 0, stored, live, ignored), nil
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return followsSummary(account, updated, stored, live, ignored), nil
}

func followsSummary(account string, updated, stored, live int, ignored []string) string {
	s := fmt.Sprintf("imported analytics follows @%s: %d updates, %d already stored, %d left live", account, updated, stored, live)
	return appendIgnored(s, ignored)
}

func appendIgnored(s string, ignored []string) string {
	if len(ignored) == 0 {
		return s
	}
	return s + "; ignored columns: " + strings.Join(ignored, ", ")
}

func indexHeaders(headers []string) map[string]int {
	m := make(map[string]int, len(headers))
	for i, h := range headers {
		m[h] = i
	}
	return m
}

func parseAnalyticsInstant(cell string) (time.Time, error) {
	if cell == "" {
		return time.Time{}, fmt.Errorf("missing: want a timestamp like \"2026-10-22 03:30:00 +0000\"")
	}
	t, err := time.Parse(analyticsDateLayout, cell)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q: want a timestamp like \"2026-10-22 03:30:00 +0000\"", cell)
	}
	return t, nil
}

func parseAnalyticsCount(cell string) (int64, error) {
	if cell == "" {
		return 0, fmt.Errorf("missing: not an integer")
	}
	if !countPattern.MatchString(cell) {
		return 0, fmt.Errorf("%q: not an integer", cell)
	}
	n, err := parseInt64(cell)
	if err != nil {
		return 0, fmt.Errorf("%q: not an integer", cell)
	}
	return n, nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad digit")
		}
		next := n*10 + int64(c-'0')
		if next < n {
			return 0, fmt.Errorf("overflow")
		}
		n = next
	}
	return n, nil
}
