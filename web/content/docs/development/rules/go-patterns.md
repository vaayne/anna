---
title: Go patterns
description: Recurring correctness patterns for Stella's Go backend.
---

## Locks and happens-before

**Invariant.** A done/signal channel closes only after the result fields observers
will read have been assigned. The winner path owns both assignment and close;
losers just return.

**How it breaks.** Moving a slow call outside a lock can break the happens-before
contract other paths rely on. In PR #669, `Close()` used to hold the lock across
a 30s container `Stop`, so `closed == true` implied `closeErr` was already
written. After `Stop` moved off-lock, watcher/loser paths could close `done`
before the winner wrote `closeErr`; a concurrent second `Close()` read stale nil,
and `Done()` observers fired while the container was still stopping.

**Fix.** When changing lock granularity, re-audit every reader of the guarded
state. Keep done-channel close in the winner path, after all result fields are
assigned.

**Source.** PR #669.

## Redaction is never gated

**Invariant.** Hiding more is always safe. Redaction sets and deny-lists must be
read unconditionally, not through capability checks that decide what a caller may
expose.

**How it breaks.** In PR #670, bash-output redaction was read through the
vault-capability resolver. That resolver returns nil for group sessions, so a
scoped token was recorded into the redaction set but dropped on the read side,
leaking plaintext into a multi-user group transcript.

**Fix.** Gate only capability methods. In review, trace the record path and read
path separately; redaction paths must not depend on session capability.

**Source.** PR #670.

## No-replace file install

**Invariant.** Restore-on-miss and cache-fill paths must not replace a local file
that appears after the miss check.

**How it breaks.** `stat miss -> fetch -> write temp -> os.Rename(tmp, target)`
has a TOCTOU hole: POSIX rename replaces the target, so a concurrent local write
can be silently overwritten by older remote content.

**Fix.** Install with `os.Link(tmp, target)` and treat `EEXIST` as "concurrent
local write wins"; keep deferred temp removal. Plain temp-plus-rename is only
correct for owned first-party writes where clobbering is intended, such as
`FSStore.Put`.

**Source.** PR #675.

## Package dependency direction

**Invariant.** Four rules. The first three are one table in `internal/boundary_test.go`, an
AST guard with no linter config and no shell script; adding a guarded tree is
one row:

1. `pkg/**` never imports `internal/**`. `pkg/` is the contract surface plugins
   compile against; the arrow points one way only. Guard: the `pkg` row.
2. `internal/platform/**` imports only the standard library, third-party
   modules, `pkg/**`, and other `internal/platform/**`. It is the infrastructure
   floor: config, the `STELLA_HOME` layout, blob storage, observability, CLI
   plumbing, diagnostics, build version, the bundled xberg CLI. `_test.go` files
   may additionally import `internal/db/dbtest`, the embedded-PostgreSQL
   harness — a test-only edge to a test-only package creates no production
   dependency. Guard: the `internal/platform` row.
3. `internal/core/**` adds other `internal/core/**` and `internal/authz` to
   platform's whitelist (plus `internal/platform/config`, which platform already
   covers). Guard: the `internal/core` row.
4. A leaf type does not live inside a hub package. If package A is imported by
   twenty packages and imports twenty-five, the types other packages actually
   need belong in `internal/core`, not in `A/<leaf>`.

`plugins/core` is the fixed release runtime, independent of configurable
plugin identities. It may import only the exact repository packages
`internal/plugin/manifest` (shared installation primitives) and
`resources/binaries` (embedded assets); its tests may also read `resources`.
Internal runtime consumers may import the exact `plugins/core` package.
This exception does not extend to `plugins/core` subpackages or other
replaceable plugins. The architecture guard enforces both directions.

**How it breaks.** `internal/agent` was both hub and kernel. Its leaves —
`toolmeta`, `access`, `agentctx`, `agenterr`, `providercred` — carried 15–16
external consumers each, so `memory`, `vault`, `connections`, and
`observability` all appeared to depend on `agent` while `agent` depended back on
them. The compiler saw no cycle (the leaves never imported the agent root), but
every author working in those packages had to re-derive that by hand before
adding an import.

**Fix.** Move the leaf kernels to `internal/core/<name>`, package name unchanged,
so the change is import lines only. To decide whether a package belongs in
`core`, apply rule 2 literally: if it needs anything outside the whitelist it is
a domain package with a runtime dependency and it stays where it is.
`internal/agent/settingspolicy` is the worked example — its `Available()` takes a
`runtime.RunnerParams`, so it is policy, not kernel. Widening the `core`
whitelist is a reviewed act: it redefines "kernel" for the whole repo.

The same test decides `platform` membership, and it is machine-decidable rather
than a matter of taste: a package moves to `internal/platform/<name>` only if,
after the move, it imports nothing under `internal/` except
`internal/platform/**`. `internal/db` fails that test and stays put —
`internal/db/authstore.go` implements `internal/auth`'s store types, so db
depends on the auth domain, infrastructure flavour notwithstanding. When only
one subpackage fails the rule, move the subpackage out instead of abandoning the
parent: `observability/tracehook` needed `internal/core/toolmeta` (now
`pkg/toolmeta`), so it became
`internal/agent/tracehook` and the rest of `observability` moved to platform.

**Source.** `internal/` layout refactor, phase 1 (`internal/core` extraction) and
phase 3 (`internal/platform` grouping).
