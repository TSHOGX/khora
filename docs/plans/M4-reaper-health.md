# M4 — Reaper, health quadrant & live reconcile

> Forward plan (expires as shipped, per `plans/README.md`). Delivers the
> engine's **first autonomous background control loop**: it classifies every
> sandbox's health (§4.3 four quadrants) and, per quadrant, drives the automatic
> and involuntary lifecycle transitions the state machine (§4.1) already names but
> nothing yet triggers — `auto-pause`, `evict`, and the *live* (not just
> boot-time) `crash-pause`.

## Why now (ladder rationale)

M3 made the transport surface real (PTY, `Connect`, multi-connection fan-out), so
the **connection-activity** signal the §4.3 `idle` quadrant is defined against can
finally exist. M4 consumes that plus the two-layer L1/L2 probe M2 built for
startup reconcile, and generalizes reconcile from *once at boot* to *periodic
against a live orchestrator*. This is the last piece of the lifecycle foundation
before storage (M5) and the security-critical egress plane (M6) layer on.

## Settled decisions feeding this milestone

Four design points were resolved before writing this plan (their reasoning lands
in `deep-dives/reaper-health.md`, the register entries are then removed):

- **Activity signal (§4.3 / §5.E).** Measured **guest-side**: `sandboxd` stamps a
  single sandbox-global "last I/O" time in the paths I/O already flows through
  (`proc.broadcast()` output, `writeInput()` input) and reports idle-duration on
  the existing `Health` probe. Chosen over host-side proxy stamping because a
  detached process produces output into its ring with **no client attached**
  (§3.2) — host-side observation would false-idle and auto-pause a working agent.
  I/O-based (not CPU) matches the spec's idle definition.
- **TTL ownership (§3.4).** `idle_timeout` + `retention` are **per-sandbox Binding
  fields** (stable across resume, materialized each placement) with an engine-wide
  default when a create omits them. They have a live consumer now (the reaper), so
  this is a justified model+API growth, not a speculative reservation (§1.3).
- **Scope.** Quadrant + reaper + periodic reconcile ship as **one milestone / one
  gate** — the health model is only coherent whole.
- **`guest_user` run-as** (open-decisions §4) stays **deferred** — unrelated to the
  reaper; keeps M4 focused.

## Scope (what M4 delivers)

1. **Activity signal** — `Health.Check` reports sandbox-global idle duration;
   `sandboxd` stamps last-I/O in `broadcast`/`writeInput`; host `Client.Health`
   surfaces it. (§5.E / transport D4.5)
2. **TTL Binding** — `model.Sandbox` gains `IdleTimeout`/`Retention`; store
   columns; `CreateSpec` + `POST /v1/sandboxes` optional inputs + engine default.
3. **Health classifier** — `classify(rec) → {healthy | idle | transport-down |
   dead-recoverable}` from L1 (`Adopt`/live-handle probe) × L2 (`Health`) ×
   activity (idle duration). (§4.3 four quadrants)
4. **Reaper daemon** — periodic loop driving, per quadrant and through the §4.4-B
   transition lock + CAS: `idle` past `idle_timeout` → `auto-pause`;
   `dead-recoverable` → `crash-pause` (involuntary, §4.2); `paused` past
   `retention` → `evict`; `transport-down` → **wait** (PID1 restarts same-IP,
   §5.E.1), never crash-pause. Distinct event Kinds (`auto-paused` vs
   `crash-paused` vs user `paused`).
5. **Live-safe periodic reconcile** — generalize `Reconcile` to run against a live
   orchestrator: `TryLock`-skip sandboxes with a held transition lock (in-flight
   resume/verb), and protect live-map placements from orphan reclaim. Startup
   reconcile is unchanged; the periodic call is added. (§4.4-A)

## Acceptance (correctness lives in the tests, not the build — CLAUDE.md)

Unit (fake substrate, `internal/lifecycle`):
- Classifier truth table: each of the four quadrants from its L1×L2×activity input.
- `auto-pause` emits Kind `auto-paused`; `evict` past `retention` → `destroyed`.
- Reaper `TryLock`-skips a sandbox whose transition lock is held (no race with an
  in-flight resume; no misclassification into `dead-recoverable`).
- Periodic reconcile leaves a live `running` survivor's placement untouched.

Substrate e2e (`test/*_substrate_test.go`, on `sbx` — ADR-0002):
- **False-idle guard (the D2 property):** a detached process still producing output
  with no client attached is **not** auto-paused.
- Idle sandbox (no I/O) → `auto-paused` after `idle_timeout`.
- A killed `running` container → `crash-paused` by the **live** reaper (proving it
  is not only the boot-time path).
- Transport child killed → classified `transport-down`; PID1 restarts it on the
  **same IP**; **no** crash-pause / no cold-start / no new epoch.
- `paused` sandbox past `retention` → `evict` → `destroyed`.
- Fault-injection at the reaper-vs-verb seam (reaper fires while a user verb holds
  the lock).

Gate: `go build` + `go vet` + the above e2e on `sbx`.

## Out of scope (M4 boundary)

- **Object-model refcount / GC sweep** (§4.3 uniform-object invariant) — that is
  **M7**. M4's `evict` deletes the Overlay **synchronously** via the existing
  `Destroy` path; it does **not** delete bytes by refcount. The reaper here drives
  *lifecycle-state* only.
- Volume (M5) / Network egress (M6) / resource-limit wiring — no reaper dependency.
- `guest_user` run-as enforcement (open-decisions §4, deferred).
- Node disk-pressure admission gate (§4, coupled to the oversubscription choice).
- Event-stream external schema / replay window (a later additive increment).

## Tuning knobs (open-decisions-style; conservative defaults, revisable)

Default `idle_timeout` and `retention` are pure numeric knobs (like the transport
ring/window values). M4 picks conservative defaults, notes them at the config
site, and leaves them tunable — they do not affect any invariant's shape.
