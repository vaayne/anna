# Model management

## Tiered models

Two tiers for different workloads, each falls back to `model` when not set:

| Tier   | Config Field   | Use Case                        |
| ------ | -------------- | ------------------------------- |
| strong | `model_strong` | Heavy reasoning, complex tasks  |
| fast   | `model_fast`   | Quick responses, simple queries |

```yaml
model: claude-sonnet-4-6 # default
model_strong: claude-opus-4-6 # optional
model_fast: claude-haiku-4-5 # optional
```

## Deployment defaults and per-agent overrides

Model configuration has two layers. An administrator sets the deployment defaults under **Admin -> Models**: the three agent tiers with their thinking levels, plus the vision and embedding roles. Each agent overrides only the fields it needs; an empty agent field inherits the deployment default rather than meaning "no model". Vision and embedding have no per-agent form and stay deployment-wide.

## Managing models

Models are managed through the Web UI (web UI). You can browse available models, switch the active model, and refresh the model cache from the Models page.

## Provider setup

Create provider rows in the Web UI or API. Select **Anthropic**, **OpenAI**, or **OpenAI-compatible**, then store that provider's API key and base URL (when required) in its row. Provider credentials and base URLs are not read from server environment variables.

An enterprise provisioner can assign an Agent-specific API key for a canonical
Provider through the Agent credential API. The override applies to every call
that Agent makes through the Provider, including Vision when it selects the same
Provider. Deleting the override restores the global key. Assigned users consume
the override but cannot change it unless they created the Agent; administrators
can always manage it. Overrides never change the Provider type, endpoint, model
catalog, or enabled state, and the Web UI has no override editor yet.

## Vision

An administrator picks the vision model once for the deployment under **Admin -> Models**. In ordinary one-to-one sessions, each image gets one immutable text baseline at ingestion, keyed to the image itself, so the same bytes carry the same description in every message and session that references them. A model declared with `image` input receives image pixels only in the active turn and its tool loop; text-only and undeclared models receive the baseline immediately, and every model receives it on later turns. If no baseline can be produced, the stable marker is `[Image baseline unavailable.]`. Original pixels remain in authorized Web history. Agents can inspect an image with `view_image`: image-capable parent models receive pixels, while other parents receive untrusted textual evidence from the vision service or generic baseline. A targeted prompt requires a usable vision model; an explicitly text-only vision model is never sent image bytes.

## Runtime switching

Use the Models page in the Web UI to switch the active model.
