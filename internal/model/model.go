// Package model holds the shared engine model (§3): sandbox identity,
// lifecycle state, and the three-state rootfs model. It is pure types with no
// dependency on any subsystem — every subsystem depends on model, model depends
// on none of them (§5: subsystems are peers over the shared model, never each
// other).
package model

import "time"

// SandboxID is the stable, opaque identity of a persistent sandbox (§3.3).
// It does NOT encode placement: after resume the IP / container / cgroup / node
// may all change, but the SandboxID is unchanged — same Overlay artifact, same
// sandbox (§3.3 铁律).
type SandboxID string

// TemplateID identifies an immutable, content-addressed read-only base (§3.1).
type TemplateID string

// OverlayID identifies the sandbox's writable delta layer as a first-class,
// ID-addressed, Template-decoupled artifact (§3.1.1). The host path where it
// materializes is an FS-provider implementation detail and never leaks above
// the Rootfs subsystem (§3.1.1 对内也 ID 寻址).
type OverlayID string

// Lifecycle is the §4.1 state machine. Exactly four states — no `failed`
// intermediate state (§4.4: the absence is a conclusion of the model, not an
// omission), no independent generation counter (§3.3 uses Placement / §4.3 uses
// placement-IP-as-epoch instead).
type Lifecycle string

const (
	Creating  Lifecycle = "creating"
	Running   Lifecycle = "running"
	Paused    Lifecycle = "paused"
	Destroyed Lifecycle = "destroyed"
)

// Sandbox is the persistent unit (§3.2): the durable record the control plane
// owns. Placement is deliberately NOT a field here — it is rebound every resume
// and lives with the runtime provider, not in the identity record (§3.3).
//
// Minimal shape by one rule: a field exists here only when this record actually
// carries it today. Binding fields not yet consumed (mounted volumes, network
// policy, resource limits) are omitted rather than stubbed — this record is a
// data shape, not a settled interface signature, so an unused field would be a
// speculative reservation (§1.3). (Contrast the runtime provider's
// SteeringSpec/ResourceLimits, which DO exist now: they are named members of the
// settled §5.A contract 4-tuple — "settled, not yet built".)
type Sandbox struct {
	ID       SandboxID
	Template TemplateID
	Overlay  OverlayID // the writable delta this sandbox owns (§3.1.1)
	State    Lifecycle
	// GuestUser is the create-time default run-as identity (§2.2 / §6.2),
	// a configurable guest default — NOT a security invariant (§2.2).
	GuestUser string

	CreatedAt time.Time
	UpdatedAt time.Time
}
