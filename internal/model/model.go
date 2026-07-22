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
// settled §5.A contract 3-tuple — "settled, not yet built".)
type Sandbox struct {
	ID       SandboxID
	Template TemplateID
	Overlay  OverlayID // the writable delta this sandbox owns (§3.1.1)
	State    Lifecycle

	// No guest-identity field: the sandbox runs as guest root by default and the
	// engine does not model guest users (§2.2 — guest identity is not a
	// host-isolation pillar). "Run as user X" is a guest-native concern (su/sudo),
	// not engine state (§3.2.1).

	// IdleTimeout and Retention are the lifecycle TTL — per-sandbox Binding
	// fields (§3.4: SoT is the control-plane record, stable across resume,
	// materialized each placement), consumed by the reaper (§4.3):
	//   - IdleTimeout: a `running` sandbox idle (no I/O) longer than this →
	//     auto-pause (§4.1 automatic transition).
	//   - Retention: a `paused` sandbox older than this → evict (reaper's
	//     destroy of a paused sandbox, §4.1).
	// These EXIST here (unlike the still-omitted volume/network Binding) because
	// the reaper is a live consumer — a justified model growth, not a
	// speculative reservation (§1.3). A create that omits them gets the
	// engine-wide default resolved at create time, so the stored value is always
	// the concrete duration the reaper reads (default site: lifecycle.Deps).
	IdleTimeout time.Duration
	Retention   time.Duration

	CreatedAt time.Time
	UpdatedAt time.Time
}
