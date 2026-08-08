package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"engine/internal/guestport"
	"engine/internal/ruleset"
)

// LocalRunsc is the single-node runtime provider (§5.A). It materializes
// placement — a pre-built netns + veth pair + allocated guest IP — and
// cold-starts runsc with sandboxd (init mode) as entrypoint, adopting the netns
// (§5.A: --network=sandbox only adopts, never self-builds). The transport
// endpoint is the guest veth IP:port; ⑥ IP discovery for local_runsc degenerates
// to reading back our own allocation (§5.A network placement mechanism).
//
// Placement-local resources are NAMED by SandboxID (§4.4-A: labeling placement
// with its SandboxID is the §3.3 reverse — allowed. It lets Adopt re-recognize a
// sandbox's own resources after a control-plane crash and ReclaimOrphans identify
// which resources back which sandbox). SandboxID-named IPs survive a restart: a
// restarted process learns which IPs survivors hold (from Adopt) and its
// allocator skips them.
//
// Contract discipline (§5.A): the Provider interface carries only policy +
// handles. Host primitives — cgroup path, netns fd, the merged host path — live
// ONLY inside this implementation. A remote provider would carry different handle
// shapes; nothing here leaks upward.
type LocalRunsc struct {
	// RunscRoot is the pinned canonical `runsc --root` (wrong root → `list`
	// silently empty; §4.4-A reconcile enumerates against this one root).
	RunscRoot string
	// StateDir holds per-launch OCI bundles: StateDir/<cid>/config.json.
	StateDir string
	// RunscBin is the runsc binary (default "runsc" on PATH).
	RunscBin string
	// Sudo, when true, prefixes runsc with sudo (dev convenience when the
	// process itself is not root but has passwordless sudo). Production runs the
	// orchestrator as root and leaves this false.
	Sudo bool

	// HostPublishIP is the host address published endpoints are bound on (§5.D:
	// the engine delivers a `host ip:port` and what sits in front of it is outside
	// the engine). Empty → publishDefaultIP.
	HostPublishIP string

	mu    sync.Mutex
	ipSeq uint32          // monotonic /30 subnet allocator within ruleset.SandboxCIDR
	inUse map[string]bool // guest IPs currently held (seeded by Adopt across restart)
	// pubSeq / pubInUse allocate host ports for published endpoints, the same shape
	// as the /30 allocator and for the same reason: a restarted process must not
	// hand a new sandbox a port an adopted survivor holds.
	pubSeq   uint16
	pubInUse map[uint16]bool
}

// publishDefaultIP is the host address published endpoints bind to when the
// deployment names none. Loopback by default, deliberately: publishing is an
// EXPLICIT inbound exception (§2.1 ③), and binding it to every host interface by
// default would make "the sandbox is reachable from the outside" a consequence of
// omitting a flag rather than of a decision. A deployment that wants external
// reachability names the address.
const publishDefaultIP = "127.0.0.1"

func (p *LocalRunsc) publishIP() string {
	if p.HostPublishIP != "" {
		return p.HostPublishIP
	}
	return publishDefaultIP
}

// resource-name prefixes: the provider OWNS this convention so enumeration is
// self-contained (it does not assume the SandboxID's own format). netns carries
// the SandboxID verbatim so it is reversible (enumerate → TrimPrefix → SandboxID);
// veth names are a short hash (IFNAMSIZ ≤ 15 chars) derived one-way from the
// SandboxID — never reversed, always recomputed from an enumerated SandboxID.
const (
	nsPrefix  = "sbxns_"
	cidPrefix = "sbxc_"
)

// cgroupRoot is the cgroup v2 mount point. A host primitive, sealed inside this
// implementation — it never appears in the Provider interface (§5.A ❌ list).
const cgroupRoot = "/sys/fs/cgroup"

// cgroupParent is the engine-owned cgroup layer beneath which runsc creates one
// leaf per sandbox (§5.A cgroup 归属: 引擎建父层、叶子归 runsc).
//
// It is a FLAT NAMING CONTAINER, deliberately not a kubelet-style cgroup-driver
// hierarchy (§5.A): we do not synthesize a per-sandbox intermediate level, since
// re-creating that layer is precisely the weight §1.1 rejects. One consequence
// is load-bearing for §4.4-A: because each leaf is named for its cid, an ORPHAN
// leaf under this parent is itself a scannable orphan-placement signal.
const cgroupParent = "engine"

// memOverheadBytes is the host-side memory headroom added ON TOP of the
// guest-visible RAM cap — local_runsc's IMPLEMENTATION CONSTANT, never a
// contract member (§5.A: the interface says only MemoryByte = N; how a provider
// pays for its own substrate is its business, and a remote provider's overhead is
// a different thing entirely).
//
// It covers the sentry + gofer footprint, measured at ~14 MiB idle. 64 MiB is
// deliberately generous: the failure mode of too LITTLE headroom is the sandbox
// being terminated below the cap the guest was promised (the §3.4 spec bug this
// split exists to avoid), whereas the cost of too much is only that the host
// ceiling sits slightly above N. Asymmetric, so round up.
const memOverheadBytes int64 = 64 << 20

// placementHandle is provider-private state for stop/start (§5.A: callers
// address by SandboxID, never by placement internals). Carries exactly what
// teardown needs — the placement-local resource kinds relevant here (§4.4-A
// reclaim order: runsc → veth → netns).
type placementHandle struct {
	// sandbox is the SandboxID this placement serves. Held so teardown and Adopt can
	// derive the rule-set table name, which carries it verbatim (§4.4-A labeling).
	sandbox   string
	cid       string
	netns     string
	vethHost  string
	guestIP   string
	gatewayIP string
	bundleDir string
	// nftTable is this sandbox's rule-set table, or "" if none was installed. A
	// PLACEMENT RESOURCE like the netns: it is crash-surviving host state, so
	// teardown must remove it and reconciliation must rebuild or reclaim it — it
	// cannot be added after the fact (§2.4).
	nftTable string
	// publishEndpoints maps each published guest port to the host `ip:port` bound
	// for it AT THIS PLACEMENT (§3.3: rebound every resume, old one invalidated).
	publishEndpoints map[uint16]string
	// publishHostPorts are the host ports held, so teardown returns them to the
	// allocator.
	publishHostPorts []uint16
	// cgroupLeaf is the host path of this sandbox's memcg/cpu leaf, or "" when the
	// sandbox has no limits (no leaf was requested). Provider-private, like every
	// other field here: it is the host primitive §5.A forbids in the interface, so
	// it lives only inside the opaque handle.
	cgroupLeaf string
}

// cgroupLeafPath is the leaf path for a cid, and cgroupRelPath the same thing
// relative to the cgroup root (the form OCI cgroupsPath wants). Deterministic
// from the cid, so Adopt can re-find a crashed sandbox's leaf to read its death
// cause (§4.3) exactly as it re-finds the netns.
func cgroupRelPath(cid string) string  { return cgroupParent + "/" + cid }
func cgroupLeafPath(cid string) string { return filepath.Join(cgroupRoot, cgroupParent, cid) }

// namesFor derives the stable placement-local resource names for a SandboxID
// (§4.4-A SandboxID labeling). Deterministic: the same SandboxID always maps to
// the same names, which is exactly what lets Adopt re-find a crashed sandbox's
// resources.
func namesFor(sandbox string) placementHandle {
	sum := sha256.Sum256([]byte(sandbox))
	short := hex.EncodeToString(sum[:])[:12] // 12 hex → "vh"+12 = 14 ≤ 15 (IFNAMSIZ)
	return placementHandle{
		sandbox:  sandbox,
		cid:      cidPrefix + sandbox,
		netns:    nsPrefix + sandbox,
		vethHost: "vh" + short,
		// The rule-set table carries the SandboxID verbatim, like the netns name, so
		// enumeration is reversible and reconciliation can both re-adopt a survivor's
		// rules and identify orphan tables (§4.4-A / §2.4: the rule set is
		// crash-surviving host state).
		nftTable: nftTableName(sandbox),
	}
}

func vethGuestName(vethHost string) string { return "vg" + vethHost[2:] }

// NewLocalRunsc constructs the provider.
func NewLocalRunsc(runscRoot, stateDir string) (*LocalRunsc, error) {
	if runscRoot == "" || stateDir == "" {
		return nil, fmt.Errorf("runtime: RunscRoot and StateDir are required")
	}
	if err := os.MkdirAll(runscRoot, 0o755); err != nil {
		return nil, fmt.Errorf("runtime: create RunscRoot: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("runtime: create StateDir: %w", err)
	}
	return &LocalRunsc{
		RunscRoot: runscRoot,
		StateDir:  stateDir,
		RunscBin:  "runsc",
		// random start reduces subnet collision across restarts; inUse (seeded by
		// Adopt during §4.4-A reconciliation) makes the guarantee exact.
		ipSeq:    uint32(rand.Intn(4000)),
		inUse:    make(map[string]bool),
		pubInUse: make(map[uint16]bool),
	}, nil
}

func (p *LocalRunsc) Name() string { return "local_runsc" }

// allocSubnet hands out a fresh /30 (host .1, guest .2) each call — resume gets
// a NEW IP (§3.3 / §4.3 IP-as-epoch). It skips IPs already held (inUse), so a
// freshly-restarted process cannot hand a new sandbox an IP a survivor adopted
// (§4.4-A). <sandbox /16>.<hi>.<lo*4>/30.
//
// The /16 comes from `internal/ruleset`, NOT from a literal here, and that is a
// correctness requirement rather than tidiness: §5.D's L0 floor names the same
// /16 as its SIBLING item (the materialization of §2.3 cross-sandbox network
// isolation, which point-to-point veth does not deliver on its own — the host
// still routes between two /30s, measured D5 ①). Two literals could drift, and
// the drift would hand out a /30 the floor does not cover — a silently reachable
// sibling. One fact, one leaf (D5 ④).
func (p *LocalRunsc) allocSubnet() (hostIP, guestIP string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	oct := ruleset.SandboxCIDR.Addr().As4()
	for {
		seq := p.ipSeq
		p.ipSeq++
		block := seq % 16000 // keep within the /16 (64 /30s per third octet)
		third := (block / 64) % 256
		base := (block % 64) * 4
		hostIP = fmt.Sprintf("%d.%d.%d.%d", oct[0], oct[1], third, base+1)
		guestIP = fmt.Sprintf("%d.%d.%d.%d", oct[0], oct[1], third, base+2)
		if !p.inUse[guestIP] {
			p.inUse[guestIP] = true
			return hostIP, guestIP
		}
	}
}

func (p *LocalRunsc) freeIP(guestIP string) {
	if guestIP == "" {
		return
	}
	p.mu.Lock()
	delete(p.inUse, guestIP)
	p.mu.Unlock()
}

func (p *LocalRunsc) markInUse(guestIP string) {
	if guestIP == "" {
		return
	}
	p.mu.Lock()
	p.inUse[guestIP] = true
	p.mu.Unlock()
}

// Launch materializes placement and cold-starts the unit (④ of the cold-start
// chain). Resources are named by spec.Sandbox (§4.4-A SandboxID labeling).
// Returns the placement with TransportEndpoint = guest veth IP:port. On any
// error it unwinds whatever it half-built (§4.4-A: failure recycles the
// half-built placement; the caller's persistent state is untouched).
func (p *LocalRunsc) Launch(ctx context.Context, spec LaunchSpec) (_ *Placement, err error) {
	if spec.Rootfs.MergedPath == "" {
		return nil, fmt.Errorf("runtime: LaunchSpec.Rootfs.MergedPath empty")
	}
	if spec.Sandbox == "" {
		return nil, fmt.Errorf("runtime: LaunchSpec.Sandbox empty (needed for placement naming, §4.4-A)")
	}
	hostIP, guestIP := p.allocSubnet()
	h := namesFor(spec.Sandbox)
	h.guestIP = guestIP
	h.gatewayIP = hostIP // the /30's host side IS this sandbox's gateway

	// unwind half-built placement on any failure path.
	defer func() {
		if err != nil {
			p.teardownPlacement(&h)
		}
	}()

	// ④a: build netns + veth + assign guest IP (engine pre-builds; runsc adopts).
	if err = p.buildNetns(&h, hostIP, guestIP); err != nil {
		return nil, fmt.Errorf("runtime: build netns: %w", err)
	}

	// ④a′: §5.D ENFORCEMENT, and its POSITION IS THE OBLIGATION (§2.4): the rules go
	// on AFTER the netns/veth exist and BEFORE the isolated unit runs. Any window in
	// which the network is up and the rules are not is a fail-open window, and only
	// the side creating the placement has this hook point at all (network.md D1 ①).
	// A failure here unwinds the half-built placement via the defer above (§4.4-A) —
	// there is no "launch it anyway without the rules" path, because that is exactly
	// the escape-is-failure-not-degradation rule (§2.4).
	//
	// The guest default route lands in the SAME pre-launch window, and for an
	// independent reason: the sentry takes the L3 address off the kernel-side veth
	// when it boots, so a post-boot `ip route add` fails with `Nexthop has invalid
	// gateway` (measured, network.md D5 ⑬).
	if err = p.bindPublishEndpoints(&h, spec.Steering); err != nil {
		return nil, err
	}
	if err = p.egressSetup(&h, hostIP, spec.Steering.RoutedEgress); err != nil {
		return nil, err
	}
	if err = p.installRuleSet(ctx, &h, spec.Steering); err != nil {
		return nil, err
	}

	// ④b: write OCI bundle (root.path = merged, entrypoint = sandboxd, adopt netns).
	h.bundleDir = filepath.Join(p.StateDir, h.cid)
	if err = os.MkdirAll(h.bundleDir, 0o755); err != nil {
		return nil, fmt.Errorf("runtime: bundle dir: %w", err)
	}

	// ④b′: materialize the §3.4 resource Binding (§5.A). The engine creates the
	// PARENT and delegates the controllers; the LEAF is left for runsc, because
	// runsc only writes limits to a leaf it created itself (see buildOCISpec).
	limited := spec.Limits.CPUMillis > 0 || spec.Limits.MemoryByte > 0
	if limited {
		if err = ensureCgroupParent(); err != nil {
			return nil, fmt.Errorf("runtime: cgroup parent: %w", err)
		}
		// A stale same-named leaf (previous incarnation reclaimed uncleanly) would be
		// "foreign" to runsc → limits silently skipped. Drop it first so runsc really
		// creates it.
		_ = os.Remove(cgroupLeafPath(h.cid))
		h.cgroupLeaf = cgroupLeafPath(h.cid)
	}

	netnsPath := filepath.Join("/var/run/netns", h.netns)
	cgPath := ""
	if limited {
		cgPath = cgroupRelPath(h.cid)
	}
	oci := buildOCISpec(spec.Rootfs.MergedPath, netnsPath, cgPath, spec.Limits, memOverheadBytes, spec.Mounts)
	b, _ := json.MarshalIndent(oci, "", "  ")
	if err = os.WriteFile(filepath.Join(h.bundleDir, "config.json"), b, 0o644); err != nil {
		return nil, fmt.Errorf("runtime: write config.json: %w", err)
	}

	// ④c: launch runsc with pinned flags, detached.
	if err = p.runscLaunch(ctx, &h); err != nil {
		return nil, fmt.Errorf("runtime: runsc run: %w", err)
	}

	// ④d: ASSERT the caps actually bound, then apply the two host-side settings
	// runsc does not write. The assertion is mandatory, not defensive: the
	// leaf-ownership trap fails OPEN (limits skipped, launch succeeds), so without
	// a read-back a mis-wired cap looks exactly like a working one (§5.A launch 后
	// 断言限额确已生效).
	if limited {
		if err = p.bindLimits(&h, spec.Limits); err != nil {
			return nil, err
		}
	}

	pl := &Placement{
		TransportEndpoint: fmt.Sprintf("%s:%d", guestIP, guestport.Port),
		PublishEndpoints:  publishEndpointList(&h, spec.Steering),
	}
	pl.SetHandle(&h)
	return pl, nil
}

// bindPublishEndpoints allocates one host endpoint per published guest port — the
// binding §5.D leaves to the provider because it is a placement product (§5.A ❌
// list / network.md D1 ②).
func (p *LocalRunsc) bindPublishEndpoints(h *placementHandle, spec SteeringSpec) error {
	if len(spec.PublishPorts) == 0 {
		return nil
	}
	h.publishEndpoints = make(map[uint16]string, len(spec.PublishPorts))
	for _, pp := range spec.PublishPorts {
		if _, dup := h.publishEndpoints[pp.GuestPort]; dup {
			// Deduplication is the declaration layer's job (§5.D: the substrate will not
			// find it — nft accepts two identical DNATs and the second never matches). If
			// two items reach here for one guest port, the two DNAT halves would target the
			// same port from two endpoints, so refuse rather than bind a second one.
			return fmt.Errorf("runtime: guest port %d appears twice in the steering spec — "+
				"deduplication belongs to the declaration layer (§5.D)", pp.GuestPort)
		}
		port := p.allocPublishPort()
		if port == 0 {
			// The range is exhausted. Fail the launch rather than bind a colliding port:
			// two sandboxes sharing a host endpoint means one of the two publishes
			// silently never matches (nft accepts the duplicate DNAT without complaint —
			// §5.D's reason deduplication cannot be left to the substrate).
			return fmt.Errorf("runtime: no host port available for publishing guest port %d "+
				"(range [%d,%d) exhausted)", pp.GuestPort, publishPortBase, publishPortBase+publishPortSpan)
		}
		h.publishHostPorts = append(h.publishHostPorts, port)
		h.publishEndpoints[pp.GuestPort] = fmt.Sprintf("%s:%d", p.publishIP(), port)
	}
	return nil
}

// publishEndpointList returns the bound endpoints in the spec's declaration order,
// which is the order §6.1's read face reports them in.
func publishEndpointList(h *placementHandle, spec SteeringSpec) []string {
	if len(spec.PublishPorts) == 0 {
		return nil
	}
	out := make([]string, 0, len(spec.PublishPorts))
	for _, pp := range spec.PublishPorts {
		out = append(out, h.publishEndpoints[pp.GuestPort])
	}
	return out
}

// Stop cleanly stops the unit AND releases its placement-local resources
// (§4.2 pause: release RAM/CPU; a paused sandbox "只占磁盘" — no netns/veth held).
// The Overlay artifact is untouched (that is the Rootfs subsystem's, §4). resume
// builds a fresh placement with a new IP (§3.3), so nothing here must survive.
func (p *LocalRunsc) Stop(ctx context.Context, pl *Placement) error {
	h, ok := pl.Handle().(*placementHandle)
	if !ok || h == nil {
		return nil
	}
	p.teardownPlacement(h)
	return nil
}

// Destroy tears down placement-local resources (netns/IP/cgroup) — same reclaim
// as Stop (idempotent). It does NOT delete the Overlay (Rootfs's job, invoked
// separately by the mediator; §4 subsystems don't call each other).
func (p *LocalRunsc) Destroy(ctx context.Context, pl *Placement) error {
	h, ok := pl.Handle().(*placementHandle)
	if !ok || h == nil {
		return nil
	}
	p.teardownPlacement(h)
	return nil
}

// Adopt re-recognizes a surviving sandbox's placement after a control-plane
// crash (§4.4-A 另一半): rebuild the handle from the SandboxID's stable resource
// names, probe liveness, and rediscover the endpoint by reading the SAME IP back
// from the netns (NOT a new epoch — the isolation unit never restarted, §3.3).
// alive=false means the isolation unit is dead/absent (caller CAS-demotes to
// paused, §4.2 involuntary pause). The returned Placement is opaque; no host
// primitive leaks (§5.A).
func (p *LocalRunsc) Adopt(ctx context.Context, sandbox string) (*Placement, bool, DeathCause, error) {
	if sandbox == "" {
		return nil, false, CauseUnknown, fmt.Errorf("runtime: Adopt empty sandbox")
	}
	h := namesFor(sandbox)
	h.bundleDir = filepath.Join(p.StateDir, h.cid)
	h.cgroupLeaf = cgroupLeafPath(h.cid)

	alive, err := p.containerRunning(ctx, h.cid)
	if err != nil {
		return nil, false, CauseUnknown, err
	}
	if !alive {
		// DEAD, and the placement is still intact — this is the LAST instant the
		// death cause is readable (§4.3 / reaper-health D4c). The memcg leaf lives
		// until `runsc delete`, which any teardown of this placement will run, so
		// there is no asking again later. Read it now and hand it out with the
		// verdict; the mediator persists it in the same commit as the demotion CAS.
		return nil, false, readDeathCause(h.cid), nil
	}

	// rediscover the endpoint (same IP — the container runs with it; §4.4-A 同一
	// IP). runsc's --network=sandbox moves the guest veth into gVisor's netstack,
	// so the guest IP is no longer visible inside the netns; but the HOST veth
	// still carries its /30 host address, and our allocation pins guest = host+1.
	ip, err := p.rediscoverGuestIP(&h)
	if err != nil {
		// The container is L1-ALIVE but we could not recover its endpoint. This is
		// UNCERTAINTY, not death — returning alive=false here would make the mediator
		// CAS-demote and ReclaimOrphans tear down a provably-live container (§4.4-A
		// 漏拆活容器不可逆). Surface an error instead so the mediator PROTECTS the
		// placement and retries next boot (§4.4-A protect-on-uncertainty). alive=false
		// is reserved for a genuinely dead/absent isolation unit (handled above).
		return nil, false, CauseUnknown, fmt.Errorf("runtime: Adopt %s: container alive but endpoint unrecoverable: %w", sandbox, err)
	}
	h.guestIP = ip
	// The gateway is the /30's host side, i.e. guest-1 — recovered here because
	// re-rendering the rule set needs it (the self-gateway allow and the publish
	// return SNAT both name it), and because rediscoverGuestIP read it to derive the
	// guest IP in the first place.
	h.gatewayIP = decLastOctet(ip)
	p.markInUse(ip) // survivor holds this IP — the allocator must skip it (§4.4-A)

	pl := &Placement{TransportEndpoint: fmt.Sprintf("%s:%d", ip, guestport.Port)}
	pl.SetHandle(&h)
	// alive → the cause is meaningless (nothing died); report Unknown rather than
	// inventing a value.
	return pl, true, CauseUnknown, nil
}

// ReclaimOrphans tears down every engine placement whose SandboxID is NOT in
// keep (§4.4-A: an orphan is a placement backing no sandbox the store says
// should hold one). keep is the store-derived set the mediator computes; the
// provider only enumerates its own host resources and reverse-dep-order tears
// down the rest. Persistent state (Overlay upper) is never touched — that is not
// a placement resource (§4.4-A 持久态不碰).
//
// # The keep-set must be complete BEFORE anything is deleted
//
// That is a hard constraint rather than a natural reading (network.md D5 ㉒). The
// keep set arrives fully computed here, and this function deletes only what is
// outside it — so "reclaim first, then adopt" is not expressible through this API,
// which is deliberate: reclaiming a live sandbox's TABLE and then re-installing it
// during Adopt is LITERALLY the delete-then-install sequence measured to kill every
// live flow across the gap (5/5).
//
// The rule set is enumerated as its own resource class here, not inferred from the
// netns list, because the two can disagree: a table survives a netns that was
// already reclaimed, and that table is exactly the kind of stale host state a later
// sandbox could inherit.
func (p *LocalRunsc) ReclaimOrphans(ctx context.Context, keep []string) error {
	keepSet := make(map[string]bool, len(keep))
	for _, s := range keep {
		keepSet[s] = true
	}
	found, err := p.enumeratePlacements()
	if err != nil {
		return err
	}
	// Rule-set tables belonging to sandboxes with no netns left: a table outlives
	// the netns it referenced whenever a previous reclaim was interrupted between the
	// two steps. Merged into the same pass so each orphan is torn down once.
	tables, _ := p.enumerateRuleSetTables(ctx)
	for _, tbl := range tables {
		sandbox, ok := sandboxOfTable(tbl)
		if !ok {
			continue // the engine-level second-hop table belongs to the process, not a sandbox
		}
		if !contains(found, sandbox) {
			found = append(found, sandbox)
		}
	}
	for _, sandbox := range found {
		if keepSet[sandbox] {
			continue
		}
		h := namesFor(sandbox)
		h.bundleDir = filepath.Join(p.StateDir, h.cid)
		h.cgroupLeaf = cgroupLeafPath(h.cid)
		// best-effort read of the held IP so we free the allocator slot too.
		if ip, e := p.rediscoverGuestIP(&h); e == nil {
			h.guestIP = ip
		}
		p.teardownPlacement(&h)
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// ReinstallRuleSet re-renders and re-installs a surviving sandbox's rule set as ONE
// atomic file — the §4.4-A Adopt face of §5.D enforcement.
//
// # Why it may re-install blindly, with no diff
//
// The atomic-reinstall measurement (network.md D5 ㉒) is not only a constraint, it
// is also a PERMISSION: because one atomic `nft -f` file does not disturb a live
// flow (5/5 survived, the ct entry's id unchanged, the masquerade counter at zero
// while bytes kept moving — the flow runs on the transform already stored in the ct
// entry and the nat hook is not re-evaluated), Adopt can re-render the whole rule
// set and load it without reasoning about what is already there. That is far simpler
// than diffing, and it is the ONLY shape that is safe.
//
// The two prohibitions from the same measurement:
//   - NEVER `delete` the table and then install (5/5 flows died — packets sent
//     during the gap left with the raw sandbox source address and the peer RST them).
//     The atomic file's leading bare-`table` + `delete` + definition is all in ONE
//     transaction, which is not the same thing.
//   - NEVER flush conntrack, in any form. Under `tcp_loose=0` a flush kills live
//     flows outright; this host's `tcp_loose=1` masks it by mid-stream-adopting the
//     flow, and that is a sysctl the engine neither owns nor may depend on.
//
// The endpoints are re-bound here rather than recovered: `Adopt` returns the SAME
// transport endpoint because the isolation unit never restarted (§3.3, no new
// epoch), but a published host port is a host-side rule, and re-deriving it keeps
// the record's read face consistent with what is actually installed.
func (p *LocalRunsc) ReinstallRuleSet(ctx context.Context, pl *Placement, spec SteeringSpec) error {
	h, ok := pl.Handle().(*placementHandle)
	if !ok || h == nil {
		return fmt.Errorf("runtime: ReinstallRuleSet on a placement with no handle")
	}
	if err := p.bindPublishEndpoints(h, spec); err != nil {
		return err
	}
	for _, port := range h.publishHostPorts {
		p.markPublishPortInUse(port)
	}
	if spec.RoutedEgress {
		// The guest default route cannot be re-added after boot (the sentry took the L3
		// address off the kernel-side veth, D5 ⑬) — but it does not need to be: the
		// isolation unit never restarted, so the route it booted with is still in its
		// netstack. Only the host-side halves are re-asserted.
		if err := writeSysctl("net/ipv4/ip_forward", "1"); err != nil {
			return fmt.Errorf("runtime: re-assert ip_forward during adopt: %w", err)
		}
	}
	if err := p.installRuleSet(ctx, h, spec); err != nil {
		return err
	}
	pl.PublishEndpoints = publishEndpointList(h, spec)
	return nil
}

// enumeratePlacements lists engine placements present on this host by scanning
// netns names carrying the SandboxID label (§4.4-A: enumerate host placement).
// netns is the primary marker — it is built first in the cold-start chain, so it
// is a superset of live containers (covers half-built placements too).
func (p *LocalRunsc) enumeratePlacements() ([]string, error) {
	entries, err := os.ReadDir("/var/run/netns")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("runtime: enumerate netns: %w", err)
	}
	var out []string
	for _, e := range entries {
		if s, ok := strings.CutPrefix(e.Name(), nsPrefix); ok {
			out = append(out, s) // reversible: netns name carries the SandboxID
		}
	}
	return out, nil
}

// containerRunning probes runsc liveness for a container (§4.4-A liveness
// judgement): `runsc state <cid>` → status. Missing container / non-running
// status → not alive.
func (p *LocalRunsc) containerRunning(ctx context.Context, cid string) (bool, error) {
	out, err := p.runscOut(ctx, "state", cid)
	if err != nil {
		// unknown container id → not alive (already gone). Not an error we surface.
		return false, nil
	}
	var st struct {
		Status string `json:"status"`
	}
	if e := json.Unmarshal(out, &st); e != nil {
		return false, nil
	}
	return st.Status == "running" || st.Status == "created", nil
}

// inetRe recovers a host veth's /30 address, anchored on the SHARED sandbox /16
// (ruleset.SandboxCIDR) rather than a literal — same reason allocSubnet derives
// it: a second copy could drift from the floor's sibling item.
var inetRe = func() *regexp.Regexp {
	oct := ruleset.SandboxCIDR.Addr().As4()
	return regexp.MustCompile(fmt.Sprintf(`inet (%d\.%d\.\d+)\.(\d+)/`, oct[0], oct[1]))
}()

// rediscoverGuestIP recovers the guest endpoint IP of a running placement (⑥ IP
// rediscovery for Adopt) WITHOUT a new epoch (§4.4-A 同一 IP). runsc's
// --network=sandbox moves the guest veth into gVisor's netstack, so the guest IP
// is invisible inside the netns; but the HOST-side veth (named by SandboxID)
// still carries its /30 host address, and the launch allocation pins
// guest = host + 1 within the /30. So we read the host veth and add one.
func (p *LocalRunsc) rediscoverGuestIP(h *placementHandle) (string, error) {
	out, err := p.runOut(context.Background(), []string{"ip", "-o", "-4", "addr", "show", "dev", h.vethHost})
	if err != nil {
		return "", err
	}
	m := inetRe.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("runtime: no engine IP on host veth %s", h.vethHost)
	}
	var lo int
	if _, err := fmt.Sscanf(string(m[2]), "%d", &lo); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.%d", string(m[1]), lo+1), nil
}

// decLastOctet recovers the /30's host (gateway) address from the guest address —
// the inverse of the launch allocation, which pins guest = host + 1.
func decLastOctet(ip string) string {
	i := strings.LastIndexByte(ip, '.')
	if i < 0 {
		return ""
	}
	n, err := strconv.Atoi(ip[i+1:])
	if err != nil || n == 0 {
		return ""
	}
	return ip[:i+1] + strconv.Itoa(n-1)
}

// teardownPlacement is the idempotent reclaim primitive (§4.4-A): reverse
// dependency order runsc → overlay(mediator owns) → veth → netns. Every step is
// best-effort so a partially-built placement (Launch failure) cleans fully.
func (p *LocalRunsc) teardownPlacement(h *placementHandle) {
	if h == nil {
		return
	}
	// runsc kill+delete. NOTE this is also the moment the death-cause signal is
	// DESTROYED: delete removes the memcg leaf runsc owns, taking
	// memory.events/oom_group_kill with it (§4.3). Anything that needs the cause
	// must have read it before reaching here — which is why Adopt reads it on the
	// !alive branch rather than leaving it to the caller.
	_ = p.runsc(context.Background(), "kill", h.cid, "KILL")
	_ = p.runsc(context.Background(), "delete", "--force", h.cid)
	// Reclaim the cgroup leaf. runsc removes a leaf it OWNS on delete, so this is
	// normally a no-op; it matters for the paths where runsc never got that far —
	// a launch that failed before/during `run`, or a leaf that became foreign — and
	// for those an orphan leaf under the engine parent would otherwise linger as a
	// stale placement resource (§4.4-A). rmdir of a live/absent cgroup fails
	// harmlessly, so this stays best-effort and idempotent like every step here.
	if h.cgroupLeaf != "" {
		_ = os.Remove(h.cgroupLeaf)
	} else {
		_ = os.Remove(cgroupLeafPath(h.cid))
	}
	// The §5.D rule set. A PLACEMENT RESOURCE like the netns — crash-surviving host
	// state (§2.4) — so it goes here rather than being left to leak, and it goes
	// BEFORE the veth: a rule referencing a veth by NAME survives that veth's
	// removal harmlessly (which is the whole reason for `iifname` over `iif`), but
	// dropping the table first means a half-torn-down placement never has rules
	// pointing at a veth that a later sandbox could recreate under the same name.
	//
	// `destroy` rather than `delete`: teardown runs on half-built placements where
	// the table may never have been installed, and `delete` on a nonexistent table
	// returns rc=1.
	p.deleteRuleSet(h.nftTable)
	// Strict reverse-dependency order (§4.4-A: build was netns → veth → runsc, so
	// teardown is runsc → veth → netns). Deleting the HOST veth cascades its guest
	// peer (a veth pair dies with either end), leaving an empty netns to drop last.
	if h.vethHost != "" {
		_ = p.ipCmd("link", "del", h.vethHost)
	}
	if h.netns != "" {
		_ = p.ipCmd("netns", "del", h.netns)
	}
	if h.bundleDir != "" {
		_ = os.RemoveAll(h.bundleDir)
	}
	p.freeIP(h.guestIP)
	for _, port := range h.publishHostPorts {
		p.freePublishPort(port)
	}
}

// ensureCgroupParent creates the engine-owned parent layer and delegates the
// controllers the leaf needs (§5.A 引擎建父层). Both halves are required: runsc
// refuses to launch if the parent is absent ("open .../memory.swap.max: no such
// file or directory"), and a leaf cannot carry cpu/memory limits unless the
// parent has delegated those controllers via subtree_control. Idempotent.
func ensureCgroupParent() error {
	dir := filepath.Join(cgroupRoot, cgroupParent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// Delegate only what the §3.4 rate + RAM axes need. `pids` is intentionally
	// NOT delegated (see ociResources: it caps host tasks and panics the sentry).
	// Writing an already-enabled controller is not an error, so this stays
	// idempotent across restarts.
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644); err != nil {
		// A controller already enabled, or unavailable on this host, must not fail
		// the launch here: the post-launch assertion in bindLimits is the real gate,
		// and it reports the concrete file that did not take the value.
		if !strings.Contains(err.Error(), "device or resource busy") {
			return fmt.Errorf("delegate controllers on %s: %w", dir, err)
		}
	}
	return nil
}

// bindLimits verifies the caps runsc was asked to apply ACTUALLY landed, and
// applies the two host-side knobs runsc does not write itself.
//
// Why an assertion and not a hope (§5.A): the cgroup leaf-ownership rule fails
// OPEN. If the leaf turns out to be foreign to runsc (someone pre-created the
// full path, or a stale leaf survived), runsc writes memory.swap.max=0, leaves
// memory.max at `max`, and the launch SUCCEEDS. The sandbox then runs with no RAM
// cap at all while every observable signal says it is capped — a silent breach of
// the §2.3 blast-radius invariant. So we read the values back and fail the launch
// if they disagree; the caller unwinds the half-built placement (§4.4-A).
func (p *LocalRunsc) bindLimits(h *placementHandle, limits ResourceLimits) error {
	leaf := h.cgroupLeaf
	if _, err := os.Stat(leaf); err != nil {
		return fmt.Errorf("runtime: cgroup leaf %s absent after launch (runsc did not create it): %w", leaf, err)
	}

	if limits.CPUMillis > 0 {
		want := fmt.Sprintf("%d %d", cpuPeriodMicros*limits.CPUMillis/1000, cpuPeriodMicros)
		got, err := readCgroupFile(leaf, "cpu.max")
		if err != nil {
			return fmt.Errorf("runtime: read cpu.max: %w", err)
		}
		if got != want {
			return fmt.Errorf("runtime: CPU cap did NOT bind: cpu.max=%q want %q (leaf %s foreign to runsc? §5.A leaf ownership)", got, want, leaf)
		}
	}

	if limits.MemoryByte > 0 {
		// The guest-visible value: runsc must have written exactly N, because that is
		// what it derived the guest's MemTotal from at boot.
		got, err := readCgroupFile(leaf, "memory.max")
		if err != nil {
			return fmt.Errorf("runtime: read memory.max: %w", err)
		}
		if got != fmt.Sprintf("%d", limits.MemoryByte) {
			return fmt.Errorf("runtime: RAM cap did NOT bind: memory.max=%q want %d (leaf %s foreign to runsc? §5.A leaf ownership — this fails OPEN, hence the assertion)", got, limits.MemoryByte, leaf)
		}
		// swap.max=0 is what makes the cap a hard ceiling instead of a door into
		// zram (§5.A / ociMemory). runsc writes it from OCI swap == limit; assert it
		// rather than assume.
		if got, err := readCgroupFile(leaf, "memory.swap.max"); err != nil || got != "0" {
			return fmt.Errorf("runtime: memory.swap.max=%q want 0 (a cap with swap is not a cap, err=%v)", got, err)
		}

		// --- the two host-side knobs runsc does not write ---

		// (1) oom.group=1 — a §2.4 PROVIDER OBLIGATION, not tuning: termination must
		// be deterministic and WHOLE-GROUP. Without it the kernel picks one victim
		// (nondeterministic across identical runs), and worse, it can land in
		// "no eligible victim → retry forever", a path that writes kernel log at
		// ~66 MB/s and fills the host disk — BYPASSING the disk capacity cap
		// entirely (§2.3). It is also what makes oom_group_kill a readable death
		// signal at all (§4.3).
		if err := writeCgroupFile(leaf, "memory.oom.group", "1"); err != nil {
			return fmt.Errorf("runtime: set memory.oom.group (§2.4 deterministic whole-group termination): %w", err)
		}

		// (2) raise the HOST ceiling to N + overhead (the §5.A two-values split, see
		// memOverheadBytes). Done AFTER launch on purpose: the guest's MemTotal was
		// already derived from N at boot, and measurement confirms this bump does not
		// move it.
		host := limits.MemoryByte + memOverheadBytes
		if err := writeCgroupFile(leaf, "memory.max", fmt.Sprintf("%d", host)); err != nil {
			return fmt.Errorf("runtime: raise host memory ceiling: %w", err)
		}
		// swap must stay pinned at 0 for the RAISED ceiling too, else the extra
		// headroom becomes swap-backed and the ceiling stops being hard.
		if err := writeCgroupFile(leaf, "memory.swap.max", "0"); err != nil {
			return fmt.Errorf("runtime: re-pin memory.swap.max=0: %w", err)
		}
	}
	return nil
}

// readCgroupFile reads and trims a single cgroup control file.
func readCgroupFile(leaf, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(leaf, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func writeCgroupFile(leaf, name, val string) error {
	return os.WriteFile(filepath.Join(leaf, name), []byte(val), 0o644)
}

// readDeathCause concludes WHY a dead sandbox's unit is gone, from its memcg leaf
// (§4.3 / §5.A Adopt). This is the local_runsc materialization of the abstract
// enum — the mediator never learns memcg exists.
//
// oom_group_kill, NOT oom_kill: the former counts whole-group kills (exactly the
// event oom.group=1 produces, measured as 1), while the latter counts individual
// tasks within the group and read 9 for the SAME single termination — a per-task
// counter cannot answer "was this sandbox killed for its RAM cap".
//
// Read-window discipline: this is only callable while the placement is intact.
// `runsc delete` inside teardownPlacement destroys the leaf and this signal with
// it, which is why Adopt reads here rather than letting the caller ask later
// (§4.3: the provider hands the death cause out together with its already-dead
// verdict, because the signal does not survive teardown).
func readDeathCause(cid string) DeathCause {
	b, err := os.ReadFile(filepath.Join(cgroupLeafPath(cid), "memory.events"))
	if err != nil {
		// No leaf (an unlimited sandbox, or already reclaimed) → we cannot tell.
		// Unknown is a legal verdict and §4.3 self-corrects it; guessing is not
		// allowed (§2.4 不许猜).
		return CauseUnknown
	}
	for _, line := range strings.Split(string(b), "\n") {
		field, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || field != "oom_group_kill" {
			continue
		}
		n, perr := strconv.ParseInt(val, 10, 64)
		if perr != nil {
			return CauseUnknown
		}
		if n > 0 {
			return CauseResourceExceeded
		}
		// The leaf is readable and reports no whole-group kill → it died of
		// something else. That IS a determination, so report Gone rather than
		// Unknown: §4.3 resumes both, but only Gone means "we actually looked".
		return CauseGone
	}
	return CauseUnknown
}

// buildNetns creates the netns + veth pair and assigns IPs (§5.A mechanism,
// proven on the real substrate).
func (p *LocalRunsc) buildNetns(h *placementHandle, hostIP, guestIP string) error {
	vethGuest := vethGuestName(h.vethHost)
	// idempotent: clear any stale same-named resources first.
	_ = p.ipCmd("netns", "del", h.netns)
	_ = p.ipCmd("link", "del", h.vethHost)

	steps := [][]string{
		{"netns", "add", h.netns},
		{"link", "add", h.vethHost, "type", "veth", "peer", "name", vethGuest},
		{"link", "set", vethGuest, "netns", h.netns},
		{"addr", "add", hostIP + "/30", "dev", h.vethHost},
		{"link", "set", h.vethHost, "up"},
		{"-n", h.netns, "addr", "add", guestIP + "/30", "dev", vethGuest},
		{"-n", h.netns, "link", "set", vethGuest, "up"},
		{"-n", h.netns, "link", "set", "lo", "up"},
	}
	for _, s := range steps {
		if err := p.ipCmd(s...); err != nil {
			return fmt.Errorf("ip %v: %w", s, err)
		}
	}
	return nil
}

// runscGlobalFlags returns `runsc --root=<pinned>` + the global pins (all global,
// so they go BEFORE the subcommand).
//
// §5.A pins the WHOLE set local_runsc depends on rather than inheriting runsc's
// defaults: each key carries one stated guarantee, and runsc's defaults drift
// across versions while §5.B's cross-runsc-version portability may not rest on
// "the default did not change". Two of these fail SILENTLY if dropped
// (`--platform=systrap` currently matches the default; a wrong `--root` makes
// `state`/`list` read empty and §4.4-A reverse reconciliation would see every live
// placement as nonexistent), which is why TestRunscGlobalFlagsArePinned pins the
// set by value rather than trusting review. Adding or removing a key here means
// updating that test's table WITH the § guarantee the key carries.
func (p *LocalRunsc) runscGlobalFlags() []string {
	return []string{
		p.RunscBin,
		"--root=" + p.RunscRoot,
		"--platform=systrap",
		"--network=sandbox",
		"--overlay2=none",
		"--file-access=shared",
		// Explicitly pinned rather than inherited (§5.A): runsc's own default
		// already strips raw/AF_PACKET sockets, so the guarantee "the guest cannot
		// forge L2 frames" would otherwise rest on an UNDECLARED default. §5.D's
		// interface-keyed predicate floor depends on it. CAP_NET_RAW is
		// correspondingly absent from the OCI cap request (see oci.go caps).
		"--net-raw=false",
		"--systemd-cgroup=false",
	}
}

// runsc runs a NON-detached runsc subcommand and captures output (kill/delete).
func (p *LocalRunsc) runsc(ctx context.Context, args ...string) error {
	return p.run(ctx, append(p.runscGlobalFlags(), args...))
}

// runscOut runs a runsc subcommand and RETURNS its stdout (state/list probes).
func (p *LocalRunsc) runscOut(ctx context.Context, args ...string) ([]byte, error) {
	return p.runOut(ctx, append(p.runscGlobalFlags(), args...))
}

// runscLaunch runs `runsc run --detach`. It must NOT pipe stdio: the detached
// sandbox process inherits the guest's stdout/stderr fds and holds them open
// for its whole life, so a CombinedOutput pipe never reaches EOF and Wait
// blocks forever. We redirect the detached container's guest stdio to a bundle
// log file (a real fd, no copy goroutine) so `runsc run --detach` returns.
func (p *LocalRunsc) runscLaunch(ctx context.Context, h *placementHandle) error {
	argv := append(p.runscGlobalFlags(), "run", "--detach", "--bundle="+h.bundleDir, h.cid)
	if p.Sudo {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	logf, err := os.Create(filepath.Join(h.bundleDir, "guest.log"))
	if err != nil {
		return fmt.Errorf("open guest.log: %w", err)
	}
	defer logf.Close()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Run(); err != nil {
		// surface the guest.log tail on failure (real output, never a claimed pass).
		b, _ := os.ReadFile(filepath.Join(h.bundleDir, "guest.log"))
		return fmt.Errorf("%v: %w: %s", argv, err, b)
	}
	return nil
}

func (p *LocalRunsc) ipCmd(args ...string) error {
	return p.run(context.Background(), append([]string{"ip"}, args...))
}

// run executes a command, optionally under sudo (dev). Output is captured and
// surfaced on error so failures report real output (never a claimed pass).
func (p *LocalRunsc) run(ctx context.Context, argv []string) error {
	_, err := p.runOut(ctx, argv)
	return err
}

// runOut executes a command and returns stdout. On non-zero exit it wraps the
// combined output in the error so failures report real output.
func (p *LocalRunsc) runOut(ctx context.Context, argv []string) ([]byte, error) {
	if p.Sudo {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v: %w: %s", argv, err, out)
	}
	return out, nil
}
