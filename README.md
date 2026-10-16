# xstats

A small, self-hosted X (Twitter) stats dashboard. Track follower growth and per-post metrics (impressions, likes, replies, reposts) for one or more accounts, with daily history and a per-post drill-in. Ships as a single Go binary.

![Dashboard, dark theme, fake demo data](docs/dark-desktop.png)

## Features

- Account switcher for any number of accounts
- Current follower count with 7 and 30 day deltas
- Follower growth chart built from daily snapshots
- Recent posts table (last 30 days), retweets excluded, replies hidden behind a toggle
- Per-post drill-in with impressions, likes, replies and reposts over time
- Light and dark themes (follows the system, remembers your choice), responsive down to phone width
- Optional "clickthroughs from X" card if you feed in referral visit counts

| Post drill-in | Mobile, light |
|---|---|
| ![Drill-in](docs/drillin-dark-desktop.png) | ![Mobile](docs/light-mobile.png) |

## Architecture

```
  your snapshot job                 server
 (any X API client)     HTTPS    +-------------------------------+
  raw JSON bundle  ------------> | Caddy (TLS, basic auth)       |
  POST /api/ingest   Bearer      |   /api/ingest: token only     |
                                 |   everything else: basic auth |
                                 |            |                  |
                                 |   xstats serve (127.0.0.1)    |
                                 |   embedded frontend + API     |
                                 |            |                  |
                                 |   SQLite (xstats.db)          |
                                 +-------------------------------+
```

- **One binary.** `xstats` embeds the static frontend with `go:embed` and uses `modernc.org/sqlite`, so it builds with `CGO_ENABLED=0` and has no runtime dependencies.
- **SQLite.** Tables: `accounts`, `follower_snapshots` (one row per account per day), `post_metrics` (one row per post per day), `clickthroughs` (one row per day). Re-ingesting a day upserts, so it is always safe to rerun.
- **Ingest API.** `POST /api/ingest` with `Authorization: Bearer <token>` accepts a snapshot bundle. The token comes from `XSTATS_INGEST_TOKEN`; if it is unset, ingest is disabled.
- **Read API.** `GET /api/data` (all accounts, history, recent posts) and `GET /api/post/<id>` (daily history for one post).
- **Caddy** terminates TLS with automatic certificates and puts basic auth in front of everything except `/api/ingest`.

xstats does not call the X API itself. Collect data with whatever client you like and hand it the raw JSON.

## Build

Requires Go 1.23+.

```
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/xstats ./cmd/xstats
```

## Run locally

```
bin/xstats ingest --db data/xstats.db snapshot.json
bin/xstats serve  --db data/xstats.db --addr 127.0.0.1:8080
```

Open http://127.0.0.1:8080. Handy URL options: `?theme=dark|light`, `?acct=<handle>`, `#post/<id>`.

## Demo with fake data

`scripts/seed.py` writes 30 days of made-up snapshots for two fictional accounts (`example_one`, `example_two`). The screenshots in this README come from it.

```
scripts/seed.py /tmp/seed 30
bin/xstats ingest --db /tmp/demo.db /tmp/seed/*.json
bin/xstats serve  --db /tmp/demo.db --addr 127.0.0.1:8080
```

## Snapshot format

One JSON file per day. The `user` and `posts` objects are the X API v2 responses as returned, so you can save them without reshaping.

```json
{
  "date": "2026-10-14",
  "accounts": {
    "example": {
      "user":  { "id": "123", "username": "example", "name": "Example",
                 "profile_image_url": "https://...",
                 "public_metrics": { "followers_count": 100, "following_count": 50, "tweet_count": 400 } },
      "posts": [ { "id": "1846...", "text": "Hello", "created_at": "2026-10-14T15:00:00.000Z",
                   "in_reply_to_user_id": "",
                   "public_metrics": { "impression_count": 120, "like_count": 4, "reply_count": 1,
                                       "retweet_count": 2, "quote_count": 0 } } ]
    }
  },
  "clickthroughs": { "visits": 12 }
}
```

- `date` defaults to today if omitted. `clickthroughs` is optional.
- Posts starting with `RT @` are skipped. Posts starting with `@` or with `in_reply_to_user_id` are flagged as replies.
- Reposts shown in the UI are `retweet_count + quote_count`.

Suggested X API v2 calls per day (3 reads for two accounts):

- `GET /2/users/by?usernames=a,b&user.fields=public_metrics,profile_image_url,name`
- `GET /2/users/:id/tweets?max_results=50&tweet.fields=public_metrics,created_at,in_reply_to_user_id` per account

Push a day to a running server:

```
STATS_URL=https://stats.example.com snapshot/push.sh snapshot-2026-10-14.json
```

`push.sh` reads the token from `~/.config/xstats/ingest_token` (override with `XSTATS_TOKEN_FILE`) and passes it to curl on stdin, never on the command line.

## Deploy

Target: a fresh Ubuntu 22.04 or 24.04 server (512 MB is plenty). Nothing is compiled on the server.

1. Point a DNS A record (for example `stats.example.com`) at the server's IPv4 address. Caddy needs this to get a certificate.
2. Create an ingest token on your machine:
   ```
   mkdir -p ~/.config/xstats && (umask 077; openssl rand -hex 32 > ~/.config/xstats/ingest_token)
   ```
3. Build, then deploy:
   ```
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/xstats ./cmd/xstats
   read -rs BASIC_AUTH_PASSWORD && export BASIC_AUTH_PASSWORD
   XSTATS_HOST=203.0.113.10 XSTATS_DOMAIN=stats.example.com SSH_KEY=~/.ssh/id_ed25519 deploy/deploy.sh
   unset BASIC_AUTH_PASSWORD
   ```

| Variable | Default | Meaning |
|---|---|---|
| `XSTATS_HOST` | required | Server IP or hostname for SSH |
| `XSTATS_DOMAIN` | required | Public hostname served by Caddy |
| `BASIC_AUTH_PASSWORD` | required | Dashboard password |
| `XSTATS_AUTH_USER` | `admin` | Dashboard username |
| `XSTATS_SSH_USER` | `root` | SSH user |
| `SSH_KEY` | `~/.ssh/id_ed25519` | SSH private key path |
| `XSTATS_TOKEN_FILE` | `~/.config/xstats/ingest_token` | Ingest token file |

`deploy.sh` installs Caddy from its official apt repo, creates an `xstats` system user, installs the binary to `/usr/local/bin`, the unit from `deploy/xstats.service`, and the token in `/etc/xstats/env` (mode 0640). The password is sent over SSH stdin and hashed on the server with `caddy hash-password`; only the bcrypt hash is stored. The script is idempotent.

To update only the binary later:

```
scp bin/xstats root@$XSTATS_HOST:/tmp/xstats.new
ssh root@$XSTATS_HOST 'install -m 0755 /tmp/xstats.new /usr/local/bin/xstats && systemctl restart xstats'
```

## Mockup

`mockup/` holds the original static design with example data.

## License

MIT, see [LICENSE](LICENSE).
