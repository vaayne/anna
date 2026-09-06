---
name: html-artifact
description: Create polished single-file HTML artifacts for plans, reports, explainers, code reviews, lightweight dashboards, prototypes, and one-off editing tools. Use when the user asks for a standalone HTML file, single HTML, HTML artifact, interactive report, shareable local artifact, Alpine.js artifact, custom CSS artifact, copy/export UI, or a browser-openable deliverable instead of Markdown.
---

# Single HTML Artifact

## Default Stack

Use this stack unless the user asks otherwise:

- One `.html` file with embedded `<style>` and `<script>`.
- Custom CSS with CSS variables for theme, spacing, borders, and status colors.
- Vanilla JavaScript for static pages and simple interactions.
- Alpine.js from CDN when stateful UI would otherwise require repetitive DOM code.
- Optional CDN libraries only when they clearly reduce complexity: Chart.js for charts, Mermaid for diagrams, highlight.js for code, KaTeX for math.

Avoid build tools, package managers, JSX transpilers, large component libraries, and app frameworks unless the artifact is truly a mini-app or the user requests them.

## Workflow

1. Clarify the artifact's job: document, comparison, dashboard, prototype, editor, or review aid.
2. Choose the smallest interaction model:
   - Static reading: HTML + custom CSS.
   - Tabs, filters, toggles, forms, copy/export: add Alpine.js.
   - Many reusable components or nested state: consider Preact, but explain the tradeoff.
3. Build the first screen as the usable artifact, not a landing page.
4. Keep the layout readable: clear hierarchy, dense but calm spacing, responsive constraints, and no overlapping text.
5. Add export affordances when the user will make decisions in the artifact: copy as Markdown, JSON, prompt, or diff.
6. Verify by opening in a browser or otherwise checking the generated HTML for syntax, layout, and interaction issues.

## CSS Guidance

Prefer custom CSS over a UI framework. Define a small token set:

```css
:root {
  --bg: #f7f7f4;
  --panel: #ffffff;
  --text: #1f2328;
  --muted: #687076;
  --accent: #2563eb;
  --border: #d9dee3;
}
```

Use framework CSS only when it fits the artifact:

- Pico.css: good for document-like pages with forms, tables, and prose.
- Tailwind CDN: acceptable for quick prototypes mostly maintained by AI, but expect noisy HTML.
- DaisyUI, Flowbite, Bootstrap, Material, shadcn-style systems: usually too heavy for single-file artifacts.

## Alpine.js Pattern

Use Alpine.js when the page needs light state:

```html
<script
  defer
  src="https://cdn.jsdelivr.net/npm/alpinejs@3.x.x/dist/cdn.min.js"
></script>

<main x-data="{ tab: 'why' }">
  <button @click="tab = 'why'">Why</button>
  <section x-show="tab === 'why'">...</section>
</main>
```

Keep state close to the markup it controls. For larger logic, define named functions inside the page script and initialize them with `x-data="artifact()"`.

## Quality Bar

Before finishing:

- Confirm the file is self-contained except for intentional CDN links.
- Ensure the core content remains readable if optional scripts fail.
- Check mobile and desktop responsiveness.
- Check buttons, tabs, filters, and export actions.
- Keep the final report concise: file path, what changed, verification, and any known tradeoffs.
