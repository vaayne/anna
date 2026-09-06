# RSS Batch Processing Workflow

Feed polling and entry bookkeeping run through the native `recally_*` tools. Saving
articles uses the native tool per [save-workflow.md](save-workflow.md).

## 1. Poll Feeds

Use `recally__feed_poll` with optional `limit` for the maximum
number of new entries to fetch per feed. Omit `id` to poll all enabled RSS feeds;
non-RSS feeds are skipped server-side. Pass `id` only when you need to poll one
feed. The response contains feed results; each result has a `new_entries` array
of pending entries.

If you need to resume or inspect pending work, use
`recally__entry_list` with `feed_id`, `status=pending`, and optional `page_size` /
`page_token`.
After marking entries via `entry_update`, re-list from the start instead of paging — the pending set shifts as entries are processed.

## 2. Process Entries Sequentially

Loop over pending entries one at a time. For each entry, call `session_create`
with a focused request to run the full [save workflow](save-workflow.md) with
`source_type=rss`, then update that entry before starting the next one. The call
is synchronous. If the focused Session needs a correction, continue its Session
ID with `session_send` before moving on.

Use `recally__entry_update` with `feed_id`, the entry `id`, and `status`:
`saved` with `article_id`, `error` with `error_msg`, or `skipped` for duplicates,
off-topic items, or paywalled content.

Each focused Session is self-contained. On failure, mark that entry as error and
continue the loop; do not let one entry block the rest. Count results after the
sequential loop finishes.

Entries with `error` status and fewer than 3 attempts are retried on the next poll
cycle.
