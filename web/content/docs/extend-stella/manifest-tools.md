---
title: Manifest Tool Plugins
description: Declarative CLI tool integrations, shipped with the server and customized from the admin UI.
---

## Overview

Manifest tool plugins are a lightweight alternative to full Go-compiled plugins for simple CLI tool integrations. Instead of writing a Go package, the tool is declared as data. Stella normalizes that declaration into the same plugin definition and four-scope configuration model used by every backend.

Stella ships with a built-in manifest that declares the default manifest-managed CLI integrations (`gh`, `lark-cli`, `lightpanda`). They appear in their semantic tabs, such as **Tools** or **Hooks**, with a `manifest` badge. You override or extend them from the Plugins admin UI; your changes are stored in the database, and the manifest compiled into the server is never modified.

## Plugin source directories

Source plugins can be grouped under any number of category folders in `plugins/`. A manifest-only CLI plugin has one `plugin.yaml` containing its existing plugin declaration, without an enclosing `plugins:` list. Build generation recursively discovers these files and produces the embedded catalog. Runtime startup does not scan the source tree.

The declared ID and namespace identify the plugin. Category folder names do not affect tool names, configuration keys or permissions, so moving a plugin between categories preserves its identity. Empty category folders do not create plugins. Duplicate IDs or namespaces, unknown YAML fields, extra YAML documents and symlink declarations fail generation before the generated catalog is replaced.

Go plugins retain explicit compiled registration and do not add a second identity declaration in YAML. `plugins/core/` owns required runtimes and source skills; it is not a configurable plugin. Putting a directory under `core` does not grant it that status. Shared OAuth provider declarations remain in `resources/oauth.yaml`.

Bundled skill files live only inside their owning plugin directory and are embedded directly through build-time asset declarations. There is no global resource skill mirror, and category folders do not determine asset identity. `web` belongs to bun and `python-script` to uv. `html-artifact` and `skill-creator` are default-enabled skill-only plugins that can be disabled normally. Project `.agents/skills/` stays independent of plugin packaging.

## Required builtins

mise, Xberg, fd and rg form Stella's required execution environment. They are available without plugin configuration and have no enable switch. Xberg's skill is a core skill. The existing platform support limits still apply, including the absence of Xberg on Windows.

These runtimes follow the Stella release. Docker images install them during the image build; native startup prepares them before accepting conversations and reuses complete local artifacts without running the installer again. Optional CLI plugins continue to use the four-scope configuration model below. Their binary names cannot replace a required runtime.

The Docker build publishes the exact prepared runtime tree at a stable image path, including executable sidecars. See `stellad system-bundle install --help` for build-time publication options.

Upgrading retires the former `tool/mise`, `tool/xberg`, `tool/fd` and `tool/rg` plugin settings, including disabled values. The required builtins remain available after this migration; other plugin settings are preserved.

## How It Works

At startup, Stella:

1. Loads the generated builtin CLI catalog and shared OAuth declarations
2. Loads your stored customizations from the database and lays them over the built-in definitions, adding any plugin you created that has no built-in behind it
3. Normalizes the definitions and scope configurations into the common plugin catalog
4. Registers enabled manifest plugins into the plugin host

A runner exposes optional plugin binaries from its captured plugin snapshot. Docker copies matching shipped artifacts from the image; a custom declaration without a matching artifact requires isolated installation. Required builtins are prepared independently of that snapshot. Native managed sessions use the managed tree; user and user-agent selections use their own sandbox trees. Docker uses Linux-native preparation inside its sandbox boundary.

## Docker sandbox CLI availability

Do not treat host `$STELLA_HOME/bin` as the source of Docker sandbox executables. On macOS and Windows, native managed installation produces host-platform binaries, which cannot run in a Linux container. Binding that directory into Docker also blurs the boundary between host-side tool management and the container runtime.

For Docker:

- Required builtins are pre-installed in the versioned sandbox image from the shared release declarations. They remain available when no optional plugin is selected.
- The resolved manifest — built-in definitions plus your stored customizations — remains the source of plugin metadata, enablement, session environment, OAuth injection, and local-sandbox binary installation.
- Shipped optional CLI artifacts are prepared during image build. They remain hidden until selected by the four-scope authorization policy; preinstallation does not enable a plugin.
- Custom CLI declarations without an exact image artifact are installed for Linux in the isolated helper, never copied from the host.

Docker prepares each selection as follows:

1. Resolve the complete four-scope plugin selection for the runner's trusted user and Agent.
2. Match each selected binary against the image cache by name, mise tool key, declared version (empty means `latest`), and all installation options. Copy matching complete artifacts and sidecars; install only misses in the isolated helper.
3. Store the resulting tools in the Docker-managed tool cache or volume keyed by one resolved image ID plus the complete selection identity.
4. Mount the selected entries into the sandbox session at a container-only path and prepend them to the in-container `PATH`.
5. Prepare a new selection when the captured snapshot or image ID changes. Matching image artifacts are copied without downloading them again. The default shipped selection can start without network access; custom versions may still need downloads.

The image artifact cache is masked from running sessions. A denied plugin has neither an executable alias nor an accessible cached install or sidecar directory. Version and option mismatches never fall back to a similarly named image tool.

This keeps the release sandbox image stable while still allowing user-added CLIs. The installed user binaries are Linux container binaries, and the host `$STELLA_HOME/bin` is not part of Docker executable resolution. The `none` backend remains trusted-host execution and provides no filesystem isolation.

## The plugin definition

A manifest plugin is the same set of fields whether it ships in a source `plugin.yaml` or you fill it in from the admin UI. The YAML form below is the clearest way to read that shape; the admin UI edits the same fields as form rows.

The manifest supplies a `PluginDefinition`; enablement and configuration are
stored as `PluginConfig` records in the four scopes `system`, `system_agent`,
`user`, and `user_agent`. The selected scope owns the complete backend decision.
An explicit `false` at system or matching system-agent scope is an upper bound,
and a disabled winner never falls back to a broader scope. Builtin definitions
and their resources are release-owned, and optional builtin plugins can be disabled. Required core runtimes do not enter this configuration model.
CLI versions can be adjusted independently. Bundled skills are release-owned; configurations in all four scopes cannot replace or reset their membership. A `skills` entry accepts only a local `{name}`, never a repository, remote source, or filesystem path.

```yaml
plugins:
  - id: tool/my-cli
    kind: tool
    name: my-cli
    display_name: My CLI
    description: Does something useful.
    enabled: true
    binaries:
      - name: my-cli
        tool: github:owner/my-cli
        version: "1.2.3" # omit for latest
    session_env:
      - env_var: MY_TOKEN
        source: static
        value: "abc123"
        required: true
```

## Plugin fields

| Field            | Required | Description                                                                                                       |
| ---------------- | -------- | ----------------------------------------------------------------------------------------------------------------- |
| `id`             | Yes      | Unique plugin ID in `kind/name` form, e.g. `tool/my-cli`                                                          |
| `kind`           | Yes      | Plugin kind, typically `tool`                                                                                     |
| `name`           | Yes      | Short machine-readable name                                                                                       |
| `display_name`   | No       | Human-readable label shown in the admin UI                                                                        |
| `description`    | No       | Short description shown in the admin UI                                                                           |
| `enabled`        | No       | Manifest input shorthand normalized into the selected `PluginConfig`; it is not a second permission system.       |
| `binaries`       | No       | CLI binary specifications; native managed installs use the managed tree, while user scopes use their sandbox tree |
| `session_env`    | No       | Environment variables to inject into sandbox sessions                                                             |
| `oauth_provider` | No       | Static OAuth provider ID used by `oauth.*` session env sources, such as `github`                                  |

## Binary fields

Each binary requires a `name` and a `tool` field. The `tool` field uses mise's tool key format: `backend:identifier`.

### Common fields

| Field              | Required | Description                                                                                   |
| ------------------ | -------- | --------------------------------------------------------------------------------------------- |
| `name`             | Yes      | Binary filename exposed in the selected runtime tree (without extension)                      |
| `tool`             | Yes      | Mise tool key in `backend:identifier` format (e.g. `github:cli/cli`)                          |
| `version`          | No       | Version to install. Defaults to `latest` for all backends.                                    |
| `strip_components` | No       | Leading directory levels to strip when extracting an archive. Auto-detected for most layouts. |
| `bin_path`         | No       | Subdirectory inside the archive containing the binary (e.g. `"bin"`).                         |
| `bin`              | No       | Rename the downloaded file when the asset is a single binary (non-archive).                   |
| `rename_exe`       | No       | Rename the executable after extraction from an archive.                                       |
| `checksum`         | No       | Verify the asset with a checksum in `algo:hex` format (e.g. `"sha256:abc123..."`).            |

### GitHub backend (`github:owner/repo`)

```yaml
binaries:
  - name: gh
    tool: github:cli/cli
    version: "2.40.1"
    bin_path: bin
```

| Field            | Description                                                                                |
| ---------------- | ------------------------------------------------------------------------------------------ |
| `asset_pattern`  | Glob pattern to select the release asset (e.g. `"gh_*_linux_x64.tar.gz"`).                 |
| `version_prefix` | Custom tag prefix (e.g. `"release-"`).                                                     |
| `no_app`         | Skip macOS `.app` bundles; prefer standalone binaries.                                     |
| `filter_bins`    | Comma-separated list of binaries to expose when the archive contains multiple executables. |
| `prerelease`     | Include pre-release versions when resolving `latest`.                                      |
| `api_url`        | GitHub API base URL for GitHub Enterprise (e.g. `"https://github.example.com/api/v3"`).    |

### HTTP backend (`http:name`)

The identifier after `http:` is the tool name used internally by mise.

```yaml
binaries:
  - name: sentinel
    tool: http:sentinel
    url: "https://releases.hashicorp.com/sentinel/{{version}}/sentinel_{{version}}_{{os()}}_{{arch()}}.zip"
    version: "0.26.3"
```

| Field               | Description                                                                                          |
| ------------------- | ---------------------------------------------------------------------------------------------------- |
| `url`               | Download URL. Required for http backend. Supports `{{version}}`, `{{os()}}`, `{{arch()}}` templates. |
| `size`              | Expected file size in bytes for verification.                                                        |
| `format`            | Archive format override (e.g. `"tar.xz"`).                                                           |
| `version_list_url`  | URL to fetch available versions from.                                                                |
| `version_regex`     | Regex to extract versions from the version list.                                                     |
| `version_json_path` | jq-style path to extract versions from JSON (e.g. `".[].tag_name"`).                                 |
| `version_expr`      | expr-lang expression to extract versions.                                                            |

### Pipx backend (`pipx:package`)

The identifier is the PyPI package name, `org/repo` for a GitHub source, or a `git+https://...` URL.

```yaml
binaries:
  - name: mypy
    tool: pipx:mypy
    version: "1.8.0"
```

| Field       | Description                                   |
| ----------- | --------------------------------------------- |
| `extras`    | Pip extras to install alongside the package.  |
| `pipx_args` | Extra arguments to pass to pipx.              |
| `uvx`       | Use `uvx` (uv's tool runner) instead of pipx. |
| `uvx_args`  | Extra arguments for uvx.                      |

### NPM backend (`npm:package`)

```yaml
binaries:
  - name: serve
    tool: npm:serve
    version: "14.2.0"
```

Platform-specific asset patterns (`platforms:` map) are not supported in the manifest.

## Session env fields

| Field      | Required    | Description                                                       |
| ---------- | ----------- | ----------------------------------------------------------------- |
| `env_var`  | Yes         | Environment variable name                                         |
| `source`   | Yes         | How the value is resolved (see below)                             |
| `value`    | Conditional | Value when `source: static`                                       |
| `required` | No          | If true, session creation fails when the value cannot be resolved |

### Env sources

| Source               | Description                                           |
| -------------------- | ----------------------------------------------------- |
| `static`             | Uses the literal `value` from the manifest            |
| `oauth.access_token` | Injects the connected provider's OAuth access token   |
| `oauth.client_id`    | Injects the connected provider bundle's client/app ID |

`oauth.*` sources resolve through the plugin's `oauth_provider`. GitHub uses Stella's built-in GitHub CLI device-flow app and needs no admin-side plugin configuration. Other providers must be declared and configured separately.

## State and caching

Prepared selections are cached by their captured plugin configuration. Docker also pins the resolved image ID. Changing a binary declaration creates a new selection; matching image artifacts are reused, while missing versions require installation. Preparation is cancelled with the session and Stella terminates installer child processes.

## Admin UI

Manifest-backed plugins are shown once, in the tab that matches their kind:

- `tool/gh`, `tool/lark-cli`, and `tool/lightpanda` appear in **Tools**.

Rows with manifest backing show a `manifest` badge and an **Edit definition** action for the plugin definition. Binaries and session environment variables are edited as form rows. If the same plugin also exposes runtime config, the row also shows **Configure**. The enable switch is stored separately from the definition, so disabling a built-in does not count as customizing it, and pinning a binary to a specific version is an ordinary definition edit.

The **Tools** tab includes **Add Tool** for creating a new manifest-backed CLI from a GitHub release binary. Saving registers the plugin; the next eligible runner lazily materializes its selected binaries. The embedded built-in manifest is never modified.

Editing a built-in stores only the fields you changed, so the rest keep following the definition shipped with the server and still improve when you upgrade. Such a plugin is marked **customized** and offers **Reset to default**, which drops the stored edits and leaves the enable switch as it is. Editable lists, binaries and session environment variables, are stored whole. Skills remain read-only release resources and are not an override list.

## Limitations in v1

- Skills declared in a shipped manifest must refer to assets bundled with that plugin. Custom CLI definitions cannot register skills or download skill sources.
- The manifest has no standalone runtime or synchronization endpoint. Binary installation is driven by the resolved plugin snapshot.
- Custom install scripts are not supported.
- Platform-specific asset patterns (`platforms:` map) are not supported. Use `asset_pattern` instead.
- Supported binary sources: GitHub releases (`github`), direct HTTP download (`http`), pipx (`pipx`), npm (`npm`).
