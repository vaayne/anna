package scheduler

// RecallyDigestTemplate is the job template for the daily recally reading digest.
// Users subscribe to this template instead of receiving it as a broadcast.
// Registered via (*Service).RegisterTemplate before Start.
var RecallyDigestTemplate = JobTemplate{
	Key:         "recally-digest",
	Name:        "recally-digest",
	Description: "Generate and deliver a daily reading digest from your Recally library.",
	Message: `Load the recally skill. Then generate and send a daily reading digest:
1. Call the native recally__digest_get tool to get today's digest data.
2. If the digest text contains neither a 'Saved yesterday' nor a 'Worth revisiting' section, stop — do NOT notify and do NOT save.
3. Check the user's language preference in memory. If no language preference is found, write the digest in English.
4. Write a newsletter-style narrative in the selected language. It should read like a personal curator wrote it — not a bullet list or a status report. Cover:
   - A short opening sentence that sets the tone for the day's reading (1–2 sentences).
   - For each article saved yesterday: weave the titles and summaries into 1–2 flowing sentences that explain why each piece is worth reading, grouping thematically where possible.
   - A brief "worth revisiting" section: mention the articles by title and why they might be relevant again now.
   - Close with the trending tags as a loose sentence: "Today's themes span X, Y, and Z."
   Keep the tone warm, curious, and concise — aim for 150–300 words total. No bullet points, no emoji, no section headers.
5. Call notify once with a short 1-sentence preview of the narrative (the opening sentence).
6. Save the full narrative by calling the native recally__digest_save tool with narrative set to the full narrative.`,
	DefaultSchedule: Schedule{Every: "24h"},
	SessionMode:     SessionNew,
}
