# Goal model

Durable, async work that survives restarts and is **accepted, not just finished**. Use this for work that outlives a single conversation: long research, multi-step builds, work that may pause for input, and work that needs an acceptance contract met before it counts as done.

A **goal** is one recursive entity. A **root** goal is the user's objective and is always **planned first** — it is a **composite** that decomposes into **child** goals (same shape, all the way down). A **leaf** child is executed directly by a worker; a **composite** child is planned and decomposed in turn. `kind ∈ {leaf, composite}`. There is no top-level leaf: every goal goes through plan → decompose → run before it can be accepted.

Completion is **derived, never asserted**. A goal converges through a bounded rework loop until its acceptance contract passes — you never mark one done by hand. The worker submits evidence; the acceptance contract decides; if it falls short, the worker is dispatched again with the gaps to repair.

## Who authors goals

Two surfaces author goals, both over the same goal HTTP API:

- **You, the agent** — via `goal_create` when available. It creates a goal and runs it autonomously: the server **plans first** (decomposes it into verifiable sub-tasks), then the dispatcher runs each child and converges to acceptance, with no further prompting. You do not choose leaf vs composite or call plan/approve/activate — just write a clear, self-contained intent. This is how you give yourself long-running work that outlives the current conversation; check back with `goal_list` or `goal_get`. For goals that need a human approval gate (`review_policy=human`), the dispatcher still plans automatically but parks the composite at `blocked(needs_plan_approval)` for the user to approve from the Web UI operator surface. When you report a created goal back to the user, mention its title — chat surfaces render a rich status card for it automatically, so never paste only the raw UUID.
- **The user** — from the Web UI (Work space), backed by the same goal HTTP API.

Authoring and working are separate roles: once a goal is active you may also be handed it as a **worker** (see the `goal_control` contract below).

Before reaching for a goal at all, check you actually need one:

- `session_create` — synchronous focused work in a persistent child session, returned inline. Use this first for short research, review, or drafting.
- **goal** — async, durable, survives restarts, can block on input, converges through an acceptance contract. This is for work tracked to acceptance: create it yourself with `goal_create`, or the user authors it from the Web UI.
- **workflow** — a reusable versioned plan saved from an accepted composite goal. Running a workflow creates a fresh goal tree; it never reopens a done goal. Use it when the same accepted plan should replay with only inputs changing.
- `scheduler__job_create` — a time trigger, not the work itself. For long or reviewable scheduled work, schedule a workflow or a prompt that creates a goal rather than doing the work inline.

## Lifecycle

Every goal — root or child, leaf or composite — runs through one state machine:

```text
draft ──activate──▶ pending ──claim──▶ active ──submit──▶ (acceptance fold)
  │                     │                │                      │
  │                     │                ├─ block ─▶ blocked ───┤ pass ─▶ done(accepted)
  │                     │                │             │        └ fail ─▶ active (rework) or done(failed)
  │                     │                │             └─resolve─▶ pending
  └─ cancel ──▶ done(cancelled)
```

- `done_reason=accepted` — terminal-good; the accepted output is frozen.
- `done_reason=failed` — no rework path remains.
- `done_reason=cancelled` — user/system cancellation.
- `blocked` is **recoverable**, not terminal. Block reasons include `budget_exhausted`, `needs_plan_approval`, `planning_invalid`, `needs_verdict`, `env_unavailable`, and `contract_conflict`. Responsibility matters: model failures spend business budget; environment/contract failures park for human action; flaky infrastructure retries outside business budget until its cap.

Acceptance is a separate projection from lifecycle: `pending | passed | failed`. A goal reaches `done(accepted)` only when its contract's acceptance fold passes.

## Composition and dependencies

A composite holds child goals produced by a **decomposition** (the only way children come into being — you cannot hand-attach a child). Siblings can declare dependency **edges**: `hard` blocks readiness, `soft` is advisory. Only an upstream's **accepted** output flows downstream. Rollup is automatic:

- all required children `done(accepted)` → composite's acceptance can pass
- a required child `done(failed|cancelled)` → parent fails
- a required child blocked → parent waits; the block is derived from children, not stored on the parent
- a hard dependency whose upstream dies is derived from edges and the upstream `done_reason`

Decomposition is automatic: `goal_create` produces a composite whose planning runs on its own, materializing children and activating them so the dispatcher runs them — no manual steps. Structural planning errors are repaired in the same planning session with `prior_errors`; if those repairs are exhausted, the goal parks at `blocked(planning_invalid)`. When a goal needs a human approval gate (`review_policy=human`), the dispatcher still plans automatically but parks the composite at `blocked(needs_plan_approval)` with the proposed `{children, edges}` stored on the goal; the user can approve or reject it from the Web UI operator surface.

## Workflows and scheduled runs

An accepted composite goal can be saved as a reusable workflow. A workflow is not a reopened goal; each run instantiates a fresh root goal tree from the saved version. Use workflows when the same plan should replay with only inputs changing. Use a plain goal or scheduler chat job when each occurrence should be re-planned from scratch.

For "save this goal and run it daily": save the accepted goal as a workflow, then create a scheduler workflow job. Scheduled workflow fires skip only when the previous run successfully instantiated a root goal that is still active; failed instantiation does not block the next tick, and stalled instantiation is resumed instead of duplicated. Partially frozen workflows require an explicit allow-replan opt-in.

## Worker: the `goal_control` contract

If you see a `goal_control` tool in your toolset, you are a worker. The goal's intent and acceptance criteria arrive as your prompt. Do the work, then call `goal_control` **exactly once** with one terminal action:

- `submit` — provide `evidence` (summary + optional artifacts) and `output` when the work meets the acceptance criteria.
- `fail` — report `reason` plus `blocked_by` (`env_unavailable` or `contract_conflict`) when a human must fix the environment or contract.
- `decompose` — **when dispatched to plan a goal** — return a `decomposition` `{children, edges}`. Each child needs `key`, `title`, `intent`, `kind` (`leaf|composite`), `required`, and `acceptance_contract`; edges declare hard/soft deps by child key. If the prompt includes `prior_errors`, fix those structural errors in the next `decompose` call. If the goal cannot be decomposed, use `fail` instead.

Rules:

- Always end with a terminal action. A final text response without `goal_control` is a protocol failure; you get exactly one repair turn, then the attempt fails.
- `submit` does **not** mark the goal done — the acceptance contract decides. If your previous attempt fell short, the gaps come back in the next prompt; address them.
- Use `fail` with `blocked_by` when you truly need a human or external dependency. Do not fake completion to avoid failing.

## Timeline and recovery

Each goal has an append-only timeline. The Web UI uses it as the human surface for plan submissions, attempt start/finish, acceptance results, lifecycle changes, and human messages; one-shot execution sessions are internal audit plumbing and are hidden from normal user session lists. If a non-dependency blocked goal receives a human timeline message, Stella records the message, authorizes one extra attempt, and dispatches it with that guidance. Dependency-blocked goals record the message but do not retry until the upstream changes or is waived.

Attempts carry a lease and heartbeat. If a worker crashes or Stella restarts, the lease expires and the dispatcher reclaims the goal if the convergence budget remains. Submitted evidence and terminal state are durable because they are written to the append-only acceptance ledger and goal timeline.
