# Twitter Feed Workflow

Discovery workflow for feeds with `kind=twitter`. Go owns dedup and storage; this
workflow only lists tweets and pushes them as entries. Correctness rests on guid
dedup, **not** a watermark — a failed run just discovers nothing, and the next run
re-lists and Go drops the duplicates.

## 1. Identify Twitter feeds

Use `recally__feed_list` to list feeds. Process each feed whose `kind` is `twitter`. The feed's metadata may hold the stable numeric X user id (rename-proof); fall back to the handle in the feed `url` if it is missing.

## 2. List recent tweets

Fetch the public FxEmbed statuses API with the `web` skill; a JSON endpoint is
printed verbatim. Prefer the numeric id; `since` (Unix timestamp) is a
best-effort optimization only — never rely on it for correctness.

```bash
bun $SKILL/scripts/web.ts fetch "https://api.fxtwitter.com/2/profile/id:<numeric-user-id>/statuses?count=20" --out statuses.json
```

A handle without `@` works in place of `id:<numeric-user-id>`. The first line
of the file is the untrusted-source note; read the rest with `jq` or `python3`
instead of printing it.

For each returned status:

- **Skip retweets** by default (`is_repost` / `is_retweet` true).
- Map fields:
  - `guid` = tweet id (stable; the dedup key)
  - `url` = tweet url
  - `title` = tweet text; when empty, fall back to `(media: <author>)`

## 3. Push entries (Go dedups)

Use `recally__entry_add` with `feed_id`, tweet ID as `guid`, tweet URL as
`url`, and tweet text as `title`.

Add one feed entry per tweet. The result reports whether the entry was inserted or already existed. Pinned and edited tweets
are handled automatically by guid dedup — just push them all. Stop pushing a feed
once you hit a run of duplicate results if you want to save calls, but pushing extras
is harmless.

## 4. Process pending entries

New entries land as `pending`, exactly like RSS entries. Process them with the
standard [save-workflow.md](save-workflow.md) using `source_type=twitter`
(read the tweet URL with the `web` skill
(`bun $SKILL/scripts/web.ts fetch "https://api.fxtwitter.com/2/status/<tweet-id>"`)
and save it via `content_path`), then
mark each entry saved / skipped / error as described in
[rss-workflow.md](rss-workflow.md).
