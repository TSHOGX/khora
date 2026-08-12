// Package runtime is subsystem §5.A (execution substrate): runsc process lifecycle and
// placement. Its core seam is the runtime Provider — "where and with what to run an isolated
// process context" (§5.A).
//
// The contract is POLICY + HANDLES, never host primitives (§5.A ❌ list): no cgroup path, no
// netns fd, no absolute host path — a unit scheduled onto a remote node cannot produce those.
// One implementation today, local_runsc (§1.1).
package runtime

import (
	"context"

	"warren/internal/ruleset"
)

// RootfsSource is the handle to an already-merged rootfs produced by §5.B: "run this rootfs",
// with no "upper path" and no "lower is a Template" (rootfs-overlay D4).
//
// The handle shape is provider-specific. MergedPath stays seam-clean because it is populated
// and consumed only by local_runsc and never enters a cross-provider decision; a remote
// provider defines its own shape (open-decisions §5.A).
type RootfsSource struct {
	// MergedPath is the merged overlay directory to hand to runsc as its root.
	MergedPath string
}

// SteeringSpec is §5.D's "reachability requirement": what the provider must enforce so the
// sandbox's reachable set is exactly the declared one. It carries the DERIVED result, never
// the declaration (§5.A / §2.1 ③).
//
// Two obligations ride on this field and show up nowhere in its shape (§2.4):
//   - Installation is ATOMIC with placement: after the netns/veth exist, before the isolated
//     unit runs — any window where the network is up and the rules are not is fail-open. Adopt
//     and ReclaimOrphans inherit it; a rule set is crash-surviving host state, like the netns.
//   - The predicate binds to the host-side veth's input interface, NOT the source address,
//     which guest root forges with `ip addr add` (measured; network.md D5 ③) — pillar ④.
//     Spelled `iifname`, by NAME: `iif` freezes an ifindex at load time and silently fails
//     open after a same-name veth rebuild (D5 ⑫), and the two render identically in
//     `nft list`, so §5.A makes it a guard.
//
// The payload is placement-free and abstract rather than nft text because three values the
// rules must name are placement products only the provider holds (network.md D1 ② / D6). The
// vocabulary lives in `internal/ruleset`, a LEAF: either subsystem owning it is the
// `network`↔`runtime` edge internal/arch fails on.
//
// (Guest identity is not a member: the sandbox runs as guest root, §2.2 / §3.2.1.)
type SteeringSpec struct {
	// Rules is the abstract, placement-free rule set (§5.D's pure function output).
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
	// per-interface forwarding, all measured unnecessary (network.md D5 ⑤).
	//
	// A derived FACT, not a knob: `via[]` alone does not need it, a steered flow being DNAT'd
	// to a host-local endpoint and delivered locally.
	RoutedEgress bool

	// SecondHopUID identifies the host-local process behind `via[].endpoint` when the
	// DEPLOYMENT supplied it — never a declaration field, the engine knowing only which local
	// uid's egress to pin under the same floor (§2.4 / §6.2).
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

// ResourceLimits is the Binding the provider materializes at placement time (§3.4 / §5.A):
// CPU rate + RAM → cgroup for local_runsc. PURE POLICY, numeric intent only — no cgroup path
// here or anywhere in the interface (§5.A ❌ list). Zero = unlimited.
//
// Disk CAPACITY is deliberately elsewhere, an FS-layer quota owned by §5.B/§5.C (§3.4):
// cgroup cannot cap at-rest bytes and a quota cannot cap a rate.
//
// RAM is a SAFETY limit (§3.4): exceeding it terminates the sandbox rather than failing one
// operation, the substrate offering no "guest process gets ENOMEM while the sandbox survives"
// path. How a provider splits N into guest-visible RAM vs a host ceiling is its own detail.
//
// The disk-IO RATE cap is a settled DEFERRAL (§3.4.1), the consumer being what is missing.
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
// (§4.3 / §2.4). It crosses the §5.A seam as an enum and NEVER as an errno or a cgroup file
// path, so the §4 mediator dispatches on these three values without learning that memcg
// exists.
//
// Separate type from model.DeathCause on purpose — this is the §5.A wire form, that one the
// persisted form — so the provider seam does not depend on the control-plane record shape.
type DeathCause int

const (
	// CauseUnknown = the unit is gone but WHY is undeterminable. A LEGAL verdict, not a
	// failure: a provider MUST report it rather than guess (§2.4 "does not guess"). §4.3
	// treats it as dead-recoverable, self-correcting structurally when a re-triggered
	// termination after the caller's resume records the real cause.
	CauseUnknown DeathCause = iota
	// CauseGone = dead for a non-resource reason (sentry crash, external kill, clean exit).
	// Disposition: dead-recoverable → resume (§4.3).
	CauseGone
	// CauseResourceExceeded = terminated for exceeding a resource cap (today the RAM safety
	// limit, whole-group via oom.group=1). Disposition: resource-terminated → do NOT resume
	// (§4.3), since the same workload re-triggers the same termination.
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
	// Mounts are the §5.C Volume mount points to bind into the guest, PEER to Rootfs rather
	// than nested inside it: the rootfs is ONE assembled composition, these are N
	// self-sufficient host mount points from a different subsystem (§5 preamble). Each is an
	// opaque source to bind — a local storage unit and a remote backend's FUSE mount point
	// are indistinguishable here.
	Mounts []MountSource
	// No guest-identity field: the sandbox runs as guest root and the engine does not model
	// guest users (§2.2). "Run as user X" is guest-native (su/sudo), not an engine
	// primitive (§3.2.1).
}

// MountSource is one Volume mount to bind into the guest (§5.C): the host mount point and
// where it goes, deliberately NOT the volume's ID or its access mode. Identity is the
// mediator's concern, and the mode's other half (exclusivity) is a control-plane fact already
// decided before this call.
type MountSource struct {
	// SourcePath is the host mount point, produced by the §5.C backend and opaque here — the
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
	// (volume.md D1).
	ReadOnly bool
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

	// PublishEndpoints are the host `ip:port` each published guest port was bound to at this
	// placement, in SteeringSpec.PublishPorts order — §6.1's `host_endpoint`.
	//
	// A placement product (§3.3), rebound every resume: hence §6.1 omits the key entirely
	// with no live placement, a falsy-but-present field inviting callers to read it as "is
	// this publish in effect". Not a ❌-list host primitive — it is the address the engine
	// DELIVERS as the whole point of publish (§5.D), so a remote provider produces one too.
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

// Provider is the §5.A core seam. Verbs belong to §4 (the lifecycle mediator);
// this interface only provides mechanism (top of §4: verbs belong to §4, mechanisms
// to §5).
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

	// Adopt re-recognizes a SURVIVING sandbox's placement after a control-plane crash
	// (§4.4-A's other half / startup reconciliation). Given the SandboxID it rebuilds the
	// opaque placement handle from the sandbox's own labeled resources, probes L1 liveness,
	// and rediscovers the transport endpoint — the SAME endpoint, NOT a new epoch, since the
	// isolation unit never restarted (§3.3). The return is a THREE-way outcome:
	//   - (placement, true, _, nil)  → L1-alive and endpoint recovered; caller runs
	//     its L2 Health probe, then adopts. The cause is meaningless here.
	//   - (nil, false, cause, nil)   → isolation unit genuinely DEAD/absent; caller
	//     CAS-demotes the record to paused (§4.2 involuntary pause) AND persists
	//     cause in that same commit (§4.3).
	//   - (nil, false, _, err)       → UNCERTAIN: L1-alive but the endpoint could not
	//     be recovered. This is NOT death — the caller must PROTECT the placement
	//     (not reclaim it) and retry next boot (§4.4-A protect-on-uncertainty:
	//     tearing down a maybe-live container is irreversible). A provider MUST NOT
	//     collapse this case into alive=false.
	//
	// The DEATH CAUSE rides the alive=false return because the signal is placement-local and
	// VOLATILE (§4.3 / reaper-health D4c): for local_runsc it is the memcg leaf's
	// oom_group_kill, destroyed by the `runsc delete` inside teardown, so "unit dead,
	// placement still intact" is the LAST moment it is readable — a provider that tears down
	// first has destroyed the evidence. Undeterminable → CauseUnknown, never a guess (§2.4).
	//
	// Addressing by SandboxID is why placement-local resources carry a SandboxID label: the
	// §3.3 reverse, allowed by §4.4-A.
	Adopt(ctx context.Context, sandboxID string) (p *Placement, alive bool, cause DeathCause, err error)

	// ReclaimOrphans tears down every engine placement whose SandboxID is NOT in keep (§4.4-A:
	// an orphan is a placement backing no sandbox the store says should hold one). The
	// provider enumerates only its OWN host resources; the mediator computes keep from the
	// authoritative store and passes it in, never the reverse (§4 subsystems don't call each
	// other). Persistent state (Overlay upper) is never touched — not a placement resource.
	//
	// THE KEEP SET ARRIVES COMPLETE, load-bearing rather than a calling convention
	// (network.md D5 ㉒): reclaiming a live sandbox's rule set and re-installing it during
	// Adopt is the delete-then-install sequence measured to kill every live flow across the
	// gap. This shape makes that sequence inexpressible.
	ReclaimOrphans(ctx context.Context, keep []string) error

	// ReinstallRuleSet re-installs a SURVIVING sandbox's §5.D rule set after a control-plane
	// restart — the second half of Adopt's obligation (§2.4: a rule set is crash-surviving
	// host state exactly like the netns).
	//
	// It MUST install as ONE atomic operation and MUST NOT flush connection tracking, both
	// measured (network.md D5 ㉒): one atomic file leaves live flows intact 5/5, a
	// delete-then-install kills them 5/5, and a conntrack flush kills them outright wherever
	// `tcp_loose=0`. The same measurement permits re-rendering blindly, with no diff against
	// what is already installed.
	ReinstallRuleSet(ctx context.Context, p *Placement, spec SteeringSpec) error

	// RuleHits reports how many packets each of this placement's §5.D rules has acted on,
	// keyed by the ABSTRACT counter name §5.D put on the rule (ruleset.Rule.Counter). It backs
	// §6.1's per-entry `hit_packet`. A member named after nft would not be seam-clean: that is
	// one renderer's mechanism.
	//
	// TAKES A PLACEMENT, not a SandboxID: counters are placement-local, so a caller must
	// already hold one to be asking, and no placement is the answer rather than an error. A
	// MISSING KEY means that counter is absent from what is installed and MUST NOT be reported
	// as zero — a bypassed rule's only symptom is a counter reading 0, so manufacturing zeros
	// destroys the signal (§2.4).
	//
	// Read-only and best-effort: it must not install or re-render, and a failed read is not a
	// placement failure.
	RuleHits(ctx context.Context, p *Placement) (map[string]uint64, error)

	// InstallSecondHop installs the ENGINE-LEVEL rule set pinning the second hop (§2.4
	// invariant 2's second half): the egress of the host-local process behind `via[].endpoint`,
	// narrowed by the deployment-supplied uid.
	//
	// UID ONLY: `skuid` is the one narrowing predicate available on the `output` hook, and a
	// gid clause could only be ANDed onto the floor drop, narrowing the floor further (D5 ㉔).
	//
	// Called ONCE at startup, not per placement: one uid covers every `via[]` in the
	// deployment, and hop 2's traffic arrives on no interface (measured: hop 2 counts ZERO in
	// `forward` in all three shapes, visible only in `output` — network.md D5 ㉔).
	//
	// A nil uid means the deployment supplied none: install nothing. A `via[]` declaration is
	// refused at the write point instead, an unenforceable invariant not being half-honored.
	InstallSecondHop(ctx context.Context, rs ruleset.RuleSet, uid *int) error
}
