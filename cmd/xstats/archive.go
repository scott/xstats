package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Archive files are JavaScript assignments, not bare JSON. Public documentation
// of the X data download describes tweets.js as:
//
//	window.YTD.tweets.part0 = [ {"tweet": {...}} ]
//
// Counts are strings. There is no reply_count or quote_count. note-tweet.js
// is not read; long posts keep full_text from the tweets file.
const archiveCreatedAt = "Mon Jan 02 15:04:05 -0700 2006"

var (
	tweetsPrefix  = regexp.MustCompile(`^window\.YTD\.tweets\.part[0-9]+ = `)
	accountPrefix = regexp.MustCompile(`^window\.YTD\.account\.part[0-9]+ = `)
	countPattern  = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	idPattern     = regexp.MustCompile(`^[0-9]+$`)
	replyFields   = []string{
		"in_reply_to_status_id",
		"in_reply_to_status_id_str",
		"in_reply_to_user_id",
		"in_reply_to_user_id_str",
		"in_reply_to_screen_name",
	}
)

type archiveError struct {
	file  string
	index int
	field string
	msg   string
}

func (e *archiveError) Error() string {
	s := e.file
	if e.index >= 0 {
		s += fmt.Sprintf(": item %d", e.index)
	}
	if e.field != "" {
		s += ": field " + e.field
	}
	if e.msg != "" {
		s += ": " + e.msg
	}
	return s
}

func archiveErr(file string, index int, field, msg string) error {
	return &archiveError{file: file, index: index, field: field, msg: msg}
}

type namedBytes struct {
	name string
	body []byte
}

type parsedPost struct {
	id, text, createdAt string
	likes, reposts      int
	isReply             int
	skipRT              bool
}

type parsedAccount struct {
	handle, userID, name string
}

// importArchive loads an X data archive zip, or an extracted directory that
// contains data/, into the database. The snapshot date is today in XSTATS_TZ.
func importArchive(db *sql.DB, path, accountFlag string) error {
	return importArchiveAt(db, path, accountFlag, time.Now())
}

// importArchiveAt is importArchive with a fixed clock. One archive row is kept
// per post. A later import does not update it. A post with any live row is skipped.
func importArchiveAt(db *sql.DB, archivePath, accountFlag string, now time.Time) error {
	day, err := importDay(now)
	if err != nil {
		return err
	}
	tweets, accountFile, err := loadArchive(archivePath)
	if err != nil {
		return err
	}
	var account *parsedAccount
	if accountFile != nil {
		account, err = parseAccount(accountFile.name, accountFile.body)
		if err != nil {
			return err
		}
	}
	handle, err := resolveArchiveHandle(account, accountFlag)
	if err != nil {
		return err
	}
	var posts []parsedPost
	seen := map[string]string{}
	for _, f := range tweets {
		batch, err := parseTweets(f.name, f.body)
		if err != nil {
			return err
		}
		for i, p := range batch {
			if prev, ok := seen[p.id]; ok {
				return archiveErr(f.name, i, "id_str", "duplicate of "+prev)
			}
			seen[p.id] = fmt.Sprintf("%s item %d", f.name, i)
			posts = append(posts, p)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	userID, name := "", ""
	if account != nil {
		userID, name = account.userID, account.name
	}
	if _, err := tx.Exec(`INSERT INTO accounts(handle,user_id,name,avatar) VALUES(?,?,?,?) ON CONFLICT(handle) DO NOTHING`, handle, userID, name, ""); err != nil {
		return err
	}
	inserted, unchanged, liveKept, rts := 0, 0, 0, 0
	for _, p := range posts {
		if p.skipRT {
			rts++
			continue
		}
		var live int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM post_metrics WHERE post_id=? AND COALESCE(source,'live')='live')`, p.id).Scan(&live); err != nil {
			return err
		}
		if live == 1 {
			liveKept++
			continue
		}
		var archived int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM post_metrics WHERE post_id=? AND source='archive')`, p.id).Scan(&archived); err != nil {
			return err
		}
		if archived == 1 {
			unchanged++
			continue
		}
		if _, err := tx.Exec(`INSERT INTO post_metrics(post_id,account,date,text,created_at,impressions,likes,replies,reposts,is_reply,source) VALUES(?,?,?,?,?,NULL,?,0,?,?, 'archive')`,
			p.id, handle, day, p.text, p.createdAt, p.likes, p.reposts, p.isReply); err != nil {
			return err
		}
		inserted++
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("imported archive %s @%s: %d new posts, %d already archived, %d left live, %d retweets skipped\n", day, handle, inserted, unchanged, liveKept, rts)
	return nil
}

// importDay is the snapshot date written on archive rows: the calendar day of
// now in XSTATS_TZ. The zone is not hardcoded.
func importDay(now time.Time) (string, error) {
	loc, err := statsLocation()
	if err != nil {
		return "", err
	}
	return now.In(loc).Format("2006-01-02"), nil
}

func resolveArchiveHandle(account *parsedAccount, flag string) (string, error) {
	flag = normHandle(flag)
	if account != nil {
		if flag != "" && flag != account.handle {
			return "", archiveErr("data/account.js", 0, "username", fmt.Sprintf("%q does not match --account %q", account.handle, flag))
		}
		return account.handle, nil
	}
	if flag == "" {
		return "", fmt.Errorf("account handle missing: pass --account or include data/account.js")
	}
	return flag, nil
}

func normHandle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	return strings.ToLower(s)
}

func loadArchive(archivePath string) (tweets []namedBytes, account *namedBytes, err error) {
	fi, err := os.Stat(archivePath)
	if err != nil {
		return nil, nil, err
	}
	if fi.IsDir() {
		tweets, account, err = loadArchiveDir(archivePath)
	} else {
		tweets, account, err = loadArchiveZip(archivePath)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(tweets) == 0 {
		return nil, nil, fmt.Errorf("%s: no data/tweets.js or data/tweets-part*.js", archivePath)
	}
	sort.Slice(tweets, func(i, j int) bool {
		oi, oj := tweetFileOrder(tweets[i].name), tweetFileOrder(tweets[j].name)
		if oi != oj {
			return oi < oj
		}
		return tweets[i].name < tweets[j].name
	})
	return tweets, account, nil
}

func tweetFileOrder(name string) int {
	base := path.Base(name)
	if base == "tweets.js" {
		return -1
	}
	n := strings.TrimSuffix(strings.TrimPrefix(base, "tweets-part"), ".js")
	i, err := strconv.Atoi(n)
	if err != nil {
		return 1 << 30
	}
	return i
}

func isTweetFile(file string) bool {
	if file == "tweets.js" {
		return true
	}
	if !strings.HasPrefix(file, "tweets-part") || !strings.HasSuffix(file, ".js") {
		return false
	}
	n := strings.TrimSuffix(strings.TrimPrefix(file, "tweets-part"), ".js")
	if n == "" {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func loadArchiveDir(root string) ([]namedBytes, *namedBytes, error) {
	dataDir := filepath.Join(root, "data")
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("%s: no data/tweets.js or data/tweets-part*.js", root)
		}
		return nil, nil, err
	}
	var tweets []namedBytes
	var account *namedBytes
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		logical := "data/" + name
		switch {
		case isTweetFile(name):
			b, err := os.ReadFile(filepath.Join(dataDir, name))
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", logical, err)
			}
			tweets = append(tweets, namedBytes{name: logical, body: b})
		case name == "account.js":
			b, err := os.ReadFile(filepath.Join(dataDir, name))
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", logical, err)
			}
			account = &namedBytes{name: logical, body: b}
		}
	}
	return tweets, account, nil
}

func loadArchiveZip(zipPath string) ([]namedBytes, *namedBytes, error) {
	z, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: not a zip archive or directory", zipPath)
	}
	defer z.Close()
	type hit struct {
		root string
		file namedBytes
	}
	var tweets []hit
	var accounts []hit
	for _, f := range z.File {
		if f.FileInfo().IsDir() {
			continue
		}
		root, logical, kind, err := classifyArchivePath(f.Name)
		if err != nil {
			return nil, nil, err
		}
		if kind == "" {
			continue
		}
		b, err := readZipFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", logical, err)
		}
		h := hit{root: root, file: namedBytes{name: logical, body: b}}
		if kind == "account" {
			accounts = append(accounts, h)
		} else {
			tweets = append(tweets, h)
		}
	}
	if len(tweets) == 0 {
		return nil, nil, fmt.Errorf("%s: no data/tweets.js or data/tweets-part*.js", zipPath)
	}
	root := tweets[0].root
	out := make([]namedBytes, 0, len(tweets))
	seen := map[string]bool{}
	for _, t := range tweets {
		if t.root != root {
			return nil, nil, fmt.Errorf("%s: more than one data directory with tweets", zipPath)
		}
		if seen[t.file.name] {
			return nil, nil, fmt.Errorf("%s: duplicate %s", zipPath, t.file.name)
		}
		seen[t.file.name] = true
		out = append(out, t.file)
	}
	var account *namedBytes
	other := false
	for _, a := range accounts {
		if a.root != root {
			other = true
			continue
		}
		if account != nil {
			return nil, nil, fmt.Errorf("%s: duplicate data/account.js", zipPath)
		}
		account = &a.file
	}
	if account == nil && other {
		return nil, nil, fmt.Errorf("%s: more than one data directory", zipPath)
	}
	return out, account, nil
}

// classifyArchivePath returns the archive root (everything before data/),
// the logical data/... name, and "tweets" or "account". Other files are ignored.
func classifyArchivePath(name string) (root, logical, kind string, err error) {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimPrefix(name, "/")
	if name == "" || strings.HasSuffix(name, "/") {
		return "", "", "", nil
	}
	parts := strings.Split(name, "/")
	for _, p := range parts {
		if p == ".." || p == "." || p == "" {
			return "", "", "", fmt.Errorf("archive: bad path %q", name)
		}
	}
	if len(parts) < 2 || parts[len(parts)-2] != "data" {
		return "", "", "", nil
	}
	file := parts[len(parts)-1]
	switch {
	case isTweetFile(file):
		kind = "tweets"
	case file == "account.js":
		kind = "account"
	default:
		return "", "", "", nil
	}
	return strings.Join(parts[:len(parts)-2], "/"), "data/" + file, kind, nil
}

func readZipFile(f *zip.File) ([]byte, error) {
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func stripAssignment(file string, body []byte, prefix *regexp.Regexp) ([]byte, error) {
	m := prefix.Find(body)
	if m == nil {
		return nil, archiveErr(file, -1, "", "unknown prefix")
	}
	rest := bytes.TrimSpace(body[len(m):])
	if len(rest) == 0 {
		return nil, archiveErr(file, -1, "", "not a JSON array")
	}
	return rest, nil
}

func parseAccount(file string, body []byte) (*parsedAccount, error) {
	rest, err := stripAssignment(file, body, accountPrefix)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err := decodeJSONArray(rest, &items); err != nil {
		return nil, archiveErr(file, -1, "", "not a JSON array")
	}
	if len(items) != 1 {
		return nil, archiveErr(file, -1, "", "expected one account")
	}
	wrap, err := objectFields(file, 0, items[0])
	if err != nil {
		return nil, err
	}
	raw, ok := wrap["account"]
	if !ok || jsonKind(raw) == "null" {
		return nil, archiveErr(file, 0, "account", "missing")
	}
	if jsonKind(raw) != "object" {
		return nil, archiveErr(file, 0, "account", "want object, got "+jsonKind(raw))
	}
	fields, err := objectFields(file, 0, raw)
	if err != nil {
		return nil, archiveErr(file, 0, "account", "want object")
	}
	username, err := requireString(file, 0, fields, "username")
	if err != nil {
		return nil, err
	}
	handle := normHandle(username)
	if handle == "" {
		return nil, archiveErr(file, 0, "username", "missing")
	}
	userID, _, err := optionalString(file, 0, fields, "accountId")
	if err != nil {
		return nil, err
	}
	name, _, err := optionalString(file, 0, fields, "accountDisplayName")
	if err != nil {
		return nil, err
	}
	return &parsedAccount{handle: handle, userID: userID, name: name}, nil
}

func parseTweets(file string, body []byte) ([]parsedPost, error) {
	rest, err := stripAssignment(file, body, tweetsPrefix)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err := decodeJSONArray(rest, &items); err != nil {
		return nil, archiveErr(file, -1, "", "not a JSON array")
	}
	out := make([]parsedPost, 0, len(items))
	for i, item := range items {
		p, err := parseTweet(file, i, item)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func parseTweet(file string, index int, item json.RawMessage) (parsedPost, error) {
	wrap, err := objectFields(file, index, item)
	if err != nil {
		return parsedPost{}, err
	}
	raw, ok := wrap["tweet"]
	if !ok || jsonKind(raw) == "null" {
		return parsedPost{}, archiveErr(file, index, "tweet", "missing")
	}
	if jsonKind(raw) != "object" {
		return parsedPost{}, archiveErr(file, index, "tweet", "want object, got "+jsonKind(raw))
	}
	tw, err := objectFields(file, index, raw)
	if err != nil {
		return parsedPost{}, archiveErr(file, index, "tweet", "want object")
	}
	id, err := requireString(file, index, tw, "id_str")
	if err != nil {
		return parsedPost{}, err
	}
	if !idPattern.MatchString(id) {
		return parsedPost{}, archiveErr(file, index, "id_str", "not a numeric id")
	}
	createdRaw, err := requireString(file, index, tw, "created_at")
	if err != nil {
		return parsedPost{}, err
	}
	created, err := time.Parse(archiveCreatedAt, createdRaw)
	if err != nil {
		return parsedPost{}, archiveErr(file, index, "created_at", `want a timestamp like "Wed Oct 10 20:19:24 +0000 2018"`)
	}
	text, err := requireString(file, index, tw, "full_text")
	if err != nil {
		return parsedPost{}, err
	}
	likes, err := requireCount(file, index, tw, "favorite_count")
	if err != nil {
		return parsedPost{}, err
	}
	reposts, err := requireCount(file, index, tw, "retweet_count")
	if err != nil {
		return parsedPost{}, err
	}
	reply, err := replyFlag(file, index, tw, text)
	if err != nil {
		return parsedPost{}, err
	}
	return parsedPost{
		id: id, text: text, createdAt: created.UTC().Format(time.RFC3339),
		likes: likes, reposts: reposts, isReply: reply,
		skipRT: strings.HasPrefix(text, "RT @"),
	}, nil
}

func replyFlag(file string, index int, tw map[string]json.RawMessage, text string) (int, error) {
	reply := 0
	if strings.HasPrefix(text, "@") {
		reply = 1
	}
	for _, field := range replyFields {
		s, ok, err := optionalString(file, index, tw, field)
		if err != nil {
			return 0, err
		}
		if ok && s != "" {
			reply = 1
		}
	}
	return reply, nil
}

func decodeJSONArray(rest []byte, dest *[]json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(rest))
	if err := dec.Decode(dest); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing data")
	}
	return nil
}

func objectFields(file string, index int, raw json.RawMessage) (map[string]json.RawMessage, error) {
	if jsonKind(raw) != "object" {
		return nil, archiveErr(file, index, "", "want object, got "+jsonKind(raw))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, archiveErr(file, index, "", "want object")
	}
	return obj, nil
}

func requireString(file string, index int, obj map[string]json.RawMessage, field string) (string, error) {
	raw, ok := obj[field]
	if !ok || jsonKind(raw) == "null" {
		return "", archiveErr(file, index, field, "missing")
	}
	if jsonKind(raw) != "string" {
		return "", archiveErr(file, index, field, "want string, got "+jsonKind(raw))
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", archiveErr(file, index, field, "want string")
	}
	return s, nil
}

func optionalString(file string, index int, obj map[string]json.RawMessage, field string) (string, bool, error) {
	raw, ok := obj[field]
	if !ok || jsonKind(raw) == "null" {
		return "", false, nil
	}
	if jsonKind(raw) != "string" {
		return "", false, archiveErr(file, index, field, "want string, got "+jsonKind(raw))
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, archiveErr(file, index, field, "want string")
	}
	return s, true, nil
}

func requireCount(file string, index int, obj map[string]json.RawMessage, field string) (int, error) {
	s, err := requireString(file, index, obj, field)
	if err != nil {
		return 0, err
	}
	if !countPattern.MatchString(s) {
		return 0, archiveErr(file, index, field, "not an integer")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, archiveErr(file, index, field, "not an integer")
	}
	return n, nil
}

func jsonKind(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "empty"
	}
	switch raw[0] {
	case '"':
		return "string"
	case 't', 'f':
		return "bool"
	case 'n':
		return "null"
	case '{':
		return "object"
	case '[':
		return "array"
	default:
		return "number"
	}
}
