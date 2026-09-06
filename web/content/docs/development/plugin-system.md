---
title: Plugin System
---

Plugins share configuration and permission rules. Their backends retain the
contracts needed to execute a CLI, call an MCP server, or run compiled Go code.

## Definition and configuration

`PluginDefinition` identifies an integration and its namespace. It describes the
backend, shipped resources and default enabled state. Builtin definitions come
from trusted release declarations; persisted builtin rows are projections.
Custom definitions cannot select arbitrary Go implementations.

`PluginConfig` records one decision for a definition at one scope:

| Scope        | Applies to                    |
| ------------ | ----------------------------- |
| System       | Everyone in the deployment    |
| System agent | One Agent, across its users   |
| User         | One user, across their Agents |
| User agent   | One user using one Agent      |

There is at most one configuration for each definition and scope tuple. The
selected configuration is user agent, user, system agent, then system. An
explicit system or matching system agent `false` is an independent upper bound.
A narrower `true` cannot override either restriction. `null` follows the shipped
default of the selected definition.

This is the complete configuration model: one `PluginDefinition` plus at most
one `PluginConfig` for each of the four scope tuples. `user_id` and `agent_id`
come from the trusted authority and are never accepted as caller-owned identity.
The definition owns stable implementation and namespace identity; the selected
config owns its backend payload and credentials for that scope.

The selected scope owns its configuration. It can override fields in the
shipped definition, but fields and credentials are never merged across scopes.
A disabled or incomplete winner does not fall back to a broader configuration.
Builtin plugins follow the same rules and administrators can disable them.

## One execution snapshot

The common service resolves a snapshot from trusted user, Agent or group
identity. A runner captures that snapshot once. Tools, Skills, prompt sections,
environment and lifecycle hooks consume that same generation.

Public resources follow the namespace winner. A custom MCP integration that
wins a namespace cannot acquire the shadowed builtin's Go hooks or credentials.
Internal capability checks use the exact plugin ID, so a namespace collision
cannot hide an administrator's restriction on a channel or background task.
Model-facing plugin tools use `{namespace}__{local_name}`. Authorization uses
trusted plugin ID and local tool identity, never a parsed display name.

Configuration writes run under the admission boundary and commit atomically.
Idle runners are retired; an admitted turn can finish before its runner is
retired. Credential access must still match the captured configuration revision:
an old runner must never send newly rotated credentials to an old endpoint.
Plugin switches govern Stella-managed exposure and admission. Filesystem and
network restrictions remain sandbox responsibilities; disabling a plugin does
not revoke an existing OAuth grant or erase a previously loaded Skill.

## CLI and Skill resources

A CLI integration can contain binaries, Skills, environment declarations and
prompt guidance. A CLI version pin and a Skill source are independent fields;
changing one need not change the other. The manifest is a release input loader,
not a separate permission system. CLI installation is lazy: a runner materializes
the selected snapshot when it needs it, and there is no standalone sync endpoint.

Builtin plugin Skill ownership comes from its trusted resource path,
`plugins/<kind>/<plugin>/<skill>`. Core Skills live under `core/<skill>` and are
not plugin contributions. User frontmatter cannot turn a core Skill into a
plugin Skill or claim a plugin owner.
Frontmatter cannot claim ownership. Prompt listing, search and direct loading
apply the same owner restriction after selecting the resource winner.
A core Skill can still have a declared CLI dependency: Web requires Bun and
Python Script requires uv. Disabled dependencies suppress that Skill through the
same listing and loading checks. Lightpanda affects rendering within Web, not
plain fetch or search.

Each runner receives only the selected CLI artifacts and their entry points.
Trusted system installations keep private options out of the runner filesystem;
Docker prepares Linux artifacts in its existing tool cache, keyed by one resolved
image ID and the complete four-scope selection. A selection helper supplies only
the chosen entries and binaries. Native managed installs use the managed tree;
user and user agent installations stay in their own sandbox trees and take
precedence in PATH. Plugin permissions govern the resources Stella supplies; the
`none` backend provides no filesystem isolation.

## Channels and accounts

A channel plugin describes one platform. Each account remains a separate
channel instance, with its own exact ID, credentials, active state and persistent
Agent binding. Saving one account cannot replace another account's credentials
or re-enable an administrator-disabled platform.

Listeners use the system and matching system agent upper bounds plus instance
active state. A user's restriction does not stop a listener shared with other
users. Event admission also checks the trusted actor's four scopes and existing
Agent access or guest policy. Channel signatures, enrollment and platform identity
checks remain in their owning adapters.

The existing uniqueness rule, `UNIQUE(agent_id, type)`, permits at most one
instance of a platform per Agent. Each instance has its own credentials, so
multiple accounts remain separate even when they use the same platform. Multiple
accounts can be bound to different Agents. Identity linking is separate from
provisioning a bot account.

## MCP credentials and observations

MCP is a plugin backend. Authored endpoint settings and credential references
belong to its selected configuration. Tokens remain in the Vault. Shared and
per-user credentials never fall back to each other.

OAuth client registration belongs to the configuration owner. For system and
system agent configurations, an administrator initializes a missing client
through the OAuth start action before users authorize their own accounts. User
and user agent configuration owners can initialize their own client. Existing
system configurations without a client ID need this administrator step after
upgrade; per-user tokens remain isolated. Disable and reset preserve grants.
Deleting a configuration removes its grants atomically.

Remote tool catalogs and connection status are backend observations keyed by
configuration and credential owner, with a configuration revision fence. A
per-user catalog cannot become another user's tool list. Legacy per-user catalogs
have no trusted owner provenance and are cold-probed after migration. Internal
OAuth bundles are excluded from public Vault access and ambient environment
injection.

## Core boundaries and upgrade

Provider adapters and sandbox backends retain their explicit compiled
registries. Core storage, orchestration and credential services do not become
optional simply because a plugin uses them. For example, disabling the public
Xberg plugin hides its public resources; the Library's explicit internal parser
dependency remains available.

`cmd/stellad` composes backend adapters and the common catalog. Backend packages
must not introduce their own scope or enabled resolver. Production provider and
sandbox adapters depend on public `pkg/**` contracts, not `internal/**`.

The legacy cutover is a maintenance upgrade: stop every old writer before
starting the new runtime. One transaction imports and validates configurations,
credential relationships and tool policies before recording completion. Legacy
rows remain for inspection, but runtime code does not read both old and new
configuration stores. Rolling old and new writers against that database is not
supported during this cutover.
