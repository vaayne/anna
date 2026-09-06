/* @meta
{
  "description": "Fetch an X / Twitter post by ID via FxEmbed",
  "domain": "api.fxtwitter.com",
  "args": {
    "id": {
      "required": true,
      "description": "Tweet / post ID"
    },
    "about_account": {
      "required": false,
      "description": "Truthy value to include account metadata when available"
    },
    "lang": {
      "required": false,
      "description": "Target language for inline translations, e.g. en, es, zh-cn"
    },
    "format": {
      "required": false,
      "description": "Return format: json or markdown"
    }
  },
  "readOnly": true
}
*/

async function(args) {
  const applyInlineStyles = (text, ranges) => {
    const boldRanges = ranges.filter(r => r.style === 'Bold').sort((a, b) => b.offset - a.offset);
    for (const range of boldRanges) {
      const start = range.offset;
      const end = range.offset + range.length;
      text = text.slice(0, start) + '**' + text.slice(start, end) + '**' + text.slice(end);
    }
    return text;
  };

  const applyURLs = (text, urls) => {
    const ranges = urls.slice().sort((a, b) => b.fromIndex - a.fromIndex);
    for (const url of ranges) {
      const label = text.slice(url.fromIndex, url.toIndex);
      text = text.slice(0, url.fromIndex) + '[' + label + '](' + url.text + ')' + text.slice(url.toIndex);
    }
    return text;
  };

  const articleMarkdown = (body) => {
    const status = body.status || body;
    const article = status.article;
    if (!article) return {error: 'No article found for status ' + (status.id || '')};

    const lines = [];
    if (article.title) lines.push('# ' + article.title.trim(), '');
    lines.push('> ' + status.url, '');

    const mediaById = {};
    for (const media of article.media_entities || []) {
      mediaById[media.media_id] = media.media_info && media.media_info.original_img_url;
    }

    const entityMap = {};
    for (const entity of article.content.entityMap || []) {
      entityMap[String(entity.key)] = entity.value;
    }

    for (const block of article.content.blocks || []) {
      if (block.type === 'atomic') {
        const entityRef = block.entityRanges && block.entityRanges[0];
        const entity = entityRef && entityMap[String(entityRef.key)];
        const items = entity && entity.data && entity.data.mediaItems || [];
        for (const item of items) {
          const imageURL = mediaById[item.mediaId];
          if (imageURL) lines.push('![](' + imageURL + ')', '');
        }
        continue;
      }

      let text = block.text || '';
      if (!text.trim()) {
        lines.push('');
        continue;
      }

      text = applyInlineStyles(text, block.inlineStyleRanges || []);
      text = applyURLs(text, block.data && block.data.urls || []);
      lines.push(text, '');
    }

    return lines.join('\n').replace(/\n{3,}/g, '\n\n').trim();
  };

  if (!args.id) return {error: 'Missing argument: id', hint: 'Provide a tweet / post ID'};

  const params = [];
  if (args.about_account) params.push('about_account=' + encodeURIComponent(args.about_account));
  if (args.lang) params.push('lang=' + encodeURIComponent(args.lang));

  let url = `https://api.fxtwitter.com/2/status/${encodeURIComponent(args.id)}`;
  if (params.length) url += '?' + params.join('&');

  const resp = await fetch(url, {
    headers: {'accept': 'application/json'}
  });

  if (!resp.ok) return {error: 'HTTP ' + resp.status};

  const body = await resp.json();
  if (args.format === 'markdown') return articleMarkdown(body);

  return body;
}
