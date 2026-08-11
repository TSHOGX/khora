// Package runtime is subsystem §5.A (execution substrate): runsc process
// lifecycle and placement. Its core seam is the runtime Provider — "where and
// with what to run an isolated process context" (§5.A).
//
// The contract is deliberately expressed in POLICY + HANDLES, never host
// primitives (§5.A ❌ list). A provider must NOT be handed "a cgroup path / a
// netns fd / the Overlay's absolute host path" — a unit scheduled onto a remote
// node cannot produce those, and leaking them binds the interface to a
// host-local assumption that no non-local provider could ever satisfy. There is
// exactly one implementation, local_runsc; the interface exists to keep the
// abstraction boundary clean, not to support other backends today (§1.1).
package runtime

import (
	"context"

	"warren/internal/ruleset"
)

// RootfsSource is the handle to an already-merged rootfs, produced by the Rootfs
// subsystem (§5.B). The provider receives a merged rootfs to run and does NOT
// know how it was assembled (§5.B / rootfs-overlay D4): it does not see "upper
// path" or "lower is a Template" — only "run this rootfs".
//
// The handle shape is provider-specific: local_runsc, the sole impl, needs a
// host path, so that is what the field carries. This is NOT the interface
// leaking a host primitive — MergedPath is populated and consumed only by
// local_runsc and never enters a cross-provider decision. A remote provider
// would define a different handle shape (open-decisions §5.A: RootfsSource
// remote form); the seam stays clean because callers never inspect it.
type RootfsSource struct {
	// MergedPath is the merged overlay directory to hand to runsc as its root.
	MergedPath string
}

// SteeringSpec is §5.D's "reachability requirement": what the provider must enforce so that
// the sandbox's reachable set is exactly the declared one — the allow set and
// publish endpoints derived from the declaration, plus the L0 floor and
// default-deny. It carries the DERIVED result, not the declaration itself: §5.D
// holds the declaration and generates rules, the provider installs them and
// holds no authority (§5.A / §2.1 ③).
//
// Two obligations ride on this field and are easy to miss because neither shows
// up in its shape (§2.4):
//   - Installation must be ATOMIC with placement — rules go on after the
//     netns/veth exist and before the isolated unit runs. Any window where the
//     network is up but the rules are not is a fail-open window, and only the
//     side creating the placement has that hook point. Adopt/ReclaimOrphans
//     inherit this: a rule set is crash-surviving host state, like the netns.
//   - The predicate must bind to a fact the guest cannot change — the host-side
//     veth's input interface, NOT the source address, which guest root can
//     forge with `ip addr add` (measured; network.md D5 ③). That is pillar ④.
//     Spell it `iifname` (by NAME): `iif` freezes an ifindex at load time and
//     silently fails open after a same-name veth rebuild (network.md D5 ⑫), and
//     the two render identically in `nft list` — §5.A makes this a guard, not a
//     review item.
//
// # Why the payload is an ABSTRACT rule set rather than nft text
//
// The split is fixed by network.md D1 ② / D6: three things the rules must
// eventually name are placement products only the provider holds — the host-side
// veth name, the guest IP, and the publish host endpoint. Rendering here, above
// the seam, would both fail to run (no placement exists when the rules are
// generated) and lift a host primitive onto the cross-provider contract (the ❌
// list above). So §5.D emits placement-FREE rules carrying Scope/Match flags
// where a placement product belongs, and the provider substitutes the concrete
// value at install time.
//
// The payoff is testability: every difference between declaration values is
// pinned in root-free unit tests, and the substrate suite only has to prove
// "installed, in the right order, and reconciled after a crash".
//
// The rule vocabulary lives in `internal/ruleset`, a LEAF both §5.D and §5.A
// depend on — not in either subsystem, since `network` importing `runtime` (or the
// reverse) is exactly the edge internal/arch fails on (CLAUDE.md: shared fact →
// leaf, never an exemption).
//
// (Guest identity is NOT a member: the sandbox runs as guest root and the engine
// does not model guest users, §2.2 / §3.2.1.)
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

	// RoutedEgress tells the provider whether this declaration can send anything
	// through the host's router, i.e. whether the three host-side egress mechanisms
	// must be put in place (network.md D5 ⑤: ip_forward, a guest default route
	// carrying `dev`, and a masquerade for this /30 — measured necessary AND
	// sufficient; rp_filter / proxy_arp / accept_local / per-interface forwarding
	// are all measured unnecessary, so a provider must not add them).
	//
	// A derived FACT rather than a knob: `via[]` alone does not need it, because a
	// steered flow is DNAT'd to a host-local endpoint and delivered locally.
	RoutedEgress bool

	// SecondHopUID identifies the host-local process behind `via[].endpoint`, when the
	// DEPLOYMENT supplied it (§2.4 / §6.2: a deployment input, never a declaration
	// field — the engine still does not know what sits behind that endpoint, only
	// which local uid's egress must be pinned under the same floor).
	//
	// It rides the spec rather than a separate call because the second-hop table is
	// engine-level: one uid means one rule set regardless of sandbox count, so the
	// provider installs it once at startup and not per placement. A pointer because
	// uid 0 is a real uid — a value type could not tell "the operator said root"
	// from "the operator said nothing", and those must behave oppositely. (It being
	// a real uid does not make it a legal one: it is the engine's own, refused at
	// assembly — network.ValidateSecondHopUID.)
	SecondHopUID *int
}

// PublishPort is one `ingress.publish[]` item needing a host endpoint.
type PublishPort struct {
	GuestPort uint16
	Proto     ruleset.Proto
}

// ResourceLimits is the Binding the provider materializes at placement time
// (§3.4 / §5.A): CPU rate + RAM → cgroup for local_runsc. Disk CAPACITY is NOT
// here — that is an FS-layer quota owned by §5.B/§5.C (§3.4), because cgroup
// cannot cap at-rest bytes and a quota cannot cap a rate. Adding a DiskByte
// field here would weld two orthogonal axes onto one mechanism.
//
// This carries PURE POLICY — numeric intent only. No cgroup path appears here or
// anywhere in the Provider interface (§5.A ❌ list): local_runsc synthesizes its
// own cgroup path internally from the cid, the same way it synthesizes netns and
// container names. A remote provider cannot produce a cgroup path at all, and
// accepting one would bind this contract to a host-local assumption.
//
// RAM is a SAFETY limit, not a resource limit (§3.4): exceeding it terminates
// the sandbox (a lifecycle event) rather than failing one operation, because the
// substrate offers no "guest process gets ENOMEM while the sandbox survives"
// path. How the provider splits one MemoryByte into guest-visible RAM vs a host
// ceiling is ITS OWN internal detail — the interface states only N (§5.A):
// local_runsc adds a sentry/gofer overhead constant, while a remote provider's
// overhead may be a VM balloon or a scheduler memory request. Zero = unlimited.
//
// The disk-IO RATE cap (§3.4's other rate-axis half) is deliberately absent, and
// that is a settled deferral, not an open question: the mechanism is proven
// (runsc writes OCI blockIO into its leaf's io.max), the CONSUMER is what's
// missing — so adding the field before one exists is the speculative reservation
// §1.3 rejects. It joins additively when a consumer appears, and what joins is a
// RATE alone: io.max needs a WHOLE-DISK MAJ:MIN, and that number is the
// PROVIDER'S to derive (§5.A) — it is not a contract member, because a
// per-sandbox Binding carries no host topology and a device number is a host
// primitive no remote provider could produce. Hence the token guard in
// seam_test.go bans `major`/`minor`/`majmin`/`devno` outright: this field would
// be the natural place to smuggle one in.
//
// THIS SEAM IS THE RIGHT HOME FOR IT, AND NOT BY DEFAULT (§5.A / §3.4.1). The
// external shapes look like they disagree — a microVM hangs its rate limiter off
// the DRIVE handle, cloud block storage attaches IOPS PER VOLUME — but those
// answer a different question: a per-volume IOPS figure is what a storage unit
// PROMISES, while this axis caps what ONE SANDBOX may consume across its Overlay
// upper AND every volume mounted into it. Recording it per volume would turn one
// ceiling into N that multiply.
//
// io.max being per-device does not force a contract change either: the provider
// already receives every path whose device it must cover (Rootfs.MergedPath and
// each Mounts[].SourcePath), so it stats them, derives each backing whole disk,
// and writes one io.max line per distinct device — asking §5.B and §5.C nothing.
//
// ONE COVERAGE LIMIT TO IMPLEMENT HONESTLY, measured rather than assumed: a
// remote volume backend's mount point is an ANONYMOUS filesystem whose st_dev
// major is 0 (measured on a live FUSE mount), so io.max — keyed on a block-device
// MAJ:MIN — cannot address it, and the I/O is charged to the backend daemon's own
// cgroup regardless. So the cap covers what the guest's cgroup owns; a remote
// backend enforces its own rate or reports the capability absent. Accepting a
// rate it does not apply is the silent-degradation §3.4 refuses.
type ResourceLimits struct {
	CPUMillis  int64 // 1000 = one full CPU
	MemoryByte int64
}

// DeathCause is the ABSTRACT enum a provider reports when it finds an isolation
// unit dead (§4.3 / §2.4). It crosses the §5.A seam as an enum and NEVER as an
// errno or a cgroup file path: local_runsc concludes it by reading its own
// memcg's oom_group_kill, a remote provider from its scheduler's termination
// reason, and the §4 mediator — which must never learn that memcg exists —
// dispatches on these three values alone. That is what makes the §4.3
// resource-terminated disposition provider-portable rather than a host-local
// hack.
//
// It mirrors model.DeathCause: this is the §5.A wire form, the model one is the
// persisted form. They are deliberately separate types so the provider seam does
// not depend on the control-plane record shape.
type DeathCause int

const (
	// CauseUnknown = the unit is gone but WHY is undeterminable. A LEGAL verdict,
	// not a failure — a provider MUST report it rather than guess (§2.4 "does not
	// guess").
	// §4.3 treats it as dead-recoverable, with STRUCTURAL (not counted)
	// self-correction: a re-triggered termination after a caller's resume records
	// the real cause.
	CauseUnknown DeathCause = iota
	// CauseGone = dead for a non-resource reason (sentry crash, external kill,
	// clean exit). Disposition: dead-recoverable → resume (§4.3).
	CauseGone
	// CauseResourceExceeded = terminated for exceeding a resource cap (today the
	// RAM safety limit, whole-group via oom.group=1). Disposition:
	// resource-terminated → do NOT resume (§4.3): the same workload would
	// re-trigger the same termination, which is a crash loop.
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
	// Mounts are the §5.C Volume mount points to bind into the guest, PEER to Rootfs
	// rather than nested inside it. The nesting would be wrong twice over: the rootfs
	// is ONE assembled composition while these are N self-sufficient host mount points,
	// and they come from a DIFFERENT subsystem (§5.C, whose seam yields exactly one
	// thing — a host mount point — which the provider then binds, §5 preamble).
	//
	// The provider treats each identically to how it treats the rootfs handle: an
	// opaque source to bind, with no knowledge of what produced it (a local storage
	// unit, or a remote backend's FUSE mount point — indistinguishable here, which is
	// the point of the §5.C seam).
	Mounts []MountSource
	// No guest-identity field: the sandbox runs as guest root by default and the
	// engine does not model guest users (§2.2 — guest identity is not a
	// host-isolation pillar; sentry virtualizes guest uid 0). "Run as user X" is a
	// guest-native concern (su/sudo), not an engine primitive (§3.2.1).
}

// MountSource is one Volume mount to bind into the guest (§5.C). It carries the host
// mount point and where it goes — and deliberately NOT the volume's ID or its access
// mode.
//
// No volume ID, because the provider has no use for one: it binds a source at a
// destination, and identity is the mediator's concern (a remote provider would
// receive its own handle shape for the same role, exactly as with RootfsSource).
//
// No AccessMode either — only ReadOnly, which is the one bit that changes what the
// provider DOES. The mode's other half (exclusivity) is a control-plane fact already
// decided before this call, so passing the enum would hand the provider a decision it
// must not participate in.
type MountSource struct {
	// SourcePath is the host mount point, produced by the §5.C backend and opaque
	// here — the same kind of handle RootfsSource.MergedPath is, and exempted in the
	// seam guard for the same reason.
	SourcePath string
	// Dest is the absolute guest path to bind at.
	Dest string
	// ReadOnly makes this a read-only bind, materializing the shared-ro access mode.
	//
	// THE DIRECTION OF THIS BIT IS LOAD-BEARING, and it is a fact about the SUBSTRATE
	// rather than an assumption about our code. "ro is enforced at runtime" holds only
	// for a WRITABLE unit given a READ-ONLY mount: the guest's write gets a clean
	// EROFS. The REVERSE — an immutable unit given a WRITABLE mount spec — is not a
	// runtime error at all: `runsc create` HARD-FAILS in the gofer mount stage, so the
	// container is never created and the guest never runs. The engine's design never
	// produces a read-only unit (a snapshot yields an ordinary writable volume), so
	// that path is unreachable — but this bit must not be treated as "safe either
	// way", because one inverted bit turns what should be a 4xx at admission into an
	// opaque cold-start mount failure (volume.md D1).
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

	// PublishEndpoints are the host `ip:port` each published guest port was bound
	// to at this placement, in the SteeringSpec.PublishPorts order. They are what
	// §6.1's `GET …/network` reports as `host_endpoint`.
	//
	// It is on Placement rather than in the record because that is what it IS: a
	// placement product (§3.3), rebound every resume and invalidated with the old
	// placement. Hence §6.1 omits the key entirely when the engine holds no live
	// placement — a `paused` sandbox has no endpoint to report, and a falsy-but-
	// present field would invite callers to read it as "is this publish in effect"
	// (the same argument death_cause's omission rests on).
	//
	// Not a host primitive in the ❌-list sense: it is the address the engine
	// DELIVERS to the caller as the whole point of publish (§5.D), so a remote
	// provider produces one too — unlike a cgroup path or a netns fd, which such a
	// provider could not produce at all.
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

	// Adopt re-recognizes a SURVIVING sandbox's placement after a control-plane
	// crash (§4.4-A's other half / startup reconciliation). Given the SandboxID it
	// rebuilds the opaque placement handle from the sandbox's own labeled
	// resources, probes L1 liveness, and rediscovers the transport endpoint — the
	// SAME endpoint, NOT a new epoch, because the isolation unit never restarted
	// (§3.3). The return is a THREE-way outcome, not a boolean:
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
	// The DEATH CAUSE rides the alive=false return for a hard reason (§4.3 /
	// reaper-health D4c): the signal is placement-local and VOLATILE — for
	// local_runsc it is the memcg leaf's oom_group_kill, destroyed by the
	// `runsc delete` inside teardown — so this "unit is dead, placement still
	// intact" instant is the LAST moment it is readable. There is no
	// ask-again-later: a provider that tears down before reading has destroyed the
	// evidence. Hence the provider MUST read it before returning, not the caller
	// afterwards. Undeterminable → CauseUnknown, never a guess (§2.4).
	//
	// Left-hand side is engine vocabulary (SandboxID); right-hand side is an OPAQUE
	// handle + an ABSTRACT enum — no host primitive leaks (§5.A ❌ list). This is why
	// placement-local resources must carry a SandboxID label — the §3.3 reverse,
	// allowed (§4.4-A).
	Adopt(ctx context.Context, sandboxID string) (p *Placement, alive bool, cause DeathCause, err error)

	// ReclaimOrphans tears down every engine placement whose SandboxID is NOT in
	// keep (§4.4-A: an orphan is a placement backing no sandbox the store says
	// should hold one). The provider enumerates only its OWN host resources; the
	// mediator computes keep from the authoritative store and passes it in — the
	// store is read by the mediator and fed to the provider, never the reverse
	// (§4 subsystems don't call each other). Persistent state (Overlay upper) is
	// never touched — it is not a placement resource (§4.4-A does not touch
	// persistent state).
	//
	// THE KEEP SET ARRIVES COMPLETE, and that is load-bearing rather than a calling
	// convention (network.md D5 ㉒): reclaiming a live sandbox's rule set and then
	// re-installing it during Adopt is literally the delete-then-install sequence
	// measured to kill every live flow across the gap. The API shape makes that
	// sequence inexpressible.
	ReclaimOrphans(ctx context.Context, keep []string) error

	// ReinstallRuleSet re-installs a SURVIVING sandbox's §5.D rule set after a
	// control-plane restart — the second half of Adopt's obligation (§2.4: a rule set
	// is crash-surviving host state exactly like the netns, so taking over a live
	// placement means rebuilding it too; it is not something that can be added later).
	//
	// It MUST install as ONE atomic operation and MUST NOT flush connection tracking.
	// Both are measured constraints, not implementation advice (network.md D5 ㉒): one
	// atomic file leaves live flows intact 5/5, a delete-then-install kills them 5/5,
	// and a conntrack flush kills them outright wherever `tcp_loose=0`. The permission
	// hidden in the same measurement is that a provider may re-render blindly, with no
	// diff against what is already installed.
	ReinstallRuleSet(ctx context.Context, p *Placement, spec SteeringSpec) error

	// RuleHits reports how many packets each of this placement's §5.D rules has acted
	// on, keyed by the ABSTRACT counter name §5.D put on the rule (ruleset.Rule.Counter).
	// It backs §6.1's per-entry `hit_packet`.
	//
	// The key is the seam-clean part: a counter NAME is the vocabulary §5.D generates and
	// §5.A renders, so nothing host-shaped crosses — the same class of value as
	// Placement.PublishEndpoints, which §5.A permits for the same reason (a remote
	// provider has its enforcement layer's per-rule counters too, whereas it could not
	// produce a cgroup path at all). A member named after nft would NOT be seam-clean,
	// since that is one renderer's mechanism.
	//
	// TAKES A PLACEMENT, not a SandboxID: counters are placement-local state, so the
	// caller must already hold one to be asking, and the absence of a placement is the
	// answer rather than an error (§6.1 omits the key). A missing KEY in the returned map
	// means that counter is not present in what is installed — which is a DIFFERENT fact
	// from zero and must not be reported as zero (§2.4's acceptance discipline: a
	// bypassed rule's only symptom is a counter that reads 0, so manufacturing zeros
	// destroys the signal).
	//
	// Read-only and best-effort by nature: it must not install, re-render, or otherwise
	// touch the rule set, and a failure to read is not a placement failure.
	RuleHits(ctx context.Context, p *Placement) (map[string]uint64, error)

	// InstallSecondHop installs the ENGINE-LEVEL rule set pinning the second hop
	// (§2.4 invariant 2's second half): the egress of the host-local process behind
	// `via[].endpoint`, narrowed by the deployment-supplied uid.
	//
	// UID ONLY: `skuid` is the one narrowing predicate available on the `output` hook
	// (D5 ㉔), and a gid clause could only be ANDed onto the floor drop — i.e. narrow
	// the floor further, which is a second way for the pin to come off with the same
	// invisible symptom.
	//
	// Called ONCE at startup rather than per placement, because the predicate is not
	// per-sandbox: one uid covers every `via[]` in the deployment, and hop 2's traffic
	// arrives on no interface, so there is nothing sandbox-specific to narrow by
	// (measured: hop 2 counts ZERO in `forward` in all three shapes and is visible
	// only in `output` — network.md D5 ㉔).
	//
	// A nil uid = the deployment supplied none: install nothing. The corresponding
	// refusal of a `via[]` declaration happens at the write point, because a
	// declaration whose own invariant the engine cannot enforce must be refused rather
	// than half-honored.
	InstallSecondHop(ctx context.Context, rs ruleset.RuleSet, uid *int) error
}
