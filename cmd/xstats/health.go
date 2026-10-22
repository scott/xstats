package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

// healthAccount is the only per-account data GET /api/health returns.
// Dates and times only: no post text and no metric counts.
type healthAccount struct {
	LatestSnapshotDate *string `json:"latest_snapshot_date"`
	LastIngest         *string `json:"last_ingest"`
}

type healthBody struct {
	Version    string                   `json:"version"`
	Commit     string                   `json:"commit"`
	ServerTime string                   `json:"server_time"`
	Accounts   map[string]healthAccount `json:"accounts"`
}

func apiHealth(db *sql.DB) (healthBody, error) {
	ver, rev := versionStamp()
	out := healthBody{
		Version:    ver,
		Commit:     rev,
		ServerTime: time.Now().UTC().Format(time.RFC3339),
		Accounts:   map[string]healthAccount{},
	}
	rows, err := db.Query(`SELECT handle, (SELECT MAX(date) FROM follower_snapshots WHERE account=accounts.handle), last_ingest FROM accounts`)
	if err != nil {
		return healthBody{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var handle string
		var snap, ingested sql.NullString
		if err := rows.Scan(&handle, &snap, &ingested); err != nil {
			return healthBody{}, err
		}
		acc := healthAccount{}
		if snap.Valid {
			s := snap.String
			acc.LatestSnapshotDate = &s
		}
		if ingested.Valid {
			s := ingested.String
			acc.LastIngest = &s
		}
		out.Accounts[handle] = acc
	}
	if err := rows.Err(); err != nil {
		return healthBody{}, err
	}
	return out, nil
}

func serveHealth(w http.ResponseWriter, db *sql.DB) {
	d, err := apiHealth(db)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(d)
}
