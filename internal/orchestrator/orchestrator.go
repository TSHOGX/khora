// Package orchestrator is the single engine-host assembly (§1.1): it wires the
// persistent store (SQLite, ADR-0004) + the two providers (local_runsc,
// host_overlayfs) + the §4 lifecycle mediator + the transport dialer, and exposes
// the minimal §6.1 sandbox-centric API. Deployment target is "single orchestrator
// binary + one config + local store" with no heavy external dependency (§1.1).
//
// Swapping store / provider is registering an implementation (§1.3), not editing
// this file — the assembly holds the seams, not concrete assumptions.
package orchestrator

import (
	"context"
	"fmt"
	"log"
	"time"

	"engine/internal/lifecycle"
	"engine/internal/model"
	"engine/internal/rootfs"
	"engine/internal/runtime"
	"engine/internal/store"
	"engine/internal/transport"
	"engine/internal/volume"
)

// Config is the orchestrator's local configuration (§1.1 one config).
type Config struct {
	// TemplatesDir holds Template directories (static; TemplateID → dir).
	TemplatesDir string
	// StateDir is the root for overlay materialization + runsc state/bundles.
	StateDir string
	// HealthTimeout bounds the ⑦ readiness gate (guest-runtime D5.1). Zero → the
	// mediator's default (lifecycle.New), the single defaulting site for this knob.
	HealthTimeout time.Duration
	// DrainGrace bounds the §4.2 pause drain barrier (D4.4): how long a voluntary
	// pause waits for in-flight engine-issued fs writes before freezing anyway.
	// Zero → the mediator's default. Negative disables the drain entirely, which
	// only a test wants (it restores the truncate window the barrier closes).
	DrainGrace time.Duration
	// Sudo prefixes runsc/ip with sudo (dev convenience). Production runs the
	// orchestrator as root and leaves this false.
	Sudo bool
	// StorePath is the SQLite store file (ADR-0004). Empty → an in-memory SQLite
	// (":memory:") for tests that don't need durability across a New() call.
	StorePath string

	// DefaultIdleTimeout / DefaultRetention are the engine-wide TTL defaults
	// (§4.3 / reaper-health D5) applied to a create that omits them. Zero →
	// the mediator's built-in conservative defaults (lifecycle.New).
	DefaultIdleTimeout time.Duration
	DefaultRetention   time.Duration

	// SweepInterval / TransportDownBudget are the reaper's tuning knobs
	// (reaper-health D5). Zero → NewReaper's conservative defaults.
	SweepInterval       time.Duration
	TransportDownBudget time.Duration

	// VolumesDir holds §5.C volume storage, one unit per volume. Empty →
	// StateDir/volumes, a SIBLING of the overlay state root rather than nested inside
	// it: each subsystem's orphan sweep enumerates its own root, so nesting would put
	// one subsystem's units inside the other's enumeration.
	VolumesDir string

	// NodeDiskByte / NodeMemoryByte are the §3.4 no-oversubscribe budgets the
	// admission gate admits against (§1.1 one config). Zero leaves that axis
	// UNMETERED — deliberately not defaulted: the engine must not invent a capacity
	// number and start refusing creates on a guess. Operators set them below the
	// raw device/RAM size, since neither budget models the store, logs, runsc
	// state, or per-sandbox sentry/gofer overhead.
	NodeDiskByte   int64
	NodeMemoryByte int64

	// FaultAfterColdStart is a TEST-ONLY injection hook forwarded to the
	// mediator to simulate a crash mid-transition. nil in production.
	FaultAfterColdStart func(id model.SandboxID)
	// FaultAfterVolumeBytes is a TEST-ONLY hook forwarded to the mediator: a crash inside
	// snapshot/clone after the bytes and cap exist but before the row is inserted (§5.C /
	// volume.md D7.3). nil in production.
	FaultAfterVolumeBytes func(id model.VolumeID)
}

// Engine is the assembled engine host.
type Engine struct {
	Mediator  *lifecycle.Mediator
	Store     store.Store
	Publisher *store.Publisher
	// Reaper is the §4.3 autonomous control loop (auto-pause / crash-pause /
	// evict + periodic reconcile). Built here; started by the caller AFTER
	// startup Reconcile, via Reaper.Run in its own goroutine.
	Reaper *lifecycle.Reaper
}

// New wires the assembly. It does not start any server, and it does NOT run
// startup reconciliation — call Reconcile explicitly after New (so tests can
// inspect the pre-reconcile store), then HTTPHandler to serve.
func New(cfg Config) (*Engine, error) {
	if cfg.TemplatesDir == "" || cfg.StateDir == "" {
		return nil, fmt.Errorf("orchestrator: TemplatesDir and StateDir are required")
	}

	rp, err := rootfs.NewHostOverlayFS(cfg.StateDir+"/overlays", cfg.TemplatesDir, cfg.StateDir+"/layers")
	if err != nil {
		return nil, err
	}
	rt, err := runtime.NewLocalRunsc(cfg.StateDir+"/runsc-root", cfg.StateDir+"/runsc-state")
	if err != nil {
		return nil, err
	}
	rt.Sudo = cfg.Sudo

	// §5.C volume backend. The local arm is the default; registering a different one
	// is a §1.3 registration, not an edit to the callers above.
	volumesDir := cfg.VolumesDir
	if volumesDir == "" {
		volumesDir = cfg.StateDir + "/volumes"
	}
	vb, err := volume.NewLocalBtrfs(volumesDir)
	if err != nil {
		return nil, err
	}

	// persistent store (ADR-0004) — register-an-implementation swap from Memory,
	// no caller change (§1.3). This durability is what CREATES the §4.4-A
	// reconciliation need the mediator's Reconcile answers.
	storePath := cfg.StorePath
	if storePath == "" {
		storePath = ":memory:"
	}
	st, err := store.OpenSQLite(storePath)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: open store: %w", err)
	}

	med := lifecycle.New(lifecycle.Deps{
		Store:                 st,
		Runtime:               rt,
		Rootfs:                rp,
		Volume:                vb,
		Dial:                  transport.Dial,
		HealthTimeout:         cfg.HealthTimeout,
		DrainGrace:            cfg.DrainGrace,
		DefaultIdleTimeout:    cfg.DefaultIdleTimeout,
		DefaultRetention:      cfg.DefaultRetention,
		FaultAfterColdStart:   cfg.FaultAfterColdStart,
		FaultAfterVolumeBytes: cfg.FaultAfterVolumeBytes,
		NodeCapacity: lifecycle.NodeCapacity{
			DiskByte:   cfg.NodeDiskByte,
			MemoryByte: cfg.NodeMemoryByte,
		},
	})

	// outbox publisher → a log sink (§4.4-C: the engine only guarantees reliable
	// write to a replayable stream; a real subscriber lives outside the engine).
	// Default retry policy: bounded attempts, capped + jittered backoff (see
	// store.PublisherConfig).
	pub := store.NewPublisher(st, store.NewDedupSink(func(ev store.Event) {
		log.Printf("event: %s sandbox=%s state=%s seq=%d id=%s", ev.Kind, ev.Sandbox, ev.State, ev.Seq, ev.ID)
	}))

	reaper := lifecycle.NewReaper(med, cfg.SweepInterval, cfg.TransportDownBudget)

	return &Engine{Mediator: med, Store: st, Publisher: pub, Reaper: reaper}, nil
}

// SeedTemplates runs the boot seed (§4.3 + §4.4-A: ensure a builtin `templates` row per
// static Template directory). Call once at boot BEFORE serving any create —
// the sandboxes.template FK (§4.3) needs the builtin rows present.
func (e *Engine) SeedTemplates(ctx context.Context) error {
	return e.Mediator.SeedBuiltinTemplates(ctx)
}

// Reconcile runs the §4.4-A startup crash-reconciliation (orphan reclaim +
// running re-adoption). Call once at boot, before serving the API.
func (e *Engine) Reconcile(ctx context.Context) error { return e.Mediator.Reconcile(ctx) }

// SweepOrphanOverlays runs the §4.4-A orphan-UNIT sweep (reclaim Overlay storage
// units backing no sandbox row). At boot this is where the accumulated residue of
// every prior run is collected: the in-flight keep-set is empty, so every unit
// without a record is a true orphan. Call after Reconcile, which deletes crashed
// `creating` records and so decides which units are orphans.
func (e *Engine) SweepOrphanOverlays(ctx context.Context) error {
	return e.Mediator.SweepOrphanOverlays(ctx)
}

// SweepOrphanLayers runs the §5.B / §4.4-A orphan-layer sweep (reclaim Promote-built layer
// bytes with no live templates row). Call once at boot after SeedTemplates so a
// crashed Promote's staging layer is reclaimed; the reaper repeats it per tick.
func (e *Engine) SweepOrphanLayers(ctx context.Context) error {
	return e.Mediator.SweepOrphanLayers(ctx)
}

// RepairVolumeCaps re-asserts every volume's RECORDED cap onto its storage (§5.C /
// volume.md D7.3). Call at boot, in the same position as the orphan sweep.
//
// It exists because writing a snapshot's cap is necessarily the SECOND substrate call, so
// a crash in between leaves a registered, live, UNCAPPED volume — and that is invisible
// afterwards on its own, since the limit read cannot distinguish "never capped" from
// "deliberately uncapped". The direction is always RECORD → STORAGE: adopting the
// storage's value into the record would silently ratify exactly that crash residue.
func (e *Engine) RepairVolumeCaps(ctx context.Context) error {
	return e.Mediator.RepairVolumeCaps(ctx)
}

// ReclaimQuotaRows runs the §5.C / volume.md D7 leaked-quota-row reclamation round. The
// `parent` FK demotes this from the main path to a backstop (it makes the leak-producing
// deletion order unreachable through the API), but the residual paths — a crash between
// two deletes, and the sweep's own per-unit ordering — do not pass through user intent,
// so the round must exist and must be correct.
func (e *Engine) ReclaimQuotaRows(ctx context.Context) error {
	return e.Mediator.ReclaimQuotaRows(ctx)
}

// SweepOrphanVolumes runs the §5.C / §4.4-A orphan-volume sweep (reclaim volume
// storage backing no `volumes` row).
//
// ITS POSITION IN THE BOOT SEQUENCE IS LOAD-BEARING, not housekeeping: it must run
// BEFORE any storage unit is created. A leaked quota row does not reserve its own
// unit id, so a restart can hand that id — ALONG WITH ITS CAP — to a brand-new
// volume, which then meets ENOSPC on a limit nobody set for it (or is silently
// over-provisioned), and the engine cannot tell the difference from the inside.
// Leaving it to the periodic tick would leave a window in which exactly that
// inheritance happens (§5.C / volume.md D7.1).
func (e *Engine) SweepOrphanVolumes(ctx context.Context) error {
	return e.Mediator.SweepOrphanVolumes(ctx)
}
