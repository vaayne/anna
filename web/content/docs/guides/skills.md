---
title: Skills
---

## What Are Skills

Skills are reusable playbooks that teach Stella how to perform specific tasks. When you ask Stella to do something like "create a GitHub release" or "write a blog post," she can load a skill that gives her step-by-step instructions for that workflow.

Skills are written in plain markdown — they are essentially cheat sheets that Stella reads and follows. You can install skills from public registries, or write your own.

## Skill scopes and priority

Each kind of Skill has one content authority. Release-provided builtins come from the immutable, content-addressed release bundle. Project Skills are ordinary files in durable Agent/project working trees. Managed global, Agent-bound, user, and user-Agent Skills use immutable revisions in typed Stella Home roots; a current selector chooses the exact revision Stella loads.

The stored scopes are `project`, `user_agent`, `user`, `system_agent`, and `system`. `builtin` is contextual: a release Skill has the immutable identity `builtin:<name>`. An administrator-installed global Skill is the separate mutable identity `system:<name>`, and an Agent-bound administrator Skill is `system_agent:<name>`.

- **Project skills** — live in your repository under `.agents/skills/`. They ship with the code and are available when the current session is attached to that project.
- **User skills** — your personal skills, available across all of your agents.
- **User · this agent** — your personal skills scoped to a single agent.
- **Shared agent skills** — managed by admins and available to everyone who uses that agent.
- **Global skills** — managed by admins and available everywhere. Skills bundled with Stella remain part of the installation; managed global skills can be installed, enabled, disabled, and removed from the Admin Console.

When names collide, Stella selects one winner in this order:

```
project > user · this agent > user > shared agent > global > builtin
```

It applies policy after selecting that winner. Disabling a winner does not reveal a lower-priority Skill with the same name.

## Per-Agent activation

Skills supplied by a plugin follow that plugin's permissions and enablement in plugin settings; they have no separate skill switch. A package containing only skills uses the same plugin settings. Project `.agents/skills/`, personal skills, and managed skills remain independent. Required Stella and Xberg guidance ships with the execution environment.

Skills are enabled for an Agent by default. An administrator or durable Agent creator can use that Agent's **Skills** tab to enable or disable a managed global or matching Agent Skill. This is one shared setting: the last committed update wins.

Activation is separate from permission to edit Skill content and from `disable_model_invocation`. A turn already admitted keeps its Skill snapshot; the next turn sees a committed activation change.

Disabled references to Skills that no longer exist do not affect execution; clear them explicitly in the Web UI. Skill activation is a product preference, not a filesystem access control.

Manage personal `user` and `user_agent` skills from **Personal Settings → Skills**. Administrators manage deployment-owned `system` and `system_agent` skills from **Admin Console → Deployment resources → Global Skills**. The two pages never mix ownership scopes.

## Installing Skills

### Choose a destination

Every install and upload requires a destination. Stella never infers it from a conversation or remembers it as authorization for a later write.

- In an Agent's **Skills** tab, choose **Only me · this Agent** (`user_agent`) or, for administrators, **Everyone · this Agent** (`system_agent`).
- In **Personal Settings → Skills**, choose a personal destination (`user` or `user_agent`).
- In **Admin Console → Deployment resources → Global Skills**, choose a deployment-owned destination (`system` or `system_agent`).

The Web UI asks you to confirm the destination immediately before it writes.

### Choose a source

Stella can install skills from several sources:

- **[clawhub.ai](https://clawhub.ai)** — browse or search the marketplace in the Web UI.
- **GitHub / GitLab** — enter a repository source in the install form.
- **ZIP upload** — upload a Skill directory containing `SKILL.md`.

If you hit rate limits on clawhub.ai, you can set a free API token:

1. Sign up at [clawhub.ai](https://clawhub.ai).
2. Go to Settings, then API Tokens, and create a token.
3. In chat, send: `/config CLAWHUB_TOKEN your-token`

## Managing Skills

### From a Conversation

- **"Find an installed skill for deploying this service."** — Stella searches only Skills already visible to the active Agent.
- **"Load the deployment skill."** — Stella loads the selected exact revision for the current task.

The conversation tool is read-only. It cannot install, create, edit, upgrade, deprecate, or remove a Skill.

### From the Web UI

Use Personal Settings to browse, install, and remove your skills. Administrators use Global Skills for deployment-wide and shared-agent skills.

## Creating Your Own Skills

You can create custom skills to teach Stella your workflows. A skill is a directory containing a `SKILL.md` file.

### Skill Format

```markdown
---
name: my-deploy-script
description: Deploy the application to production.
---

# Deploy to Production

Follow these steps to deploy:

1. Run the test suite and confirm all tests pass.
2. Build the production bundle.
3. Push to the production branch.
4. Verify the deployment is healthy.

Always ask the user for confirmation before pushing to production.
```

### Frontmatter Fields

| Field                      | Required | Description                                           |
| -------------------------- | -------- | ----------------------------------------------------- |
| `name`                     | Yes      | Lowercase with hyphens, max 64 characters             |
| `description`              | Yes      | One-line summary shown in search results              |
| `disable-model-invocation` | No       | Prevent automatic selection while allowing direct use |

### Saving a Custom Skill

For a managed Skill, create the directory locally, package it as a ZIP, and upload it in the Web UI to an explicit destination. For a project Skill, add the directory directly under `.agents/skills/` in the project repository.

## Tips

- **Start by searching.** Before creating a skill from scratch, check the marketplace in the Web UI.
- **Keep skills focused.** One skill per task. A skill for "deploy" and a skill for "rollback" is better than one skill that tries to do both.
- **Use project skills for team workflows.** Put shared skills in `.agents/skills/` in your repository so everyone on the team benefits.
- **Test skills by loading them.** After creating a skill, ask Stella to load it and try the workflow to verify the instructions work.

## Upgrading older builtin skill settings

Bundled skills follow their owning plugin’s enabled state. Older per-Agent builtin skill disable settings no longer apply and do not block upgrades. Managed skill activation settings and project `.agents/skills/` discovery are unchanged.
