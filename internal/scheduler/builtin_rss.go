package scheduler

// RecallyRSSTemplate is the job template for recurring recally RSS polling.
// Users subscribe to this template instead of receiving it as a broadcast.
// Registered via (*Service).RegisterTemplate before Start.
var RecallyRSSTemplate = JobTemplate{
	Key:         "recally-rss",
	Name:        "recally-rss",
	Description: "Poll your Recally feeds every 6 hours and save new articles.",
	Message: `1. Discover new entries for every enabled feed. Load the recally skill, then dispatch by feed kind:
   - rss: call the native recally__feed_poll tool with limit=20 once. Omit id to poll every enabled RSS feed; non-rss feeds are skipped server-side.
   - twitter / website (and other non-rss kinds): call the native recally__feed_list tool, and for each feed whose kind matches that workflow, follow the recally skill (e.g. the Twitter or website workflow) to list items and push them via recally__entry_add (dedup on guid).
2. For each pending entry (across all feeds), follow the save workflow defined in the recally skill (fetch → generate metadata → save with the entry's source type → mark).
3. Notify only when at least one article was saved: count articles saved in step 2. If zero, do NOT call the notify tool — stop here. If one or more, call notify once:
   - For each article (up to 5): Worth-Reading label (emoji + text), title, author, and the "# Summary" section from the structured summary.
   - If more than 5 were saved, list the remaining as title + author only.`,
	DefaultSchedule: Schedule{Every: "6h"},
	SessionMode:     SessionNew,
}
