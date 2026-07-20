// Package runtime is subsystem §5.A (execution substrate): runsc process
// lifecycle and placement. Its core seam is the runtime Provider — "where and
// with what to run an isolated process context" (§5.A).
//
// The contract is deliberately expressed in POLICY + HANDLES, never host
// primitives (§5.A ❌ list). A provider must NOT be handed "a cgroup path / a
// netns fd / the Overlay's absolute host path" — a unit scheduled onto a remote
// node cannot produce those, and leaking them binds the interface to a
// host-local assumption that no non-local provider could ever satisfy. M1 ships
// exactly one implementation, local_runsc; the interface exists to keep the
// abstraction boundary clean, not to support other backends today (§1.1).
package runtime

import "context"

// RootfsSource is an OPAQUE handle to an already-merged rootfs, produced by the
// Rootfs subsystem (§5.B). The provider receives a merged rootfs to run and
// does NOT know how it was assembled (§5.B / rootfs-overlay D4): the provider
// does not see "upper path" or "lower is a Template" — only "run this rootfs".
type RootfsSource struct {
	// MergedPath is the merged overlay directory to hand to runsc as its root.
	// NOTE (seam discipline): this is a host path and is therefore only ever
	// populated/consumed by the local_runsc provider. It stays out of any
	// cross-provider decision. A remote provider would carry a different handle
	// shape here. Kept concrete in M1 because local_runsc is the only impl.
	MergedPath string
}

// SteeringSpec is the §5.D "引流要求": the ingress descriptor + default-deny
// that forces all egress into the engine's data plane. It is NOT the full
// network policy — policy enforcement and credential injection live in the
// engine's own egress data plane, not in the provider (§5.A / §2.1 ③④).
//
// M1 is network=none, so this is empty. The type exists now to keep the ④-step
// contract shape correct for M5.
type SteeringSpec struct{}

// ResourceLimits is the rate-axis Binding the provider materializes at
// placement time (§3.4 / §5.A): CPU / RAM / disk-IO → cgroup for local_runsc.
// Disk CAPACITY is NOT here — that is an FS-layer quota owned by §5.B/§5.C
// (§3.4 两轴切分). M1 leaves these zero (unlimited).
type ResourceLimits struct {
	CPUMillis  int64
	MemoryByte int64
	// disk-IO (io controller) added when M1 grows resource limits; capacity
	// axis intentionally absent — it belongs to the FS provider.
}

// LaunchSpec is the provider input — the contract's left-hand side:
//
//	(rootfs 来源, 引流要求, 资源上限, guest 身份) → (placement 句柄, transport 端点, stop/start)
type LaunchSpec struct {
	Sandbox   string // opaque sandbox id, for naming/logging only
	Rootfs    RootfsSource
	Steering  SteeringSpec
	Limits    ResourceLimits
	GuestUser string // create-time default run-as (§2.2 / §6.2)
}

// Placement is the opaque handle to this launch's physical resources — rebound
// every resume, old one invalidated (§3.3). The engine addresses the sandbox by
// SandboxID, never by anything in here.
type Placement struct {
	// TransportEndpoint is where the host dials sandboxd (§5.E). For
	// local_runsc it is the current netns veth IP:port, discovered at ⑥
	// (§5.A placement discovery). IP change across resume is itself the epoch
	// signal (§4.3 placement/IP-as-epoch) — the engine does not add a counter.
	TransportEndpoint string

	// handle is provider-private state for stop/start (M1 local_runsc: the
	// runsc container id + netns name). Deliberately not exported — callers
	// address by SandboxID, not by placement internals.
	handle any
}

// SetHandle / Handle let the provider stash and retrieve its private placement
// state without exposing its shape to callers.
func (p *Placement) SetHandle(h any) { p.handle = h }
func (p *Placement) Handle() any     { return p.handle }

// Provider is the §5.A core seam. Verbs belong to §4 (the lifecycle mediator);
// this interface only provides mechanism (§4 开头: 动词归 §4,机制归 §5).
type Provider interface {
	// Name identifies the provider (e.g. "local_runsc").
	Name() string

	// Launch materializes placement and cold-starts the isolated unit with
	// sandboxd (init mode) as entrypoint — the ④ step of the cold-start chain
	// (guest-runtime D5.1). Returns the placement handle including the
	// transport endpoint once the netns IP is discovered (⑥).
	Launch(ctx context.Context, spec LaunchSpec) (*Placement, error)

	// Stop cleanly stops the isolated unit, releasing RAM/CPU (§4.2 pause: the
	// Overlay is already on the backend, so stop is all pause needs to do).
	// The Overlay artifact is untouched.
	Stop(ctx context.Context, p *Placement) error

	// Destroy tears down placement-local resources (netns/IP/cgroup). It does
	// NOT delete the Overlay artifact — that is the Rootfs subsystem's job,
	// invoked separately by the mediator (§4 subsystems don't call each other).
	Destroy(ctx context.Context, p *Placement) error

	// Adopt re-recognizes a SURVIVING sandbox's placement after a control-plane
	// crash (§4.4-A 另一半 / startup reconciliation). Given the SandboxID it
	// rebuilds the opaque placement handle from the sandbox's own labeled
	// resources, probes L1 liveness, and rediscovers the transport endpoint — the
	// SAME endpoint, NOT a new epoch, because the isolation unit never restarted
	// (§3.3). The return is a THREE-way outcome, not a boolean:
	//   - (placement, true, nil)  → L1-alive and endpoint recovered; caller runs
	//     its L2 Health probe, then adopts.
	//   - (nil, false, nil)       → isolation unit genuinely DEAD/absent; caller
	//     CAS-demotes the record to paused (§4.2 involuntary pause).
	//   - (nil, false, err)       → UNCERTAIN: L1-alive but the endpoint could not
	//     be recovered. This is NOT death — the caller must PROTECT the placement
	//     (not reclaim it) and retry next boot (§4.4-A protect-on-uncertainty:
	//     tearing down a maybe-live container is irreversible). A provider MUST NOT
	//     collapse this case into alive=false.
	// Left-hand side is engine vocabulary (SandboxID); right-hand side is an OPAQUE
	// handle — no host primitive leaks (§5.A ❌ list). This is why placement-local
	// resources must carry a SandboxID label — the §3.3 reverse, allowed (§4.4-A).
	Adopt(ctx context.Context, sandboxID string) (p *Placement, alive bool, err error)

	// ReclaimOrphans tears down every engine placement whose SandboxID is NOT in
	// keep (§4.4-A: an orphan is a placement backing no sandbox the store says
	// should hold one). The provider enumerates only its OWN host resources; the
	// mediator computes keep from the authoritative store and passes it in — the
	// store is read by the mediator and fed to the provider, never the reverse
	// (§4 subsystems don't call each other). Persistent state (Overlay upper) is
	// never touched — it is not a placement resource (§4.4-A 持久态分毫不动).
	ReclaimOrphans(ctx context.Context, keep []string) error
}
