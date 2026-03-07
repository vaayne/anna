---
name: release
description: >
  Release workflow for anna Go CLI project. Create releases with semantic versioned
  tags, update CHANGELOG.md, and trigger automated CI/CD builds. Use when the user
  asks to "release", "create a release", "tag a version", "update changelog",
  "prepare release", "cut a release", or discusses versioning and release artifacts.
---

# Release

## Tag Format

Use semantic versioning with `v` prefix: `v0.1.0`, `v1.0.0`, `v1.2.3-rc.1`.
GoReleaser auto-detects pre-release suffixes (`-rc.1`, `-beta.1`).

## Release Flow

1. Update `CHANGELOG.md` (see below)
2. Commit: `📝 docs: Update CHANGELOG for vX.Y.Z`
3. Tag: `git tag vX.Y.Z`
4. Push: `git push origin main --tags`
5. CI triggers `.github/workflows/release.yml` → GoReleaser binaries + Docker images

## Update CHANGELOG.md

Gather changes since last tag:

```bash
git log $(git describe --tags --abbrev=0 2>/dev/null || git rev-list --max-parents=0 HEAD)..HEAD --oneline
gh pr list --state merged --base main --search "merged:>=$(git log -1 --format=%aI $(git describe --tags --abbrev=0 2>/dev/null || git rev-list --max-parents=0 HEAD))"
```

Apply to `CHANGELOG.md`:

1. Rename `[Unreleased]` to `[X.Y.Z] - YYYY-MM-DD`
2. Add fresh `[Unreleased]` section above
3. Categorize: `✨ Features`, `🐛 Bug Fixes`, `♻️ Refactoring`, `📝 Documentation`, `📦 Dependencies`
4. Link PRs: `([#123](https://github.com/vaayne/anna/pull/123))`
5. Append: `**Full Changelog**: [vPREV...vX.Y.Z](https://github.com/vaayne/anna/compare/vPREV...vX.Y.Z)`

## Validate and Test

```bash
mise run release:check     # Validate .goreleaser.yaml
mise run release:snapshot  # Test release locally (no tag needed)
```

## Artifacts

- **Binaries**: linux/darwin/windows × amd64/arm64 (GoReleaser)
- **Docker**: `ghcr.io/vaayne/anna` — linux/amd64 + linux/arm64
- **Docker tags**: `latest` (stable), `vX.Y.Z` (release), SHA (every build)
