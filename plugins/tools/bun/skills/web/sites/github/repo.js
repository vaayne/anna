/* @meta
{
  "name": "github/repo",
  "description": "GitHub repository stats and latest release via the public API",
  "domain": "github.com",
  "args": {
    "repo": {"required": true, "description": "owner/repo format (e.g. epiral/bb-browser)"}
  },
  "capabilities": ["network"],
  "readOnly": true,
  "example": "bb-browser site github/repo epiral/bb-browser"
}
*/

async function(args) {
  if (!args.repo) return {error: 'Missing argument: repo', hint: 'Use owner/repo format'};
  const parts = args.repo.split('/');
  if (parts.length !== 2) return {error: 'Invalid repo format', hint: 'Use owner/repo format (e.g. lightpanda-io/browser)'};

  // The REST API is stable where the HTML markup is not; anonymous calls get
  // 60 requests per hour per address, enough for a lookup.
  const headers = {Accept: 'application/vnd.github+json'};
  const api = `https://api.github.com/repos/${args.repo}`;
  const resp = await fetch(api, {headers});
  if (!resp.ok) return {error: 'HTTP ' + resp.status, hint: resp.status === 404 ? 'Repo not found: ' + args.repo : resp.status === 403 ? 'GitHub API rate limit; retry later' : 'GitHub error'};
  const repo = await resp.json();

  // /releases/latest can lag or point at a rolling tag; the newest published
  // non-draft, non-prerelease entry is what people mean by "latest release".
  let latest_release = null;
  const rel = await fetch(`${api}/releases?per_page=10`, {headers});
  if (rel.ok) {
    const releases = await rel.json();
    const stable = releases.filter(r => !r.draft && !r.prerelease && r.tag_name !== 'nightly');
    const pick = (stable.length ? stable : releases)[0];
    if (pick) latest_release = {tag: pick.tag_name, name: pick.name, published_at: pick.published_at, url: pick.html_url};
  }

  return {
    full_name: repo.full_name,
    description: repo.description,
    language: repo.language,
    url: repo.html_url,
    homepage: repo.homepage || null,
    stars: repo.stargazers_count,
    forks: repo.forks_count,
    open_issues: repo.open_issues_count,
    license: repo.license?.spdx_id || null,
    topics: repo.topics?.length ? repo.topics : null,
    default_branch: repo.default_branch,
    pushed_at: repo.pushed_at,
    latest_release
  };
}
