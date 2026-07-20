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
	"fmt"
	"time"

	"engine/internal/lifecycle"
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
}

// Engine is the assembled engine host.
type Engine struct {
	Mediator *lifecycle.Mediator
	Store    store.Store
	cfg      Config
}

// New wires the assembly. It does not start any server; call HTTPHandler for the
// §6.1 API or drive Mediator directly.
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

	st := store.NewMemory()
	med := lifecycle.New(lifecycle.Deps{
		Store:         st,
		Runtime:       rt,
		Rootfs:        rp,
		Dial:          transport.DialFunc,
		HealthTimeout: cfg.HealthTimeout,
	})
	return &Engine{Mediator: med, Store: st, cfg: cfg}, nil
}
