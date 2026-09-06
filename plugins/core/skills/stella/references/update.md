# Updating stella

## Check current version

```bash
stellad version
```

## Self-update (recommended)

```bash
stellad upgrade
stellad upgrade 0.50.0                             # install a specific release
stellad upgrade 0.66.0-rc.1                         # install a specific RC
stellad upgrade --channel stable                    # leave an RC and use latest stable
stellad upgrade --install-dir "$HOME/.local/bin"  # custom install path
```

Downloads a stable release from GitHub for your platform (the latest by default, or the version you pass) and replaces the running `stellad` binary by default. An RC build requires an explicit version or `--channel stable`, so a plain upgrade cannot silently downgrade it to an older stable release. Progress is shown while the archive downloads. If the target directory is not writable, rerun with the required OS permission or use `--install-dir`. If the binary is locked or busy, stop the running Stella process or service first, then retry.

## Other methods

### From source

```bash
cd ~/path/to/stella
git pull origin main
mise run setup
mise run build
# Move dist/bin/stellad to your PATH
```

`go install` is not a supported method. The binary embeds generated API code,
the built Web UI, and the bundled runtimes, none of which are tracked in the
repository, so the module does not compile on its own.

### GitHub releases

Download the latest binary from https://github.com/CherryHQ/stella/releases

Binaries available for: linux/darwin/windows x amd64/arm64.

### Docker

```bash
docker pull ghcr.io/cherryhq/stella:latest
```

Tags: `latest` (stable), `vX.Y.Z` (specific stable release), `vX.Y.Z-rc.N` (release candidate, pin explicitly).

## After updating

- Back up PostgreSQL and all durable workspace bytes before upgrading; database migrations run automatically when the new release starts
- Review release notes and resolve any startup-reported blockers before serving traffic
- Refresh the model cache from the Web UI if new models are available
- Builtin skills update with the binary through its immutable release bundle

## Skill upgrade and downgrade checks

Before upgrading, inspect legacy `$STELLA_HOME/.agents/skills`. Using the old working binary, import each custom Skill root as a managed global (`system`) Skill through **Settings → Skills** on older releases or **Admin Console → Deployment resources → Global Skills** on newer releases. Back up, verify, and remove other residual paths. The new binary lists every blocking path and stops without deleting or changing anything. Paths owned by the current release manifest are inert even when their contents or modes are stale; every other Skill root or residual path blocks startup.

Before downgrading to a binary that predates AgentSkillPolicy v1, re-enable every disabled Skill and explicitly clear dangling disablements in the Web UI. Older binaries ignore canonical policy, and ordinary Agent edits can overwrite the reused column. Retained bundle directories are derived and inert after rollback.

Explicit destructive user, group, and Agent deletion fence execution before removing the database owner. Workspace bytes and inodes remain, but subsequent access fails owner validation. For live owners, the sole `WorkspaceManager` creates missing deterministic roots and rejects non-directories, symlinks, unsafe IDs, and trusted-root replacement. Any filesystem entry at `agents/{id}` reserves that Agent ID. Run restore and root cleanup while Stella is stopped. Routine upgrades and Helm uninstall do not delete workspace bytes. This is a trusted-host, single-replica POSIX contract; multi-replica, Kubernetes, and S3 authority require a future redesign.
