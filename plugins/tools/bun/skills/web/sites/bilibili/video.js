/* @meta
{
  "name": "bilibili/video",
  "description": "Get Bilibili video details by bvid",
  "domain": "www.bilibili.com",
  "args": {
    "bvid": {"required": true, "description": "Video BV ID (e.g. BV1xx411c7mD)"}
  },
  "capabilities": ["network"],
  "readOnly": true,
  "example": "bb-browser site bilibili/video BV1LGwHzrE4A"
}
*/

async function(args) {
  const bvid = args.bvid || args._positional?.[0];
  if (!bvid) return {error: 'Missing argument: bvid'};
  const resp = await fetch('https://api.bilibili.com/x/web-interface/view?bvid=' + encodeURIComponent(bvid), {credentials: 'include'});
  if (!resp.ok) return {error: 'HTTP ' + resp.status, hint: 'Not logged in?'};
  const d = await resp.json();
  if (d.code !== 0) return {error: d.message || 'API error ' + d.code, hint: d.code === -404 ? 'Video not found' : 'Not logged in?'};
  const v = d.data;
  const result = {
    bvid: v.bvid,
    aid: v.aid,
    title: v.title,
    description: v.desc,
    cover: v.pic,
    duration: v.duration,
    duration_text: Math.floor(v.duration / 60) + ':' + String(v.duration % 60).padStart(2, '0'),
    author: v.owner?.name,
    author_mid: v.owner?.mid,
    author_face: v.owner?.face,
    category: v.tname,
    tags: v.tag || null,
    pub_date: v.pubdate ? new Date(v.pubdate * 1000).toISOString() : null,
    stat: {
      view: v.stat?.view,
      like: v.stat?.like,
      dislike: v.stat?.dislike,
      coin: v.stat?.coin,
      favorite: v.stat?.favorite,
      share: v.stat?.share,
      reply: v.stat?.reply,
      danmaku: v.stat?.danmaku
    },
    pages: (v.pages || []).map(p => ({
      page: p.page,
      cid: p.cid,
      title: p.part,
      duration: p.duration
    })),
    url: 'https://www.bilibili.com/video/' + v.bvid
  };

  // Also fetch related videos
  try {
    const resp2 = await fetch('https://api.bilibili.com/x/web-interface/archive/related?bvid=' + encodeURIComponent(bvid), {credentials: 'include'});
    const d2 = await resp2.json();
    if (d2.code === 0 && d2.data) {
      result.related = d2.data.slice(0, 5).map(r => ({
        bvid: r.bvid,
        title: r.title,
        author: r.owner?.name,
        view: r.stat?.view,
        duration: r.duration,
        url: 'https://www.bilibili.com/video/' + r.bvid
      }));
    }
  } catch(e) {}

  return result;
}
