package main

import (
	"database/sql"
	"time"
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
// Score is nil when Impressions is 0 (no divide by zero). NewFollowers on the
// first snapshot is 0: there is no prior day to subtract. InProgress is true
// when Date equals today, because that snapshot can still be re-pushed.
// ScoreDelta is this score minus the mean score of earlier days in the
// previous 7 calendar days; days with no score are skipped. Nil when this
// day has no score or the window has none.
type dayEngagement struct {
	Date                                    string
	Comments, NewFollowers, Reshares, Likes int
	Impressions, Points                     int
	Score, ScoreDelta                       *float64
	InProgress                              bool
	Top                                     *scoredPost
}

// utcDay is the UTC calendar day of a post's created_at. That is the same
// clock as the 30-day posts window (UTC). Snapshot dates are the ingest
// bundle's YYYY-MM-DD; they line up when the bundle date is that UTC day,
// which is how scripts/seed.py and the README example are written.
func utcDay(createdAt string) (string, bool) {
	s := createdAt
	if s == "" {
		return "", false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("2006-01-02"), true
		}
	}
	if len(s) >= 10 {
		if _, err := time.Parse("2006-01-02", s[:10]); err == nil {
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
// Posts whose UTC creation day is not a snapshot day are left out (the chart
// x-axis is the snapshot series). A post is counted once, from its latest
// snapshot, so re-pushing a day replaces numbers instead of adding them.
func aggregateDays(hist []histDay, posts []scoredPost, today string) []dayEngagement {
	byDay := map[string]*dayBucket{}
	for _, p := range posts {
		day, ok := utcDay(p.CreatedAt)
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
		if e.Impressions > 0 {
			sc := float64(e.Points) * 1000 / float64(e.Impressions)
			e.Score = &sc
		}
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
