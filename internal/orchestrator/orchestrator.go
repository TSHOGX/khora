// Package orchestrator is the single engine-host assembly (the engine vocabulary boundary): it wires the
// persistent store (SQLite, the SQLite store choice) + the two providers (local_runsc,
// host_overlayfs) + the lifecycle mediator + the transport dialer, and exposes
// the minimal the control-plane API sandbox-centric API. Deployment target is "single orchestrator
// binary + one config + local store" with no heavy external dependency (the engine vocabulary boundary).
//
// Swapping store / provider is registering an implementation (the extension boundary), not editing
// this file — the assembly holds the seams, not concrete assumptions.
package orchestrator

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/TSHOGX/khora/internal/lifecycle"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/network"
	"github.com/TSHOGX/khora/internal/rootfs"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/transport"
	"github.com/TSHOGX/khora/internal/volume"
)

// VolumeProvider is the typed orchestrator configuration name for a built-in
// Volume provider. The alias keeps the durable vocabulary owned by the Volume
// subsystem while making Config self-documenting to assembly callers.
type VolumeProvider = volume.ProviderName

const (
	VolumeProviderLocalBtrfs = volume.LocalBtrfsProvider
	VolumeProviderPlainDir   = volume.PlainDirProvider
)

// Config is the orchestrator's local configuration (the engine vocabulary boundary one config).
type Config struct {
	// TemplatesDir holds Template directories (static; TemplateID → dir).
	TemplatesDir string
	// StateDir is the root for overlay materialization + runsc state/bundles.
	StateDir string
	// HealthTimeout bounds the cold-start readiness gate. Zero means the
	// mediator's default (lifecycle.New), the single defaulting site for this knob.
	HealthTimeout time.Duration
	// DrainGrace bounds the FS-first pause and resume pause drain barrier (the lifecycle contract): how long a voluntary
	// pause waits for in-flight engine-issued fs writes before freezing anyway.
	// Zero → the mediator's default. Negative disables the drain entirely, which
	// only a test wants (it restores the truncate window the barrier closes).
	DrainGrace time.Duration
	// Sudo prefixes runsc/ip with sudo (dev convenience). Production runs the
	// orchestrator as root and leaves this false.
	Sudo bool
	// StorePath is the SQLite store file (the SQLite store choice). Empty → an in-memory SQLite
	// (":memory:") for tests that don't need durability across a New() call.
	StorePath string

	// DefaultIdleTimeout / DefaultRetention are the engine-wide TTL defaults
	// (health and reclamation / the health and reaper boundary) applied to a create that omits them. Zero →
	// the mediator's built-in conservative defaults (lifecycle.New).
	DefaultIdleTimeout time.Duration
	DefaultRetention   time.Duration

	// EventRetention is the transactional outbox's replayable-window size. Zero = NEVER TRIM, and that
	// zero is the shipped behavior rather than a "fill in a default" sentinel — the
	// opposite of the two TTLs above. See lifecycle.Deps.EventRetention for why the
	// engine must not invent this number.
	EventRetention time.Duration

	// SweepInterval / TransportDownBudget are the reaper's tuning knobs
	// (the health and reaper boundary). Zero → NewReaper's conservative defaults.
	SweepInterval       time.Duration
	TransportDownBudget time.Duration

	// VolumesDir holds the Volume subsystem volume storage, one unit per volume. Empty →
	// StateDir/volumes, a SIBLING of the overlay state root rather than nested inside
	// it: each subsystem's orphan sweep enumerates its own root, so nesting would put
	// one subsystem's units inside the other's enumeration.
	VolumesDir string

	// VolumeProvider selects the built-in Volume provider for newly-created
	// Volumes. Existing records are always routed by their persisted backend name.
	// Empty selects the standard local_btrfs provider.
	VolumeProvider volume.ProviderName
	// VolumeLocalBtrfs and VolumePlainDir carry provider-owned settings. Only the
	// selected provider's settings are used; unknown selections fail before the
	// engine starts serving.
	VolumeLocalBtrfs volume.LocalBtrfsConfig
	VolumePlainDir   volume.PlainDirConfig

	// NodeDiskByte / NodeMemoryByte are the resource admission no-oversubscribe budgets the
	// admission gate admits against (the engine vocabulary boundary one config). Zero leaves that axis
	// UNMETERED — deliberately not defaulted: the engine must not invent a capacity
	// number and start refusing creates on a guess. Operators set them below the
	// raw device/RAM size, since neither budget models the store, logs, runsc
	// state, or per-sandbox sentry/gofer overhead.
	NodeDiskByte   int64
	NodeMemoryByte int64

	// SecondHopUID pins the SECOND hop (the second-hop floor): the uid of the host-local
	// process sitting behind `egress.via[].endpoint`, whose own egress must be held
	// under the same L0 floor BY THE KERNEL rather than by its own discipline.
	//
	// A DEPLOYMENT input, never a declaration field (the provider security contract / the API error contract): the engine still
	// does not know or ask what is behind that endpoint — only which local uid's
	// egress to pin. A pointer because uid 0 is a real uid, and "the operator said
	// root" must not read as "the operator said nothing".
	//
	// Unset ⇒ no second-hop table is installed AND `via[]` declarations are refused at
	// the write point. Refused rather than accepted-unenforced, because accepting one
	// records a declaration whose own invariant the engine cannot deliver, leaving a
	// confused deputy live while every FIRST-hop assertion still passes (measured: hop
	// 2 counts zero in `forward`, so the first-hop floor never evaluates it —
	// network substrate measurements).
	//
	// Set to THIS PROCESS'S OWN euid ⇒ New refuses to assemble (see the check there).
	SecondHopUID *int

	// ListenAddr is the control-plane API listen address, validated at assembly time against
	// the deployment boundary's loopback requirement. EMPTY = not validated, which is what every
	// in-process caller wants: a test serving through httptest.NewServer never listens on
	// this address at all, so there is nothing to classify. The refusal lives in the
	// assembly rather than in cmd/ for the reason SecondHopUID's does (see New).
	ListenAddr string
	// InsecureListen is the deployment saying OUT LOUD that ListenAddr may be
	// routable (the deployment boundary). It buys exactly one thing: ValidateListenAddr is not
	// consulted. The engine gains no authentication either way — this only records
	// that publishing an unauthenticated control plane was a decision rather than an
	// omission, which is the whole difference between a boundary and a default.
	InsecureListen bool
	// ResolveHost resolves a NAME in ListenAddr. nil → net.LookupHost. Injectable so
	// the name arm is pinned against a fixture in a root-free unit test rather than
	// against this machine's resolver (the same shape, and the same reason, as
	// network.Deployment.HostLocal).
	ResolveHost ResolveHost

	// HostPublishIP is the host address published endpoints bind to (the network declaration: the engine
	// delivers a `host ip:port`, and what sits in front of it is outside the engine).
	// Empty → loopback, which is the conservative default: publish is an EXPLICIT
	// inbound exception (the isolation model), so external reachability must come from a decision
	// rather than from omitting a flag.
	HostPublishIP string

	// FaultAfterColdStart is a TEST-ONLY injection hook forwarded to the
	// mediator to simulate a crash mid-transition. nil in production.
	FaultAfterColdStart func(id model.SandboxID)
	// FaultAfterVolumeBytes is a TEST-ONLY hook forwarded to the mediator: a crash inside
	// snapshot/clone after the bytes and cap exist but before the row is inserted (the Volume subsystem /
	// volume cap enforcement). nil in production.
	FaultAfterVolumeBytes func(id model.VolumeID)
}

// Engine is the assembled engine host.
type Engine struct {
	Mediator *lifecycle.Mediator
	Store    store.Store
	// VolumeRegistry is the configured internal selection set. It is exposed on
	// the assembled Engine for startup/reconciliation tests; API callers still
	// address Volumes by ID and never receive provider handles.
	VolumeRegistry *volume.Registry
	Publisher      *store.Publisher
	// health backs /healthz, /readyz and /metrics (see health.go).
	health health
	// httpHandlers gates and tracks handlers entering the engine mux. Shutdown closes
	// the gate before draining, then waits for the active set to empty before Store
	// closes, including after forcing timed-out connections closed.
	httpHandlers handlerTracker
	// Reaper is the health and reclamation autonomous control loop (auto-pause / crash-pause / evict +
	// periodic reconcile). Built here; started by StartLoops AFTER startup Reconcile.
	Reaper *lifecycle.Reaper
	// loops is the lifetime of the three background loops (loops.go). The assembly owns it
	// because stopping them is a MIDDLE step of Shutdown's sequence, and a caller holding
	// it would have to re-implement that order at the call site.
	loops loopSet
}

type handlerTracker struct {
	mu       sync.Mutex
	draining bool
	active   int
	drained  chan struct{}
}

func (t *handlerTracker) enter() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.draining {
		return false
	}
	t.active++
	return true
}

func (t *handlerTracker) leave() {
	t.mu.Lock()
	t.active--
	if t.draining && t.active == 0 {
		close(t.drained)
	}
	t.mu.Unlock()
}

func (t *handlerTracker) beginDrain() {
	t.mu.Lock()
	t.draining = true
	t.drained = make(chan struct{})
	if t.active == 0 {
		close(t.drained)
	}
	t.mu.Unlock()
}

func (t *handlerTracker) wait(ctx context.Context) error {
	t.mu.Lock()
	drained := t.drained
	t.mu.Unlock()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	// intermittently, since skuid misses packets. Here rather than at boot's
	// InstallSecondHopTable because this is the LAST point at which nothing has been
	// installed and no request has been served.
	//
	// In the assembly rather than in cmd/ so every path reaches it — the substrate harness
	// builds an Engine directly and would otherwise bypass a flag-level check entirely.
	if err := network.ValidateSecondHopUID(dep, os.Geteuid()); err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}

	// the deployment boundary's enforcement half, in the same fail-fast position and for the same reason
	// as the check above.
	if cfg.ListenAddr != "" && !cfg.InsecureListen {
		if err := ValidateListenAddr(cfg.ListenAddr, cfg.ResolveHost); err != nil {
			return nil, err
		}
	}

	// Assemble and validate the configured Volume provider before constructing
	// the remaining substrate providers. An unknown selection is a startup
	// error, never an implicit fallback to local_btrfs.
	volumeCfg := volume.ProviderConfig{
		Provider:   cfg.VolumeProvider,
		LocalBtrfs: cfg.VolumeLocalBtrfs,
		PlainDir:   cfg.VolumePlainDir,
	}
	if volumeCfg.LocalBtrfs.VolumesDir == "" {
		volumeCfg.LocalBtrfs.VolumesDir = cfg.VolumesDir
		if volumeCfg.LocalBtrfs.VolumesDir == "" {
			volumeCfg.LocalBtrfs.VolumesDir = cfg.StateDir + "/volumes"
		}
	}
	if volumeCfg.PlainDir.Dir == "" {
		volumeCfg.PlainDir.Dir = cfg.VolumesDir
		if volumeCfg.PlainDir.Dir == "" {
			volumeCfg.PlainDir.Dir = cfg.StateDir + "/volumes"
		}
	}
	registry, err := volume.Assemble(volumeCfg)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: %w", err)
	}
	if err := registry.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("orchestrator: validate volume providers: %w", err)
	}
	if err := registry.Seal(); err != nil {
		return nil, fmt.Errorf("orchestrator: seal volume providers: %w", err)
	}
	defaultBackend, err := registry.Default()
	if err != nil {
		return nil, fmt.Errorf("orchestrator: select volume provider: %w", err)
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

	// persistent store (the SQLite store choice) — register-an-implementation swap from Memory,
	// no caller change (the extension boundary). This durability is what CREATES the atomic transition rollback
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
		Volume:                defaultBackend,
		VolumeRegistry:        registry,
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
	if err := med.ValidateVolumeProviders(context.Background()); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("orchestrator: validate durable volume providers: %w", err)
	}

	// outbox publisher → a log sink (the transactional outbox: the engine only guarantees reliable
	// write to a replayable stream; a real subscriber lives outside the engine).
	// Default retry policy: bounded attempts, capped + jittered backoff (see
	// store.PublisherConfig).
	pub := store.NewPublisher(st, store.NewDedupSink(func(ev store.Event) {
		log.Printf("event: %s sandbox=%s state=%s seq=%d id=%s", ev.Kind, ev.Sandbox, ev.State, ev.Seq, ev.ID)
	}))

	reaper := lifecycle.NewReaper(med, cfg.SweepInterval, cfg.TransportDownBudget)

	eng := &Engine{Mediator: med, Store: st, VolumeRegistry: registry, Publisher: pub, Reaper: reaper}
	eng.health.startedAt = time.Now()
	return eng, nil
}

// SeedTemplates runs the boot seed (health and reclamation + atomic transition rollback: ensure a builtin `templates` row per
// static Template directory). Call once at boot BEFORE serving any create —
// the sandboxes.template FK (health and reclamation) needs the builtin rows present.
func (e *Engine) SeedTemplates(ctx context.Context) error {
	return e.Mediator.SeedBuiltinTemplates(ctx)
}

// ValidateVolumeProviders rechecks configured providers and durable Volume
// references. New performs this gate before returning; the method is useful to
// callers that reopen or replace a durable Store before serving.
func (e *Engine) ValidateVolumeProviders(ctx context.Context) error {
	if e == nil || e.Mediator == nil {
		return fmt.Errorf("orchestrator: no lifecycle mediator")
	}
	return e.Mediator.ValidateVolumeProviders(ctx)
}

// Reconcile runs the atomic transition rollback startup crash-reconciliation (orphan reclaim +
// running re-adoption). Call once at boot, before serving the API.
func (e *Engine) Reconcile(ctx context.Context) error { return e.Mediator.Reconcile(ctx) }

// InstallSecondHopTable installs the ENGINE-LEVEL table pinning the second hop
// (the engine-level second-hop floor). Call once at boot, BEFORE serving.
//
// Before serving is the requirement, and it mirrors why the rules are installed
// before the isolated unit runs: a `via[]` declaration accepted while that table is
// absent leaves an unpinned confused deputy for as long as the gap lasts. Idempotent
// — it re-renders and re-installs whole, and with no uid supplied it removes any
// stale table a previous run left under a uid this one does not have.
func (e *Engine) InstallSecondHopTable(ctx context.Context) error {
	return e.Mediator.InstallSecondHopTable(ctx)
}

// SweepOrphanOverlays runs the atomic transition rollback orphan-UNIT sweep (reclaim Overlay storage
// units backing no sandbox row). At boot this is where the accumulated residue of
// every prior run is collected: the in-flight keep-set is empty, so every unit
// without a record is a true orphan. Call after Reconcile, which deletes crashed
// `creating` records and so decides which units are orphans.
func (e *Engine) SweepOrphanOverlays(ctx context.Context) error {
	return e.Mediator.SweepOrphanOverlays(ctx)
}

// SweepOrphanLayers runs the Rootfs subsystem / atomic transition rollback orphan-layer sweep (reclaim Promote-built layer
// bytes with no live templates row). Call once at boot after SeedTemplates so a
// crashed Promote's staging layer is reclaimed; the reaper repeats it per tick.
func (e *Engine) SweepOrphanLayers(ctx context.Context) error {
	return e.Mediator.SweepOrphanLayers(ctx)
}

// RepairVolumeCaps re-asserts every volume's RECORDED cap onto its storage (the Volume subsystem /
// volume cap enforcement). Call at boot, in the same position as the orphan sweep.
//
// It exists because writing a snapshot's cap is necessarily the SECOND substrate call, so
// a crash in between leaves a registered, live, UNCAPPED volume — and that is invisible
// afterwards on its own, since the limit read cannot distinguish "never capped" from
// "deliberately uncapped". The direction is always RECORD → STORAGE: adopting the
// storage's value into the record would silently ratify exactly that crash residue.
func (e *Engine) RepairVolumeCaps(ctx context.Context) error {
	return e.Mediator.RepairVolumeCaps(ctx)
}

// ReclaimQuotaRows runs the Volume subsystem / Volume quota cleanup leaked-quota-row reclamation round. The
// `parent` FK demotes this from the main path to a backstop (it makes the leak-producing
// deletion order unreachable through the API), but the residual paths — a crash between
// two deletes, and the sweep's own per-unit ordering — do not pass through user intent,
// so the round must exist and must be correct.
func (e *Engine) ReclaimQuotaRows(ctx context.Context) error {
	return e.Mediator.ReclaimQuotaRows(ctx)
}

// SweepOrphanVolumes runs the Volume subsystem / atomic transition rollback orphan-volume sweep (reclaim volume
// storage backing no `volumes` row).
//
// ITS POSITION IN THE BOOT SEQUENCE IS LOAD-BEARING, not housekeeping: it must run
// BEFORE any storage unit is created. A leaked quota row does not reserve its own
// unit id, so a restart can hand that id — ALONG WITH ITS CAP — to a brand-new
// volume, which then meets ENOSPC on a limit nobody set for it (or is silently
// over-provisioned), and the engine cannot tell the difference from the inside.
// Leaving it to the periodic tick would leave a window in which exactly that
// inheritance happens (the Volume subsystem / leaked quota-row reclamation).
func (e *Engine) SweepOrphanVolumes(ctx context.Context) error {
	return e.Mediator.SweepOrphanVolumes(ctx)
}
