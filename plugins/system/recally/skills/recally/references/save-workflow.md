# Enriched Article Save Workflow

Load this reference only when the user asks to summarize, organize, evaluate, tag, or rate a single article. A bare “save this URL” request follows the **Capture one URL** workflow in `SKILL.md` instead.

## 1. Capture once

Save the URL first exactly as the **Capture one URL** workflow in `SKILL.md` describes: `skill_load web`, `bun $SKILL/scripts/web.ts fetch <url> --out $TMPDIR/recally/<slug>.md`, then `recally__article_save` with `url`, `source_type`, and `content_path` plus the metadata from the JSON line. The server never fetches a page itself. Check `content_chars` and `content_preview` before going on. A `thin extraction` error or a preview that reads as an excerpt means the page needs the original article URL; a 404 is terminal; a 401/403 means login or a paywall is required.

## 2. Generate Metadata

Read the saved article with `recally__article_get` (pass the returned `id`) only to understand it and generate metadata. Produce: **Title**, **Author**, **Tags** (3-7 lowercase), **Source Type**, **Worth-Reading tier**, and a **structured summary**.

**Worth-Reading tier**: pick exactly one value:

- `Top pick`: high novelty or insight density
- `Good read`: solid and informative
- `Skim`: low depth or mostly known

**Structured summary**: generate it in Wall Street Journal style (clear, professional, neutral). It is model-authored text and can be passed directly as `summary`; never copy the article body back into a tool argument.

The summary must contain exactly these sections:

```
# Summary
(2-3 sentences) Brief abstract capturing the essence of the article.

# Abstract
(150-200 words) Detailed yet concise summary covering key information, arguments, and narratives.

# Key Points
Bullet list of the most critical points or takeaways.

# Insights and Implications
Significant insights, implications, or conclusions. How the article relates to broader trends or current events.

# Actionable Takeaways
(if applicable) Practical advice or recommendations from the article.

# Critical Analysis
Potential biases, assumptions, strengths, or weaknesses. Any limitations or areas worth further exploration.
```

## 3. Save

Call `recally__article_save` again for the same URL with the generated metadata and no body. The article already exists, so this is a metadata-only update: the stored body is kept and the empty body is not rejected. Do not pass `canonical_url`.

Call it directly when it is listed. Otherwise use `tools.invoke("recally__article_save", ...)` inside `code`; the exact name and arguments are documented here, so do not search for or describe it first. Each item should include the generated title, author, structured summary, tags, source type, published time when available, and `worth_reading` metadata.

```js
return await tools.invoke("recally__article_save", {
  articles: [{
    url,
    title,
    summary,
    tags,
    source_type: "web"
  }]
});
```

The capture fields remain the archive baseline. For this enriched workflow, add:

- a model-authored structured summary
- 3-7 lowercase tags
- `worth_reading` metadata set to exactly `Top pick`, `Good read`, or `Skim`
- author and published time when the capture returned them

Do not invent a missing author or publication date. The source type is `web` unless the URL is known to be Twitter/X, YouTube, GitHub, RSS, or a PDF.

**Output**: the save action returns per-item results with `url`, `id`, and `status` (`created`, `updated`, or `error`). Do not echo raw IDs unless the user asks; summarize what was saved.

## 4. Optional Share

Only create a public link when the user asks. `share_create_article` is the exact tool name; pass the saved article id and the requested expiry. Do not search for or describe it.

When both tools are behind Code, save and share in one Code call. This is the reason to use Code: the intermediate article id stays between tools instead of returning to the model.

```js
const saved = tools.json(await tools.invoke("recally__article_save", {
  articles: [{
    url,
    title,
    summary,
    tags,
    source_type: "web"
  }]
}));

const article = Array.isArray(saved.results)
  ? saved.results.find(result => result.status !== "error")
  : undefined;
if (!article) return saved;

try {
  const shared = await tools.invoke("share_create_article", {
    article_id: article.id,
    expires_in: "7d"
  });
  return { saved, shared };
} catch (error) {
  return { saved, shareError: error.value || String(error) };
}
```

Both tools are cold, so `code` is the only way to reach them and the script above is the only way to chain them: a directly called tool returns its result to the model rather than into the next call.

To refresh an existing article's body: read it with the `web` skill (`bun $SKILL/scripts/web.ts fetch <url> --out FILE`), then call `recally__article_save` for the same URL with that file as `content_path`.
