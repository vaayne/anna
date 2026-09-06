/* @meta
{
  "description": "List X / Twitter user statuses via FxEmbed",
  "domain": "api.fxtwitter.com",
  "args": {
    "handle": {
      "required": true,
      "description": "Username without @, or numeric user id as id:123"
    },
    "count": {
      "required": false,
      "description": "Page size, 1-100"
    },
    "cursor": {
      "required": false,
      "description": "Pagination cursor"
    },
    "since": {
      "required": false,
      "description": "Unix timestamp; returns posts newer than this when no cursor is provided"
    },
    "with_replies": {
      "required": false,
      "description": "Truthy value to include replies"
    },
    "groupthreads": {
      "required": false,
      "description": "Truthy value to group conversation rows"
    },
    "lang": {
      "required": false,
      "description": "Target language for inline translations, e.g. en, es, zh-cn"
    }
  },
  "readOnly": true
}
*/

async function(args) {
  if (!args.handle) return {error: 'Missing argument: handle', hint: 'Provide username without @, or numeric user id as id:123'};

  const handle = String(args.handle).replace(/^@/, '');
  const params = [];
  if (args.count) params.push('count=' + encodeURIComponent(args.count));
  if (args.cursor) params.push('cursor=' + encodeURIComponent(args.cursor));
  if (args.since) params.push('since=' + encodeURIComponent(args.since));
  if (args.with_replies) params.push('with_replies=' + encodeURIComponent(args.with_replies));
  if (args.groupthreads) params.push('groupthreads=' + encodeURIComponent(args.groupthreads));
  if (args.lang) params.push('lang=' + encodeURIComponent(args.lang));

  let url = `https://api.fxtwitter.com/2/profile/${encodeURIComponent(handle)}/statuses`;
  if (params.length) url += '?' + params.join('&');

  const resp = await fetch(url, {
    headers: {'accept': 'application/json'}
  });

  if (!resp.ok) return {error: 'HTTP ' + resp.status};

  return await resp.json();
}
