// Package orchestrator is the single engine-host assembly (§1.1): it wires the
// in-memory store + the two providers (local_runsc, host_overlayfs) + the §4
// lifecycle mediator + the transport dialer, and exposes the minimal §6.1
// sandbox-centric API. Deployment target is "single orchestrator binary + one
// config + local store" with no heavy external dependency (§1.1).
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
)

// Config is the orchestrator's local configuration (§1.1 one config).
type Config struct {
	// TemplatesDir holds Template directories (static in M1; TemplateID → dir).
	TemplatesDir string
	// StateDir is the root for overlay materialization + runsc state/bundles.
	StateDir string
	// HealthTimeout bounds the ⑦ readiness gate (guest-runtime D5.1).
	HealthTimeout time.Duration
	// Sudo prefixes runsc/ip with sudo (dev convenience). Production runs the
	// orchestrator as root and leaves this false.
	Sudo bool
	// StorePath is the SQLite store file (ADR-0004). Empty → an in-memory SQLite
	// (":memory:") for tests that don't need durability across a New() call.
	StorePath string

	// FaultAfterColdStart is a TEST-ONLY (R5) injection hook forwarded to the
	// mediator to simulate a crash mid-transition. nil in production (plan §7 R5).
	FaultAfterColdStart func(id model.SandboxID)
}

// Engine is the assembled engine host.
type Engine struct {
	Mediator  *lifecycle.Mediator
	Store     store.Store
	Publisher *store.Publisher
	cfg       Config
}

// New wires the assembly. It does not start any server, and it does NOT run
// startup reconciliation — call Reconcile explicitly after New (so tests can
// inspect the pre-reconcile store), then HTTPHandler to serve.
func New(cfg Config) (*Engine, error) {
	if cfg.TemplatesDir == "" || cfg.StateDir == "" {
		return nil, fmt.Errorf("orchestrator: TemplatesDir and StateDir are required")
	}
	if cfg.HealthTimeout == 0 {
		cfg.HealthTimeout = 30 * time.Second
	}

	rp, err := rootfs.NewHostOverlayFS(cfg.StateDir+"/overlays", cfg.TemplatesDir)
	if err != nil {
		return nil, err
	}
	rt, err := runtime.NewLocalRunsc(cfg.StateDir+"/runsc-root", cfg.StateDir+"/runsc-state")
	if err != nil {
		return nil, err
	}
	rt.Sudo = cfg.Sudo

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
		Store:               st,
		Runtime:             rt,
		Rootfs:              rp,
		Dial:                transport.DialFunc,
		HealthTimeout:       cfg.HealthTimeout,
		FaultAfterColdStart: cfg.FaultAfterColdStart,
	})

	// outbox publisher → a log sink (§4.4-C: the engine only guarantees reliable
	// write to a replayable stream; a real subscriber lives outside the engine).
	pub := store.NewPublisher(st, store.NewDedupSink(func(ev store.Event) {
		log.Printf("event: %s sandbox=%s state=%s id=%s", ev.Kind, ev.Sandbox, ev.State, ev.ID)
	}))

	return &Engine{Mediator: med, Store: st, Publisher: pub, cfg: cfg}, nil
}

// Reconcile runs the §4.4-A startup crash-reconciliation (orphan reclaim +
// running re-adoption). Call once at boot, before serving the API.
func (e *Engine) Reconcile(ctx context.Context) error { return e.Mediator.Reconcile(ctx) }
