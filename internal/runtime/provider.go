// Package runtime is subsystem the runtime provider contract (execution substrate): runsc process lifecycle and
// placement. Its core seam is the runtime Provider — "where and with what to run an isolated
// process context" (the runtime provider contract).
//
// The contract is POLICY + HANDLES, never host primitives (the runtime provider contract list): no cgroup path, no
// netns fd, no absolute host path — a unit scheduled onto a remote node cannot produce those.
// One implementation today, local_runsc (the engine vocabulary boundary).
package runtime

import (
	"context"
	"net/netip"
	"time"

	"github.com/TSHOGX/khora/internal/ruleset"
)

// RootfsSource is the handle to an already-merged rootfs produced by the Rootfs subsystem: "run this rootfs",
// with no "upper path" and no "lower is a Template" (rootfs seam ownership).
//
// The handle shape is provider-specific. MergedPath stays seam-clean because it is populated
// and consumed only by local_runsc and never enters a cross-provider decision. Another
// provider may define a different private handle without changing the portable policy.
type RootfsSource struct {
	// MergedPath is the merged overlay directory to hand to runsc as its root.
	MergedPath string
}

// SteeringSpec is the network declaration's "reachability requirement": what the provider must enforce so the
// sandbox's reachable set is exactly the declared one. It carries the DERIVED result, never
// the declaration (the runtime provider contract / the isolation model).
//
// Two obligations ride on this field and show up nowhere in its shape (the provider security contract):
//   - Installation is ATOMIC with placement: after the netns/veth exist, before the isolated
//     unit runs — any window where the network is up and the rules are not is fail-open. Adopt
//     and ReclaimOrphans inherit it; a rule set is crash-surviving host state, like the netns.
//   - The predicate binds to the host-side veth's input interface, NOT the source address,
//     which guest root forges with `ip addr add` (measured by the network substrate suite).
//     Spelled `iifname`, by NAME: `iif` freezes an ifindex at load time and silently fails
//     open after a same-name veth rebuild. The two forms render identically in
//     `nft list`, so the runtime provider contract makes it a guard.
//
// The payload is placement-free and abstract rather than nft text because three values the
// rules must name are placement products only the provider holds. The
// vocabulary lives in `internal/ruleset`, a LEAF: either subsystem owning it is the
// `network`↔`runtime` edge internal/arch fails on.
//
// (Guest identity is not a member: the sandbox runs as guest root, the guest identity model / the product-neutral sandbox boundary.)
type SteeringSpec struct {
	// Rules is the abstract, placement-free rule set (the network declaration's pure function output).
	// Empty = install nothing, which is NOT the same as "deny nothing": a
	// declaration of `deny` + empty still generates the floor and a default-deny.
	// Genuinely empty only where no declaration was produced at all.
	Rules ruleset.RuleSet

	// PublishPorts are the guest ports needing a host endpoint bound, in
	// declaration order. The provider allocates one host endpoint per item at
	// placement time and substitutes it into every rule scoped to it; the
	// endpoints therefore do not appear here (they do not exist yet, and they are
	// placement products either way).
	PublishPorts []PublishPort

	// RoutedEgress says whether this declaration can send anything through the host's router,
	// i.e. whether the three host-side egress mechanisms are needed: ip_forward, a guest
	// default route carrying `dev`, and a masquerade for this /30 — measured necessary AND
	// sufficient, so a provider must not add rp_filter, proxy_arp, accept_local or
	// per-interface forwarding, all measured unnecessary (network substrate measurements).
	//
	// A derived FACT, not a knob: `via[]` alone does not need it, a steered flow being DNAT'd
	// to a host-local endpoint and delivered locally.
	RoutedEgress bool

	// SecondHopUID identifies the host-local process behind `via[].endpoint` when the
	// DEPLOYMENT supplied it — never a declaration field, the engine knowing only which local
	// uid's egress to pin under the same floor (the provider security contract / the API error contract).
	//
	// It rides the spec rather than a separate call because the second-hop table is
	// engine-level: one uid means one rule set regardless of sandbox count, installed once at
	// startup. A pointer because uid 0 is a real uid, and "the operator said root" must behave
	// oppositely from "said nothing" — real, but not legal: it is the engine's own uid, refused
	// at assembly (network.ValidateSecondHopUID).
	SecondHopUID *int
}

// PublishPort is one `ingress.publish[]` item needing a host endpoint.
type PublishPort struct {
	GuestPort uint16
	Proto     ruleset.Proto
}

// ResourceLimits is the Binding the provider materializes at placement time (resource admission / the runtime provider contract):
// CPU rate + RAM → cgroup for local_runsc. PURE POLICY, numeric intent only — no cgroup path
// here or anywhere in the interface (the runtime provider contract list). Zero = unlimited.
//
// Disk CAPACITY is deliberately elsewhere, an FS-layer quota owned by the Rootfs subsystem/the Volume subsystem (resource admission):
// cgroup cannot cap at-rest bytes and a quota cannot cap a rate.
//
// RAM is a SAFETY limit (resource admission): exceeding it terminates the sandbox rather than failing one
// operation, the substrate offering no "guest process gets ENOMEM while the sandbox survives"
// path. How a provider splits N into guest-visible RAM vs a host ceiling is its own detail.
//
// The disk-IO RATE cap is a settled DEFERRAL (the resource-ceiling axes), the consumer being what is missing.
// What joins later is a RATE alone: io.max needs a whole-disk MAJ:MIN, which is the PROVIDER'S
// to derive from the paths it already holds, never a contract member — hence seam_test.go's
// token guard on `major`/`minor`/`majmin`/`devno`. Measured coverage limit for whoever lands
// it: a remote backend's mount point is an anonymous filesystem with st_dev major 0 (measured
// on a live FUSE mount), so io.max cannot address it and that I/O is charged to the backend
// daemon's cgroup instead.
type ResourceLimits struct {
	CPUMillis  int64 // 1000 = one full CPU
	MemoryByte int64
}

// DeathCause is the ABSTRACT enum a provider reports when it finds an isolation unit dead
// (health and reclamation / the provider security contract). It crosses the runtime provider contract seam as an enum and NEVER as an errno or a cgroup file
// path, so the lifecycle mediator dispatches on these three values without learning that memcg
// exists.
//
// Separate type from model.DeathCause on purpose — this is the runtime provider contract wire form, that one the
// persisted form — so the provider seam does not depend on the control-plane record shape.
type DeathCause int

const (
	// CauseUnknown = the unit is gone but WHY is undeterminable. A LEGAL verdict, not a
	// failure: a provider MUST report it rather than guess (the provider security contract "does not guess"). health and reclamation
	// treats it as dead-recoverable, self-correcting structurally when a re-triggered
	// termination after the caller's resume records the real cause.
	CauseUnknown DeathCause = iota
	// CauseGone = dead for a non-resource reason (sentry crash, external kill, clean exit).
	// Disposition: dead-recoverable → resume (health and reclamation).
	CauseGone
	// CauseResourceExceeded = terminated for exceeding a resource cap (today the RAM safety
	// limit, whole-group via oom.group=1). Disposition: resource-terminated → do NOT resume
	// (health and reclamation), since the same workload re-triggers the same termination.
	CauseResourceExceeded
)

func (c DeathCause) String() string {
	switch c {
	case CauseGone:
		return "gone"
	case CauseResourceExceeded:
		return "resource-exceeded"
	default:
		return "unknown"
	}
}

// LaunchSpec is the provider input — the contract's left-hand side:
//
//	(rootfs source, reachability requirement, resource ceilings) → (placement handle,
//	 transport endpoint, stop/start)
type LaunchSpec struct {
	Sandbox  string // opaque sandbox id, for naming/logging only
	Rootfs   RootfsSource
	Steering SteeringSpec
	Limits   ResourceLimits
	// ProcessRetention is the bounded addressability window for exited logical
	// process handles. The provider passes this portable policy to the guest
	// PID-1 supervisor so its replay registry and the control-plane Store share
	// one retention boundary. Zero leaves the guest's documented default in
	// effect for direct provider callers; lifecycle always supplies a positive
	// value after applying its default.
	ProcessRetention time.Duration
	// Mounts are the Volume subsystem Volume mount points to bind into the guest, PEER to Rootfs rather
	// than nested inside it: the rootfs is ONE assembled composition, these are N
	// self-sufficient host mount points from a different subsystem (the subsystem boundaries preamble). Each is an
	// opaque source to bind — a local storage unit and a remote backend's FUSE mount point
	// are indistinguishable here.
	Mounts []MountSource
	// No guest-identity field: the sandbox runs as guest root and the engine does not model
	// guest users (the guest identity model). "Run as user X" is guest-native (su/sudo), not an engine
	// primitive (the product-neutral sandbox boundary).
}

// MountSource is one Volume mount to bind into the guest (the Volume subsystem): the host mount point and
// where it goes, deliberately NOT the volume's ID or its access mode. Identity is the
// mediator's concern, and the mode's other half (exclusivity) is a control-plane fact already
// decided before this call.
type MountSource struct {
	// SourcePath is the host mount point, produced by the Volume subsystem backend and opaque here — the
	// same kind of handle RootfsSource.MergedPath is, exempted in the seam guard alike.
	SourcePath string
	// Dest is the absolute guest path to bind at.
	Dest string
	// ReadOnly makes this a read-only bind, materializing the shared-ro access mode.
	//
	// THE DIRECTION OF THIS BIT IS LOAD-BEARING, as a substrate fact: "ro is enforced at
	// runtime" holds only for a WRITABLE unit given a READ-ONLY mount, where the guest's
	// write gets a clean EROFS. The reverse — an immutable unit given a writable mount spec
	// — is not a runtime error at all: `runsc create` hard-fails in the gofer mount stage,
	// turning what should be a 4xx at admission into an opaque cold-start failure. The
	// engine never produces a read-only unit, so that path is unreachable today
	// (Volume materialization).
	ReadOnly bool
}

// Placement is the opaque handle to this launch's physical resources — rebound
// every resume, old one invalidated (the ID-reference model). The engine addresses the sandbox by
// SandboxID, never by anything in here.
type Placement struct {
	// Epoch is the durable realization number supplied by the lifecycle
	// mediator. Providers treat it as opaque correlation data; stale requests
	// are rejected by the control plane before reaching this seam.
	Epoch uint64
	// Address is the guest-visible address assigned to this placement. It is a
	// portable placement fact, independent of the transport endpoint and rebound
	// on every resume.
	Address netip.Addr

	// TransportEndpoint is where the host dials sandboxd (the transport data plane). For
	// local_runsc it is the current netns veth IP:port, discovered at
	// (the runtime provider contract placement discovery). IP change across resume is itself the epoch
	// signal (health and reclamation placement/IP-as-epoch) — the engine does not add a counter.
	TransportEndpoint string

	// PublishEndpoints are the host `ip:port` each published guest port was bound to at this
	// placement, in SteeringSpec.PublishPorts order — the control-plane API's `host_endpoint`.
	//
	// A placement product (the ID-reference model), rebound every resume: hence the control-plane API omits the key entirely
	// with no live placement, a falsy-but-present field inviting callers to read it as "is
	// this publish in effect". Not a ❌-list host primitive — it is the address the engine
	// DELIVERS as the whole point of publish (the network declaration), so a remote provider produces one too.
	PublishEndpoints []string

	// handle is provider-private state for stop/start (local_runsc: the
	// runsc container id + netns name). Deliberately not exported — callers
	// address by SandboxID, not by placement internals.
	handle any
}

// SetHandle / Handle let the provider stash and retrieve its private placement
// state without exposing its shape to callers.
func (p *Placement) SetHandle(h any) { p.handle = h }
func (p *Placement) Handle() any     { return p.handle }

// Provider is the runtime provider contract core seam. Verbs belong to the lifecycle (the lifecycle mediator);
// this interface only provides mechanism (top of the lifecycle: verbs belong to the lifecycle, mechanisms
// to the subsystem boundaries).
type Provider interface {
	// Name identifies the provider (e.g. "local_runsc").
	Name() string

	// Launch materializes placement and cold-starts the isolated unit with
	// sandboxd (init mode) as entrypoint. It returns the placement handle,
	// including the transport endpoint once the netns IP is discovered.
	Launch(ctx context.Context, spec LaunchSpec) (*Placement, error)

	// Stop cleanly stops the isolated unit, releasing RAM/CPU (FS-first pause and resume pause: the
	// Overlay is already on the backend, so stop is all pause needs to do).
	// The Overlay artifact is untouched.
	Stop(ctx context.Context, p *Placement) error

	// Destroy tears down placement-local resources (netns/IP/cgroup). It does
	// NOT delete the Overlay artifact — that is the Rootfs subsystem's job,
	// invoked separately by the mediator (the lifecycle subsystems don't call each other).
	Destroy(ctx context.Context, p *Placement) error

	// Adopt re-recognizes a SURVIVING sandbox's placement after a control-plane crash
	// (atomic transition rollback's other half / startup reconciliation). Given the SandboxID it rebuilds the
	// opaque placement handle from the sandbox's own labeled resources, probes L1 liveness,
	// and rediscovers the transport endpoint — the SAME endpoint, NOT a new epoch, since the
	// isolation unit never restarted (the ID-reference model). The return is a THREE-way outcome:
	//   - (placement, true, _, nil)  → L1-alive and endpoint recovered; caller runs
	//     its L2 Health probe, then adopts. The cause is meaningless here.
	//   - (nil, false, cause, nil)   → isolation unit genuinely DEAD/absent; caller
	//     CAS-demotes the record to paused (FS-first pause and resume involuntary pause) AND persists
	//     cause in that same commit (health and reclamation).
	//   - (nil, false, _, err)       → UNCERTAIN: L1-alive but the endpoint could not
	//     be recovered. This is NOT death — the caller must PROTECT the placement
	//     (not reclaim it) and retry next boot (atomic transition rollback protect-on-uncertainty:
	//     tearing down a maybe-live container is irreversible). A provider MUST NOT
	//     collapse this case into alive=false.
	//
	// The DEATH CAUSE rides the alive=false return because the signal is placement-local and
	// VOLATILE (health and reclamation / death-cause propagation): "unit dead, placement still intact" is the LAST
	// moment it is readable, so a provider that tears down first has destroyed the evidence.
	// Undeterminable → CauseUnknown, never a guess (the provider security contract).
	//
	// Addressing by SandboxID is why placement-local resources carry a SandboxID label: the
	// the ID-reference model reverse, allowed by atomic transition rollback.
	Adopt(ctx context.Context, sandboxID string) (p *Placement, alive bool, cause DeathCause, err error)

	// ReclaimOrphans tears down every engine placement whose SandboxID is NOT in keep (atomic transition rollback:
	// an orphan is a placement backing no sandbox the store says should hold one). The
	// provider enumerates only its OWN host resources; the mediator computes keep from the
	// authoritative store and passes it in, never the reverse (the lifecycle subsystems don't call each
	// other). Persistent state (Overlay upper) is never touched — not a placement resource.
	//
	// THE KEEP SET ARRIVES COMPLETE, load-bearing rather than a calling convention:
	// reclaiming a live sandbox's rule set and re-installing it during Adopt is the
	// delete-then-install sequence ReinstallRuleSet below is forbidden from using, and this
	// shape makes it inexpressible.
	ReclaimOrphans(ctx context.Context, keep []string) error

	// ReinstallRuleSet re-installs a SURVIVING sandbox's the network declaration rule set after a control-plane
	// restart — the second half of Adopt's obligation (the provider security contract: a rule set is crash-surviving
	// host state exactly like the netns).
	//
	// It MUST install as ONE atomic operation and MUST NOT flush connection tracking, both
	// measured (network substrate measurements): one atomic file leaves live flows intact 5/5, a
	// delete-then-install kills them 5/5, and a conntrack flush kills them outright wherever
	// `tcp_loose=0`. The same measurement permits re-rendering blindly, with no diff against
	// what is already installed.
	ReinstallRuleSet(ctx context.Context, p *Placement, spec SteeringSpec) error

	// RuleHits reports how many packets each of this placement's the network declaration rules has acted on,
	// keyed by the ABSTRACT counter name the network declaration put on the rule (ruleset.Rule.Counter). It backs
	// the control-plane API's per-entry `hit_packet`. A member named after nft would not be seam-clean: that is
	// one renderer's mechanism.
	//
	// TAKES A PLACEMENT, not a SandboxID: counters are placement-local, so a caller must
	// already hold one to be asking, and no placement is the answer rather than an error. A
	// MISSING KEY means that counter is absent from what is installed and MUST NOT be reported
	// as zero — a bypassed rule's only symptom is a counter reading 0, so manufacturing zeros
	// destroys the signal (the provider security contract).
	//
	// Read-only and best-effort: it must not install or re-render, and a failed read is not a
	// placement failure.
	RuleHits(ctx context.Context, p *Placement) (map[string]uint64, error)

	// InstallSecondHop installs the ENGINE-LEVEL rule set that constrains the egress
	// of the host-local process behind `via[].endpoint`,
	// narrowed by the deployment-supplied uid.
	//
	// UID ONLY: `skuid` is the one narrowing predicate available on the `output` hook, and a
	// gid clause could only be ANDed onto the floor drop, narrowing the floor further.
	//
	// Called ONCE at startup, not per placement: one uid covers every `via[]` in the
	// deployment, and hop 2's traffic arrives on no interface (measured: hop 2 counts ZERO in
	// `forward` in all three shapes, visible only in `output` — network substrate measurements).
	//
	// A nil uid means the deployment supplied none: install nothing. A `via[]` declaration is
	// refused at the write point instead, an unenforceable invariant not being half-honored.
	InstallSecondHop(ctx context.Context, rs ruleset.RuleSet, uid *int) error
}

// ManifestChecker is an optional provider capability check. The lifecycle
// mediator supplies only portable platform/capability declarations; provider
// details stay behind this seam.
type ManifestChecker interface {
	CheckManifest(ctx context.Context, platform string, capabilities []string) error
}
