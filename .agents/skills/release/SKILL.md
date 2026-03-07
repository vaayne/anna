# Release

Guide for creating releases of anna.

## Tag Format

Semantic versioning with `v` prefix: `v0.1.0`, `v1.0.0`, `v1.2.3-rc.1`

Pre-release tags (e.g., `-rc.1`, `-beta.1`) are auto-detected by GoReleaser.

## Release Flow

1. Update `CHANGELOG.md` (see below)
2. Commit: `📝 docs: Update CHANGELOG for vX.Y.Z`
3. Tag: `git tag vX.Y.Z`
4. Push: `git push origin main --tags`
5. CI runs `.github/workflows/release.yml` → GoReleaser binaries + Docker images

## Updating CHANGELOG.md

Before tagging a release:

```bash
# Commits since last tag (or all if first release)
git log $(git describe --tags --abbrev=0 2>/dev/null || git rev-list --max-parents=0 HEAD)..HEAD --oneline

# Merged PRs since last tag
gh pr list --state merged --base main --search "merged:>=$(git log -1 --format=%aI $(git describe --tags --abbrev=0 2>/dev/null || git rev-list --max-parents=0 HEAD))"
```

Then in `CHANGELOG.md`:

1. Rename `[Unreleased]` to `[X.Y.Z] - YYYY-MM-DD`
2. Add a fresh empty `[Unreleased]` section above it
3. Categorize with: `✨ Features`, `🐛 Bug Fixes`, `♻️ Refactoring`, `📝 Documentation`, `📦 Dependencies`
4. Link PRs: `([#123](https://github.com/vaayne/anna/pull/123))`
5. Add comparison link: `**Full Changelog**: [vPREV...vX.Y.Z](https://github.com/vaayne/anna/compare/vPREV...vX.Y.Z)`

## Release Tasks

```bash
mise run release:check     # Validate .goreleaser.yaml
mise run release:snapshot  # Test release locally (no tag needed)
mise run release           # Production release (requires tag)
```

## Artifacts

- **Binaries**: linux/darwin/windows × amd64/arm64 (GoReleaser)
- **Docker**: `ghcr.io/vaayne/anna` — linux/amd64 + linux/arm64
- **Docker tags**: `latest` (stable), `vX.Y.Z` (release), SHA (every build)
