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
	"os"
	"time"

	"warren/internal/lifecycle"
	"warren/internal/model"
	"warren/internal/network"
	"warren/internal/rootfs"
	"warren/internal/runtime"
	"warren/internal/store"
	"warren/internal/transport"
	"warren/internal/volume"
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

	// EventRetention is §4.4-C's replayable-window size. Zero = NEVER TRIM, and that
	// zero is the shipped behavior rather than a "fill in a default" sentinel — the
	// opposite of the two TTLs above. See lifecycle.Deps.EventRetention for why the
	// engine must not invent this number.
	EventRetention time.Duration

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

	// SecondHopUID pins the SECOND hop (§2.4 invariant 2): the uid of the host-local
	// process sitting behind `egress.via[].endpoint`, whose own egress must be held
	// under the same L0 floor BY THE KERNEL rather than by its own discipline.
	//
	// A DEPLOYMENT input, never a declaration field (§2.4 / §6.2): the engine still
	// does not know or ask what is behind that endpoint — only which local uid's
	// egress to pin. A pointer because uid 0 is a real uid, and "the operator said
	// root" must not read as "the operator said nothing".
	//
	// Unset ⇒ no second-hop table is installed AND `via[]` declarations are refused at
	// the write point. Refused rather than accepted-unenforced, because accepting one
	// records a declaration whose own invariant the engine cannot deliver, leaving a
	// confused deputy live while every FIRST-hop assertion still passes (measured: hop
	// 2 counts zero in `forward`, so the first-hop floor never evaluates it —
	// network.md D5 ㉔).
	//
	// Set to THIS PROCESS'S OWN euid ⇒ New refuses to assemble. That value would put
	// the engine's own guest dials under the second-hop floor, whose daddr set contains
	// the sandbox address space — see network.ValidateSecondHopUID.
	SecondHopUID *int

	// ListenAddr is the §6.1 API listen address, validated at assembly time against
	// ADR-0007's loopback requirement. EMPTY = not validated, which is what every
	// in-process caller wants: a test serving through httptest.NewServer never
	// listens on this address at all, so there is nothing to classify.
	//
	// The refusal lives HERE rather than in cmd/ for the reason SecondHopUID's does:
	// the substrate harness builds an Engine directly, so a flag-level check would be
	// bypassed by every test and by any embedder. What differs is the empty case —
	// a uid is either given or not, while a listen address only exists for a caller
	// that is about to serve on it.
	ListenAddr string
	// InsecureListen is the deployment saying OUT LOUD that ListenAddr may be
	// routable (ADR-0007). It buys exactly one thing: ValidateListenAddr is not
	// consulted. The engine gains no authentication either way — this only records
	// that publishing an unauthenticated control plane was a decision rather than an
	// omission, which is the whole difference between a boundary and a default.
	InsecureListen bool
	// ResolveHost resolves a NAME in ListenAddr. nil → net.LookupHost. Injectable so
	// the name arm is pinned against a fixture in a root-free unit test rather than
	// against this machine's resolver (the same shape, and the same reason, as
	// network.Deployment.HostLocal).
	ResolveHost ResolveHost

	// HostPublishIP is the host address published endpoints bind to (§5.D: the engine
	// delivers a `host ip:port`, and what sits in front of it is outside the engine).
	// Empty → loopback, which is the conservative default: publish is an EXPLICIT
	// inbound exception (§2.1 ③), so external reachability must come from a decision
	// rather than from omitting a flag.
	HostPublishIP string

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
	// health backs /healthz, /readyz and /metrics (see health.go). A field rather
	// than package state so two Engines in one test process do not share counters.
	health health
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

	dep := network.Deployment{SecondHopUID: cfg.SecondHopUID}
	// FAIL FAST, BEFORE ANY SUBSTRATE STATE EXISTS. A second-hop uid equal to this
	// process's own euid puts the engine's OWN dials to guest veth addresses under the
	// second-hop floor (its daddr set contains the whole sandbox address space), which
	// takes out readiness, liveness and every RPC for every sandbox at once — and
	// intermittently, since skuid misses packets. Refusing here rather than at boot's
	// InstallSecondHopTable is deliberate: this is the LAST point at which nothing has
	// been installed and no request has been served, and an engine that has already put
	// that table in the kernel is past the point where discovering this helps.
	//
	// The check lives in the assembly rather than in cmd/ so every path reaches it —
	// the substrate harness builds an Engine directly and would otherwise bypass a
	// flag-level check entirely.
	if err := network.ValidateSecondHopUID(dep, os.Geteuid()); err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}

	// ADR-0007's enforcement half, in the same fail-fast position and for the same
	// reason: this is the last point at which nothing has been installed and nothing
	// has been served. An engine that has already put the second-hop table in the
	// kernel and begun serving is past the point where learning "that listener is
	// published" helps anyone.
	if cfg.ListenAddr != "" && !cfg.InsecureListen {
		if err := ValidateListenAddr(cfg.ListenAddr, cfg.ResolveHost); err != nil {
			return nil, err
		}
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
	rt.HostPublishIP = cfg.HostPublishIP

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
		EventRetention:        cfg.EventRetention,
		FaultAfterColdStart:   cfg.FaultAfterColdStart,
		FaultAfterVolumeBytes: cfg.FaultAfterVolumeBytes,
		NodeCapacity: lifecycle.NodeCapacity{
			DiskByte:   cfg.NodeDiskByte,
			MemoryByte: cfg.NodeMemoryByte,
		},
		Deployment: dep,
	})

	// outbox publisher → a log sink (§4.4-C: the engine only guarantees reliable
	// write to a replayable stream; a real subscriber lives outside the engine).
	// Default retry policy: bounded attempts, capped + jittered backoff (see
	// store.PublisherConfig).
	pub := store.NewPublisher(st, store.NewDedupSink(func(ev store.Event) {
		log.Printf("event: %s sandbox=%s state=%s seq=%d id=%s", ev.Kind, ev.Sandbox, ev.State, ev.Seq, ev.ID)
	}))

	reaper := lifecycle.NewReaper(med, cfg.SweepInterval, cfg.TransportDownBudget)

	eng := &Engine{Mediator: med, Store: st, Publisher: pub, Reaper: reaper}
	eng.health.startedAt = time.Now()
	return eng, nil
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

// InstallSecondHopTable installs the ENGINE-LEVEL table pinning the second hop
// (§2.4 invariant 2's second half). Call once at boot, BEFORE serving.
//
// Before serving is the requirement, and it mirrors why the rules are installed
// before the isolated unit runs: a `via[]` declaration accepted while that table is
// absent leaves an unpinned confused deputy for as long as the gap lasts. Idempotent
// — it re-renders and re-installs whole, and with no uid supplied it removes any
// stale table a previous run left under a uid this one does not have.
func (e *Engine) InstallSecondHopTable(ctx context.Context) error {
	return e.Mediator.InstallSecondHopTable(ctx)
}

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
