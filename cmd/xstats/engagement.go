package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "time/tzdata" // America/Denver on a host image that has no zoneinfo files
)

// Engagement weights. comments are reply_count, reshares are the reposts
// column (ingest stores retweet_count + quote_count), likes are like_count.
// new_followers is not a post metric; it is the day-over-day follower change.
const (
	weightComments     = 3
	weightNewFollowers = 4
	weightReshares     = 2
	weightLikes        = 1
)

func engagementWeights() map[string]int {
	return map[string]int{
		"comments":      weightComments,
		"new_followers": weightNewFollowers,
		"reshares":      weightReshares,
		"likes":         weightLikes,
	}
}

func dayLevelPoints(comments, newFollowers, reshares, likes int) int {
	return weightComments*comments + weightNewFollowers*newFollowers + weightReshares*reshares + weightLikes*likes
}

func postLevelPoints(replies, reshares, likes int) int {
	return weightComments*replies + weightReshares*reshares + weightLikes*likes
}

// histDay is one follower snapshot, already ordered by date.
type histDay struct {
	Date      string
	Followers int
}

// scoredPost is one post's latest snapshot. Reposts must already be
// retweets + quotes.
type scoredPost struct {
	ID, Text, CreatedAt                  string
	Impressions, Likes, Replies, Reposts int
	Points                               int
}

// dayEngagement is the per-account, per-snapshot-day score.
//
// Score is the weighted points total, including days with 0 impressions.
// Points is the same number, kept so older clients still find it. A negative
// total (followers dropped) is returned; the chart draws no bar unless the
// score is greater than 0. NewFollowers on the first snapshot is 0: there is
// no prior day to subtract. InProgress is true when Date equals today,
// because that snapshot can still be re-pushed. ScoreDelta is this score
// minus the mean score of earlier snapshot days in the previous 7 calendar
// days. Nil when the window has no scored day.
type dayEngagement struct {
	Date                                    string
	Comments, NewFollowers, Reshares, Likes int
	Impressions, Points                     int
	Score, ScoreDelta                       *float64
	InProgress                              bool
	Top                                     *scoredPost
}

// statsLocation is the calendar used for engagement days and for "today".
// XSTATS_TZ overrides it; the default is America/Denver, which is the day
// the hourly ingest closes. Read on each call so a change is not cached.
func statsLocation() (*time.Location, error) {
	name := strings.TrimSpace(os.Getenv("XSTATS_TZ"))
	if name == "" {
		name = "America/Denver"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("XSTATS_TZ %q: %w", name, err)
	}
	return loc, nil
}

// postDay is the calendar day of created_at in loc.
// A bare YYYY-MM-DD is that day; shifting it would move UTC midnight into
// the previous evening. A full timestamp is converted into loc, so 23:30
// Mountain Time (05:30 UTC the next day) stays on the Mountain Time day.
func postDay(createdAt string, loc *time.Location) (string, bool) {
	s := strings.TrimSpace(createdAt)
	if s == "" || loc == nil {
		return "", false
	}
	if len(s) == 10 {
		if _, err := time.Parse("2006-01-02", s); err == nil {
			return s, true
		}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.In(loc).Format("2006-01-02"), true
		}
	}
	if len(s) >= 10 {
		if _, err := time.Parse("2006-01-02", s[:10]); err == nil {
			if len(s) > 10 && (s[10] == 'T' || s[10] == 't') {
				return "", false
			}
			return s[:10], true
		}
	}
	return "", false
}

type dayBucket struct {
	comments, reshares, likes, impressions int
	top                                    scoredPost
	hasTop                                 bool
}

func (b *dayBucket) add(p scoredPost) {
	b.comments += p.Replies
	b.reshares += p.Reposts
	b.likes += p.Likes
	b.impressions += p.Impressions
	pts := postLevelPoints(p.Replies, p.Reposts, p.Likes)
	p.Points = pts
	if !b.hasTop || pts > b.top.Points || (pts == b.top.Points && (p.Impressions > b.top.Impressions || (p.Impressions == b.top.Impressions && p.ID < b.top.ID))) {
		b.top = p
		b.hasTop = true
	}
}

// aggregateDays buckets latest-per-post metrics onto follower snapshot days.
// created_at is interpreted in loc (see postDay). A post whose day is not a
// snapshot day is left out. A post is counted once, from its latest snapshot,
// so re-pushing a day replaces numbers instead of adding them.
// today is the calendar day in that same zone; it sets InProgress.
func aggregateDays(hist []histDay, posts []scoredPost, today string, loc *time.Location) []dayEngagement {
	byDay := map[string]*dayBucket{}
	for _, p := range posts {
		day, ok := postDay(p.CreatedAt, loc)
		if !ok {
			continue
		}
		b := byDay[day]
		if b == nil {
			b = &dayBucket{}
			byDay[day] = b
		}
		b.add(p)
	}
	out := make([]dayEngagement, len(hist))
	for i, s := range hist {
		nf := 0
		if i > 0 {
			nf = s.Followers - hist[i-1].Followers
		}
		e := dayEngagement{Date: s.Date, NewFollowers: nf, InProgress: s.Date == today}
		if b := byDay[s.Date]; b != nil {
			e.Comments = b.comments
			e.Reshares = b.reshares
			e.Likes = b.likes
			e.Impressions = b.impressions
			if b.hasTop {
				top := b.top
				e.Top = &top
			}
		}
		e.Points = dayLevelPoints(e.Comments, e.NewFollowers, e.Reshares, e.Likes)
		sc := float64(e.Points)
		e.Score = &sc
		out[i] = e
	}
	for i := range out {
		out[i].ScoreDelta = priorWeekDelta(out, i)
	}
	return out
}

// priorWeekDelta is score[i] minus the average score of days in [date-7d, date).
func priorWeekDelta(days []dayEngagement, i int) *float64 {
	if days[i].Score == nil {
		return nil
	}
	cur, err := time.Parse("2006-01-02", days[i].Date)
	if err != nil {
		return nil
	}
	start := cur.AddDate(0, 0, -7)
	var sum float64
	n := 0
	for j := 0; j < i; j++ {
		if days[j].Score == nil {
			continue
		}
		d, err := time.Parse("2006-01-02", days[j].Date)
		if err != nil || d.Before(start) || !d.Before(cur) {
			continue
		}
		sum += *days[j].Score
		n++
	}
	if n == 0 {
		return nil
	}
	delta := *days[i].Score - sum/float64(n)
	return &delta
}

func loadLatestPosts(db *sql.DB, account string) ([]scoredPost, error) {
	// One row per post: the newest snapshot. Re-pushing a date replaces that
	// date's rows at ingest, and MAX(date) then drops the stale copy.
	rows, err := db.Query(`SELECT post_id, text, created_at, impressions, likes, replies, reposts
		FROM post_metrics m
		WHERE account=? AND date=(SELECT MAX(date) FROM post_metrics WHERE post_id=m.post_id AND account=m.account)`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scoredPost
	for rows.Next() {
		var p scoredPost
		if err := rows.Scan(&p.ID, &p.Text, &p.CreatedAt, &p.Impressions, &p.Likes, &p.Replies, &p.Reposts); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func engagementJSON(handle string, e dayEngagement) map[string]any {
	m := map[string]any{
		"comments":      e.Comments,
		"new_followers": e.NewFollowers,
		"reshares":      e.Reshares,
		"likes":         e.Likes,
		"impressions":   e.Impressions,
		"points":        e.Points,
		"in_progress":   e.InProgress,
		"score":         nil,
		"score_delta":   nil,
		"top_post":      nil,
	}
	if e.Score != nil {
		m["score"] = *e.Score
	}
	if e.ScoreDelta != nil {
		m["score_delta"] = *e.ScoreDelta
	}
	if e.Top != nil {
		m["top_post"] = map[string]any{
			"id":     e.Top.ID,
			"text":   e.Top.Text,
			"url":    "https://x.com/" + handle + "/status/" + e.Top.ID,
			"points": e.Top.Points,
		}
	}
	return m
}
