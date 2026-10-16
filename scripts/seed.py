#!/usr/bin/env python3
"""Generate fake snapshot files for demos and screenshots.

Usage: scripts/seed.py OUTDIR [DAYS]
Writes OUTDIR/YYYY-MM-DD.json for the last DAYS days (default 30), ending today.
All accounts, numbers and post text are made up.
"""
import json, os, random, sys
from datetime import date, datetime, timedelta, timezone

out = sys.argv[1]
days = int(sys.argv[2]) if len(sys.argv) > 2 else 30
os.makedirs(out, exist_ok=True)
random.seed(42)

ACCOUNTS = {
    "example_one": {"id": "1001", "name": "Example One", "followers": 12269, "following": 842, "tweets": 3150, "growth": 7},
    "example_two": {"id": "1002", "name": "Example Two", "followers": 1840, "following": 310, "tweets": 620, "growth": 3},
}
TEXTS = [
    "Shipped a big release today. Here is what we learned.",
    "How we test before every deploy: a thread",
    "New feature: share a doc with comments in one command.",
    "What I look for in a dev tool landing page",
    "Small servers, big wins: hosting side projects for $6 a month",
    "Hot take: the best docs are the ones with copy-paste examples.",
    "We cut our build time in half. The fix was boring.",
    "Five things I wish I knew before launching on a Friday",
    "@someone Agreed, that is the part most teams skip.",
    "Changelog for this week is up. Mostly bug fixes, one nice surprise.",
]

today = date.today()
start = today - timedelta(days=days - 1)
# Posts spread over the window; metrics grow after posting, then flatten.
posts = {h: [] for h in ACCOUNTS}
pid = 1900000000000000000
for h in ACCOUNTS:
    for i in range(14):
        pid += random.randint(10**12, 10**13)
        when = datetime.combine(start + timedelta(days=random.randint(0, days - 1)), datetime.min.time(), timezone.utc) + timedelta(hours=random.randint(13, 23))
        scale = 1.0 if h == "example_one" else 0.2
        posts[h].append({"id": str(pid), "text": TEXTS[(i + len(h)) % len(TEXTS)], "created": when,
                         "peak": int(random.randint(2000, 50000) * scale), "lr": random.uniform(0.008, 0.02)})

for n in range(days):
    d = start + timedelta(days=n)
    end_of_day = datetime.combine(d, datetime.max.time(), timezone.utc)
    bundle = {"date": d.isoformat(), "accounts": {}}
    for h, a in ACCOUNTS.items():
        followers = a["followers"] + a["growth"] * n + random.randint(-3, 6)
        ps = []
        for p in posts[h]:
            if p["created"] > end_of_day:
                continue
            age = (end_of_day - p["created"]).days + 1
            frac = min(1.0, 1 - 0.5 ** age)
            imp = int(p["peak"] * frac)
            ps.append({"id": p["id"], "text": p["text"], "created_at": p["created"].strftime("%Y-%m-%dT%H:%M:%S.000Z"),
                       "public_metrics": {"impression_count": imp, "like_count": int(imp * p["lr"]),
                                          "reply_count": int(imp * p["lr"] / 12), "retweet_count": int(imp * p["lr"] / 7), "quote_count": 0}})
        bundle["accounts"][h] = {"user": {"id": a["id"], "username": h, "name": a["name"],
                                          "public_metrics": {"followers_count": followers, "following_count": a["following"], "tweet_count": a["tweets"] + n}},
                                 "posts": ps}
    bundle["clickthroughs"] = {"visits": random.randint(20, 90)}
    with open(os.path.join(out, d.isoformat() + ".json"), "w") as f:
        json.dump(bundle, f)
print(f"wrote {days} files to {out}")
