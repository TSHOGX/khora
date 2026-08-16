package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/TSHOGX/khora/internal/guestport"
	"github.com/TSHOGX/khora/internal/ruleset"
)

// LocalRunsc is the single-node runtime provider (the runtime provider contract). It materializes
// placement — a pre-built netns + veth pair + allocated guest IP — and
// cold-starts runsc with sandboxd (init mode) as entrypoint, adopting the netns
// (the runtime provider contract: --network=sandbox only adopts, never self-builds). The transport
// endpoint is the guest veth IP:port;  IP discovery for local_runsc degenerates
// to reading back our own allocation (the runtime provider contract network placement mechanism).
//
// Placement-local resources are NAMED by SandboxID (atomic transition rollback: labeling placement
// with its SandboxID is the ID-reference model reverse — allowed. It lets Adopt re-recognize a
// sandbox's own resources after a control-plane crash and ReclaimOrphans identify
// which resources back which sandbox). SandboxID-named IPs survive a restart: a
// restarted process learns which IPs survivors hold (from Adopt) and its
// allocator skips them.
//
// Host primitives — cgroup path, netns fd, the merged host path — live ONLY
// inside this implementation, never in the interface (the runtime provider contract; enforced by
// seam_test.go).
type LocalRunsc struct {
	// RunscRoot is the pinned canonical `runsc --root` (wrong root → `list`
	// silently empty; atomic transition rollback reconcile enumerates against this one root).
	RunscRoot string
	// StateDir holds per-launch OCI bundles: StateDir/<cid>/config.json.
	StateDir string
	// RunscBin is the runsc binary (default "runsc" on PATH).
	RunscBin string
	// Sudo, when true, prefixes runsc with sudo (dev convenience when the
	// process itself is not root but has passwordless sudo). Production runs the
	// orchestrator as root and leaves this false.
	Sudo bool

	// HostPublishIP is the host address published endpoints are bound on (the network declaration:
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
// deployment names none. Loopback deliberately: publishing is an EXPLICIT inbound
// exception (the isolation model), and defaulting to every host interface would make "reachable
// from the outside" a consequence of omitting a flag rather than of a decision.
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

// cgroupRoot is the cgroup v2 mount point — a host primitive, so it stays inside
// this implementation (the runtime provider contract list).
const cgroupRoot = "/sys/fs/cgroup"

// cgroupParent is the engine-owned cgroup layer beneath which runsc creates one
// leaf per sandbox (the runtime provider contract cgroup ownership: the engine builds the parent layer, the
// leaf belongs to runsc).
//
// A FLAT NAMING CONTAINER, not a kubelet-style cgroup-driver hierarchy (the runtime provider contract): no
// per-sandbox intermediate level, since re-creating that layer is the weight the engine vocabulary boundary
// rejects. One consequence is load-bearing for atomic transition rollback — each leaf is named for its
// cid, so an ORPHAN leaf under this parent is a scannable orphan-placement signal.
const cgroupParent = "engine"

// memOverheadBytes is the host-side memory headroom added ON TOP of the
// guest-visible RAM cap — local_runsc's IMPLEMENTATION CONSTANT, never a
// contract member (the runtime provider contract: the interface says only MemoryByte = N; how a provider
// pays for its own substrate is its business, and a remote provider's overhead is
// a different thing entirely).
//
// It covers the sentry + gofer footprint, measured at ~14 MiB idle. 64 MiB is
// deliberately generous because the error is asymmetric: too LITTLE headroom
// terminates the sandbox below the cap the guest was promised (the resource admission spec bug this
// split exists to avoid), too much only lifts the host ceiling slightly above N.
const memOverheadBytes int64 = 64 << 20

// placementHandle is provider-private state for stop/start (the runtime provider contract: callers
// address by SandboxID, never by placement internals). Carries exactly what
// teardown needs — the placement-local resource kinds relevant here (atomic transition rollback
// reclaim order: runsc → veth → netns).
type placementHandle struct {
	// sandbox is the SandboxID this placement serves. Held so teardown and Adopt can
	// derive the rule-set table name, which carries it verbatim (atomic transition rollback labeling).
	sandbox   string
	cid       string
	netns     string
	vethHost  string
	guestIP   string
	gatewayIP string
	bundleDir string
	// nftTable is this sandbox's rule-set table, ALWAYS set because it is derived from
	// the SandboxID rather than discovered — so there is no window where the name is
	// unknown and no need for an "installed?" signal. A PLACEMENT RESOURCE like the
	// netns: crash-surviving host state teardown must remove and reconciliation must
	// rebuild or reclaim (the provider security contract).
	nftTable string
	// publishEndpoints maps each published `{guest_port, proto}` to the host `ip:port`
	// bound for it AT THIS PLACEMENT (the ID-reference model: rebound every resume, old one invalidated —
	// but STABLE within one placement, see bindPublishEndpoints).
	publishEndpoints map[publishKey]string
	// publishHostPorts are the host ports held, so teardown returns them to the
	// allocator. Derived from publishEndpoints at every bind, never accumulated.
	publishHostPorts []uint16
	// cgroupLeaf is the host path of this sandbox's memcg/cpu leaf, or "" when the
	// sandbox has no limits (no leaf was requested).
	cgroupLeaf string
}

// cgroupLeafPath is the leaf path for a cid, and cgroupRelPath the same thing
// relative to the cgroup root (the form OCI cgroupsPath wants). Deterministic
// from the cid, so Adopt can re-find a crashed sandbox's leaf to read its death
// cause (health and reclamation) exactly as it re-finds the netns.
func cgroupRelPath(cid string) string  { return cgroupParent + "/" + cid }
func cgroupLeafPath(cid string) string { return filepath.Join(cgroupRoot, cgroupParent, cid) }

// namesFor derives the stable placement-local resource names for a SandboxID
// (atomic transition rollback SandboxID labeling). Deterministic, which is what lets Adopt re-find a
// crashed sandbox's resources.
func namesFor(sandbox string) placementHandle {
	sum := sha256.Sum256([]byte(sandbox))
	short := hex.EncodeToString(sum[:])[:12] // 12 hex → "vh"+12 = 14 ≤ 15 (IFNAMSIZ)
	return placementHandle{
		sandbox:  sandbox,
		cid:      cidPrefix + sandbox,
		netns:    nsPrefix + sandbox,
		vethHost: "vh" + short,
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
		// Adopt during atomic transition rollback reconciliation) makes the guarantee exact.
		ipSeq:    uint32(rand.Intn(4000)),
		inUse:    make(map[string]bool),
		pubInUse: make(map[uint16]bool),
	}, nil
}

func (p *LocalRunsc) Name() string { return "local_runsc" }

// allocSubnet hands out a fresh /30 (host .1, guest .2) each call — resume gets
// a NEW IP (the ID-reference model / health and reclamation IP-as-epoch). It skips IPs already held (inUse), so a
// freshly-restarted process cannot hand a new sandbox an IP a survivor adopted
// (atomic transition rollback). <sandbox /16>.<hi>.<lo*4>/30.
//
// The /16 comes from `internal/ruleset`, NOT a literal here, for correctness rather
// than tidiness: the L0 floor names the same /16 in its sibling-address set.
// Point-to-point veths do not provide cross-sandbox isolation on their own; the
// host still routes between two /30s, as the network substrate suite measures. Two literals could drift,
// and the drift hands out a /30 the floor does not cover: a silently reachable
// sibling. One fact belongs in one leaf package.
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

// Launch materializes placement and cold-starts the unit ( of the cold-start
// chain). Resources are named by spec.Sandbox (atomic transition rollback SandboxID labeling).
// Returns the placement with TransportEndpoint = guest veth IP:port. On any
// error it unwinds whatever it half-built (atomic transition rollback: failure recycles the
// half-built placement; the caller's persistent state is untouched).
func (p *LocalRunsc) Launch(ctx context.Context, spec LaunchSpec) (_ *Placement, err error) {
	if spec.Rootfs.MergedPath == "" {
		return nil, fmt.Errorf("runtime: LaunchSpec.Rootfs.MergedPath empty")
	}
	if spec.Sandbox == "" {
		return nil, fmt.Errorf("runtime: LaunchSpec.Sandbox empty (needed for placement naming, atomic transition rollback)")
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

	// a: build netns + veth + assign guest IP (engine pre-builds; runsc adopts).
	if err = p.buildNetns(&h, hostIP, guestIP); err != nil {
		return nil, fmt.Errorf("runtime: build netns: %w", err)
	}

	// a′: the network declaration ENFORCEMENT, and its POSITION IS THE OBLIGATION (the provider security contract): the rules go
	// on AFTER the netns/veth exist and BEFORE the isolated unit runs. Any window in
	// which the network is up and the rules are not is a fail-open window, and only
	// the side creating the placement has this hook point at all (the network enforcement locus).
	// A failure here unwinds the half-built placement via the defer above (atomic transition rollback);
	// there is no "launch it anyway without the rules" path (the provider security contract: an escape is a
	// failure, not a degradation).
	//
	// The guest default route lands in the SAME pre-launch window, and for an
	// independent reason: the sentry takes the L3 address off the kernel-side veth
	// when it boots, so a post-boot `ip route add` fails with `Nexthop has invalid
	// gateway` (measured, network substrate measurements).
	if err = p.bindPublishEndpoints(&h, spec.Steering); err != nil {
		return nil, err
	}
	if err = p.egressSetup(&h, hostIP, spec.Steering.RoutedEgress); err != nil {
		return nil, err
	}
	if err = p.installRuleSet(ctx, &h, spec.Steering); err != nil {
		return nil, err
	}

	// b: write OCI bundle (root.path = merged, entrypoint = sandboxd, adopt netns).
	h.bundleDir = filepath.Join(p.StateDir, h.cid)
	if err = os.MkdirAll(h.bundleDir, 0o755); err != nil {
		return nil, fmt.Errorf("runtime: bundle dir: %w", err)
	}

	// b′: materialize the resource admission resource Binding (the runtime provider contract). The engine creates the
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

	// c: launch runsc with pinned flags, detached.
	if err = p.runscLaunch(ctx, &h); err != nil {
		return nil, fmt.Errorf("runtime: runsc run: %w", err)
	}

	// d: ASSERT the caps actually bound, then apply the two host-side settings runsc
	// does not write. Mandatory, not defensive — the leaf-ownership trap fails OPEN
	// (the runtime provider contract; see bindLimits).
	if limited {
		if err = p.bindLimits(&h, spec.Limits); err != nil {
			return nil, err
		}
	}

	address, parseErr := netip.ParseAddr(guestIP)
	if parseErr != nil {
		return nil, fmt.Errorf("runtime: allocated guest address %q: %w", guestIP, parseErr)
	}
	pl := &Placement{
		Address:           address,
		TransportEndpoint: fmt.Sprintf("%s:%d", guestIP, guestport.Port),
		PublishEndpoints:  publishEndpointList(&h, spec.Steering),
	}
	pl.SetHandle(&h)
	return pl, nil
}

// bindPublishEndpoints binds one host endpoint per published `{guest_port, proto}` —
// the binding the network declaration leaves to the provider because it is a placement product (the runtime provider contract
// list / the network enforcement locus).
//
// # It binds only what is NOT already bound: WITHIN vs ACROSS a placement
//
// ACROSS placements (pause→resume, any rebuild) the endpoint MAY change — the ID-reference model's
// placement product. WITHIN one placement it may NOT, and the two callers that reach
// here on a LIVE placement each lose a different promise if it does:
//
//   - the publish hot-add (the network declaration's one Binding field writable on `running`): re-drawing
//     the endpoint of a publish nobody touched is not ADDITIVE, which is what the network declaration's
//     endorsement rests on, and it breaks the control-plane API's idempotent repeated publish — a second
//     identical POST would answer with a DIFFERENT endpoint.
//   - Adopt (atomic transition rollback): the runtime provider contract promises the same endpoint and no new epoch, so re-drawing
//     would hand a caller's cached endpoint to whatever sandbox the allocator reaches
//     next.
//
// So the reuse is keyed on what is already in the handle, and the ONE way a live
// placement's handle can be missing its bindings — a control-plane restart, which
// rebuilds the handle from resource names — is closed on the Adopt side by
// recoverPublishEndpoints.
func (p *LocalRunsc) bindPublishEndpoints(h *placementHandle, spec SteeringSpec) error {
	if len(spec.PublishPorts) == 0 {
		// Nothing declared: release whatever this handle held, so the allocator does not
		// keep a slot for a publish that is no longer in the record.
		p.releasePublishPorts(h, nil)
		h.publishEndpoints = nil
		return nil
	}
	bound := make(map[publishKey]string, len(spec.PublishPorts))
	for _, pp := range spec.PublishPorts {
		k := publishKey{port: pp.GuestPort, proto: pp.Proto}
		if _, dup := bound[k]; dup {
			// Deduplication is the declaration layer's job (the network declaration: the substrate will not
			// find it — nft accepts two identical DNATs and the second never matches). If
			// two items reach here for one PAIR, the two DNAT halves would target the same
			// guest port from two endpoints, so refuse rather than bind a second one.
			//
			// The pair, not the port: `[{8080,tcp},{8080,udp}]` is two legal items that
			// network.AddPublish appends and the network declaration keys separately (see publishKey).
			return fmt.Errorf("runtime: published %s appears twice in the steering spec — "+
				"deduplication belongs to the declaration layer (the network declaration)", k)
		}
		if ep, ok := h.publishEndpoints[k]; ok {
			bound[k] = ep // already bound at this placement — see the doc comment
			continue
		}
		port := p.allocPublishPort()
		if port == 0 {
			// The range is exhausted. Fail rather than bind a colliding port: two sandboxes
			// sharing a host endpoint means one of the two publishes silently never matches
			// (nft accepts the duplicate DNAT without complaint — the network declaration's reason
			// deduplication cannot be left to the substrate).
			return fmt.Errorf("runtime: no host port available for publishing %s "+
				"(range [%d,%d) exhausted)", k, publishPortBase, publishPortBase+publishPortSpan)
		}
		bound[k] = joinHostPort(p.publishAddr(), port)
	}
	// Ports the record no longer publishes go back to the allocator; every port still
	// bound stays held. publishHostPorts is rebuilt from the map so it cannot drift
	// from it — teardown frees exactly what is bound.
	p.releasePublishPorts(h, bound)
	h.publishEndpoints = bound
	return nil
}

// releasePublishPorts returns to the allocator every host port the handle holds that
// `keep` does not, and resets publishHostPorts to keep's ports. Called on every
// (re)bind so the held set is derived from the bindings rather than accumulated.
//
// INVARIANT for the ports that remain: every port in publishHostPorts is marked in
// use — allocPublishPort marks what it hands out, and recoverPublishEndpoints marks
// what it reads back off the installed table. No caller needs a follow-up marking
// pass, and one would hide a gap rather than close it.
func (p *LocalRunsc) releasePublishPorts(h *placementHandle, keep map[publishKey]string) {
	held := make(map[uint16]bool, len(keep))
	ports := make([]uint16, 0, len(keep))
	for _, ep := range keep {
		if _, port, err := splitEndpoint(ep); err == nil {
			held[port] = true
			ports = append(ports, port)
		}
	}
	for _, port := range h.publishHostPorts {
		if !held[port] {
			p.freePublishPort(port)
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	h.publishHostPorts = ports
}

// publishAddr is the host address published endpoints bind on, parsed. An unparseable
// HostPublishIP falls back to loopback rather than rendering a bare word into an
// address position: nft resolves a bare word THROUGH THE HOST RESOLVER, so a
// malformed address would not fail — it would silently become a rule matching an
// unrelated IP (the same reason renderPrefixSet takes literals only).
func (p *LocalRunsc) publishAddr() netip.Addr {
	if addr, err := netip.ParseAddr(p.publishIP()); err == nil {
		return addr
	}
	return netip.MustParseAddr(publishDefaultIP)
}

// recoverPublishEndpoints reads a surviving placement's publish bindings back OFF THE
// INSTALLED TABLE, so Adopt keeps the endpoints the sandbox already has instead of
// drawing new ones (the runtime provider contract `Adopt`: the same endpoint, no new epoch).
//
// This is the publish counterpart of rediscoverGuestIP + markInUse, and it is needed
// for the same reason: a restarted process's allocator starts empty, so without a
// read-back it re-issues ports in `ListSandboxes` order. Two survivors A@21000 and
// B@21001 adopted in the other order come back SWAPPED — a caller's cached endpoint
// then points at a DIFFERENT SANDBOX, which the ID-reference model does not license (it licenses a NEW
// placement's endpoint differing, not a live one's moving under it).
//
// Reversible by construction, like the netns name: the DNAT lines this provider
// renders carry the host endpoint (`daddr` + `dport`) and the guest port (`dnat … to
// <guest-ip>:<port>`) in one line, and the protocol keyword is the `dport` clause's
// own — which is exactly the `{guest_port, proto}` key. Both DNAT halves (prerouting
// and output) carry the same tuple, so parsing either yields the same map.
//
// Best-effort by design: a table that is absent or unparseable leaves the handle
// unbound, and ReinstallRuleSet then binds fresh endpoints and re-installs. That is a
// worse outcome (the endpoint moves) but not an unsafe one, and failing the adopt
// instead would demote a container L1 just reported alive (atomic transition rollback protect-on-uncertainty
// runs the other way).
func (p *LocalRunsc) recoverPublishEndpoints(ctx context.Context, h *placementHandle) {
	out, err := p.runOut(ctx, []string{"nft", "list", "table", "inet", nftTableName(h.sandbox)})
	if err != nil {
		return
	}
	bound := map[publishKey]string{}
	for _, m := range publishDNATRe.FindAllStringSubmatch(string(out), -1) {
		hostAddr, err := netip.ParseAddr(m[1])
		if err != nil {
			continue
		}
		hostPort, err := strconv.ParseUint(m[3], 10, 16)
		if err != nil {
			continue
		}
		guestPort, err := strconv.ParseUint(m[4], 10, 16)
		if err != nil {
			continue
		}
		k := publishKey{port: uint16(guestPort), proto: ruleset.Proto(m[2])}
		bound[k] = joinHostPort(hostAddr, uint16(hostPort))
	}
	if len(bound) == 0 {
		return
	}
	h.publishEndpoints = bound
	// Hold the recovered ports in the allocator BEFORE anything else is placed, so a
	// new sandbox cannot be handed a port this survivor is already using — the same
	// obligation markInUse discharges for the /30.
	p.releasePublishPorts(h, bound)
	for _, port := range h.publishHostPorts {
		p.markPublishPortInUse(port)
	}
}

// publishDNATRe matches one rendered publish DNAT line in `nft list` output,
// capturing the host address, the protocol, the host port and the guest port.
//
// It is anchored on the shape this provider itself renders (renderScope's
// ScopeHostEndpoint arm + renderVerdict's DNAT arm), which is what makes it
// reversible rather than a general nft parser. `nft list` quotes the counter name it
// echoes back (measured) and normalizes nothing else in these clauses.
var publishDNATRe = regexp.MustCompile(
	`(?:ip|ip6) daddr (\S+) (tcp|udp) dport (\d+) [^\n]*?dnat (?:ip|ip6) to \[?[^\s\[\]]+?\]?:(\d+)`)

// publishEndpointList returns the bound endpoints in the spec's declaration order,
// which is the order the control-plane API's read face reports them in.
func publishEndpointList(h *placementHandle, spec SteeringSpec) []string {
	if len(spec.PublishPorts) == 0 {
		return nil
	}
	out := make([]string, 0, len(spec.PublishPorts))
	for _, pp := range spec.PublishPorts {
		out = append(out, h.publishEndpoints[publishKey{port: pp.GuestPort, proto: pp.Proto}])
	}
	return out
}

// Stop cleanly stops the unit AND releases its placement-local resources (FS-first pause and resume
// pause: a paused sandbox "occupies only disk" — no netns/veth held). Resume builds
// a fresh placement with a new IP (the ID-reference model), so nothing here must survive.
func (p *LocalRunsc) Stop(ctx context.Context, pl *Placement) error {
	h, ok := pl.Handle().(*placementHandle)
	if !ok || h == nil {
		return nil
	}
	p.teardownPlacement(h)
	return nil
}

// Destroy tears down placement-local resources (netns/IP/cgroup) — same reclaim
// as Stop, idempotent.
func (p *LocalRunsc) Destroy(ctx context.Context, pl *Placement) error {
	h, ok := pl.Handle().(*placementHandle)
	if !ok || h == nil {
		return nil
	}
	p.teardownPlacement(h)
	return nil
}

// Adopt re-recognizes a surviving sandbox's placement after a control-plane
// crash (atomic transition rollback's other half): rebuild the handle from the SandboxID's stable resource
// names, probe liveness, and rediscover the endpoint by reading the SAME IP back
// from the netns (NOT a new epoch — the isolation unit never restarted, the ID-reference model).
// alive=false means the isolation unit is dead/absent (caller CAS-demotes to
// paused, FS-first pause and resume involuntary pause). The returned Placement is opaque; no host
// primitive leaks (the runtime provider contract).
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
		// DEAD, and the placement is still intact — the LAST instant the death cause is
		// readable (health and reclamation / death-cause propagation; the window is readDeathCause's). Read it now
		// and hand it out with the verdict; the mediator persists it in the same commit as
		// the demotion CAS.
		return nil, false, readDeathCause(h.cid), nil
	}

	// rediscover the endpoint (same IP — the container runs with it; atomic transition rollback "the
	// same IP").
	ip, err := p.rediscoverGuestIP(&h)
	if err != nil {
		// The container is L1-ALIVE but its endpoint is unrecoverable: UNCERTAINTY, not
		// death. alive=false here would make the mediator CAS-demote and ReclaimOrphans
		// tear down a provably-live container — irreversible — so surface an error and let
		// the mediator protect the placement and retry next boot (atomic transition rollback
		// protect-on-uncertainty). alive=false is for a genuinely dead unit (above).
		return nil, false, CauseUnknown, fmt.Errorf("runtime: Adopt %s: container alive but endpoint unrecoverable: %w", sandbox, err)
	}
	h.guestIP = ip
	// Recovered because re-rendering the rule set needs it: the self-gateway allow and
	// the publish return SNAT both name the gateway.
	h.gatewayIP = decLastOctet(ip)
	p.markInUse(ip) // survivor holds this IP — the allocator must skip it (atomic transition rollback)
	// Recover the publish bindings the same way and for the same reason — read back, not
	// re-drawn (the runtime provider contract: the same endpoint, no new epoch; see recoverPublishEndpoints).
	p.recoverPublishEndpoints(ctx, &h)

	address, err := netip.ParseAddr(ip)
	if err != nil {
		return nil, false, CauseUnknown, fmt.Errorf("runtime: Adopt %s: rediscovered guest address %q is invalid: %w", sandbox, ip, err)
	}
	pl := &Placement{Address: address, TransportEndpoint: fmt.Sprintf("%s:%d", ip, guestport.Port)}
	pl.SetHandle(&h)
	// alive → the cause is meaningless (nothing died); report Unknown rather than
	// inventing a value.
	return pl, true, CauseUnknown, nil
}

// ReclaimOrphans tears down every engine placement whose SandboxID is NOT in
// keep (atomic transition rollback: an orphan is a placement backing no sandbox the store says
// should hold one). keep is the store-derived set the mediator computes; the
// provider only enumerates its own host resources and reverse-dep-order tears
// down the rest. Persistent state (Overlay upper) is never touched — that is not
// a placement resource (atomic transition rollback does not touch persistent state).
//
// # The keep-set must be complete BEFORE anything is deleted
//
// A hard constraint, not a natural reading (network substrate measurements). The keep set arrives
// fully computed and this function deletes only what is outside it, so "reclaim
// first, then adopt" is deliberately not expressible through this API: reclaiming a
// live sandbox's TABLE and re-installing it during Adopt IS the delete-then-install
// sequence measured to kill every live flow across the gap.
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
// atomic file — the atomic transition rollback Adopt face of the network declaration enforcement.
//
// IT MAY RE-INSTALL BLINDLY, WITH NO DIFF, because the measurement that constrains the
// file's shape is also the permission: one atomic `nft -f` file does not disturb a live
// flow, while `delete` then install kills every one (see renderRuleSet, network substrate measurements).
// The same measurement forbids flushing conntrack in any form.
//
// ENDPOINTS ARE PRESERVED, only new items getting one: the TABLE is re-rendered blindly,
// the BINDINGS are not. Both callers hold a LIVE placement, where re-binding what is
// already bound breaks a promise — see bindPublishEndpoints for which promise, per caller.
func (p *LocalRunsc) ReinstallRuleSet(ctx context.Context, pl *Placement, spec SteeringSpec) error {
	h, ok := pl.Handle().(*placementHandle)
	if !ok || h == nil {
		return fmt.Errorf("runtime: ReinstallRuleSet on a placement with no handle")
	}
	if err := p.bindPublishEndpoints(h, spec); err != nil {
		return err
	}
	if spec.RoutedEgress {
		// The guest default route cannot be re-added after boot, as measured by the
		// network substrate suite, and
		// need not be: the isolation unit never restarted, so the route it booted with is
		// still in its netstack. Only the host-side halves are re-asserted.
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

// RuleHits implements Provider.RuleHits: this placement's the network declaration counters, keyed by the
// abstract name the network declaration minted. Read-only — it lists counters and installs nothing.
func (p *LocalRunsc) RuleHits(ctx context.Context, pl *Placement) (map[string]uint64, error) {
	h, ok := pl.Handle().(*placementHandle)
	if !ok || h == nil {
		return nil, fmt.Errorf("runtime: RuleHits on a placement with no handle")
	}
	return p.ruleHits(ctx, h.nftTable)
}

// enumeratePlacements lists engine placements present on this host by scanning
// netns names carrying the SandboxID label (atomic transition rollback: enumerate host placement).
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

// containerRunning probes runsc liveness for a container (atomic transition rollback liveness
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

// rediscoverGuestIP recovers the guest endpoint IP of a running placement ( IP
// rediscovery for Adopt) WITHOUT a new epoch (atomic transition rollback "the same IP"). runsc's
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

// teardownPlacement is the idempotent reclaim primitive (atomic transition rollback): reverse
// dependency order runsc → overlay(mediator owns) → veth → netns. Every step is
// best-effort so a partially-built placement (Launch failure) cleans fully.
func (p *LocalRunsc) teardownPlacement(h *placementHandle) {
	if h == nil {
		return
	}
	// runsc kill+delete. NOTE this is also the moment the death-cause signal is
	// DESTROYED: delete removes the memcg leaf runsc owns, taking
	// memory.events/oom_group_kill with it (health and reclamation). Anything that needs the cause
	// must have read it before reaching here — which is why Adopt reads it on the
	// !alive branch rather than leaving it to the caller.
	_ = p.runsc(context.Background(), "kill", h.cid, "KILL")
	_ = p.runsc(context.Background(), "delete", "--force", h.cid)
	// Reclaim the cgroup leaf. Normally a no-op (runsc removes a leaf it OWNS on
	// delete); it matters where runsc never got that far — a launch that failed
	// before/during `run`, or a leaf that became foreign — whose orphan leaf would
	// otherwise linger as a stale placement resource (atomic transition rollback). rmdir of a live/absent
	// cgroup fails harmlessly, so this stays best-effort like every step here.
	if h.cgroupLeaf != "" {
		_ = os.Remove(h.cgroupLeaf)
	} else {
		_ = os.Remove(cgroupLeafPath(h.cid))
	}
	// The network declaration rule set, BEFORE the veth: dropping the table first means a
	// half-torn-down placement never has rules pointing at a veth name a later sandbox
	// could recreate.
	//
	// (`destroy` rather than `delete`, for the reason in deleteRuleSet.)
	p.deleteRuleSet(h.nftTable)
	// Strict reverse-dependency order (atomic transition rollback: build was netns → veth → runsc, so
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
// controllers the leaf needs (the runtime provider contract: the engine builds the parent layer). Both halves
// are required: runsc
// refuses to launch if the parent is absent ("open .../memory.swap.max: no such
// file or directory"), and a leaf cannot carry cpu/memory limits unless the
// parent has delegated those controllers via subtree_control. Idempotent.
func ensureCgroupParent() error {
	dir := filepath.Join(cgroupRoot, cgroupParent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// Delegate only what the resource admission rate + RAM axes need. `pids` is intentionally
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
// Why an assertion and not a hope (the runtime provider contract): the cgroup leaf-ownership rule fails
// OPEN (buildOCISpec carries the measured constraint). A leaf foreign to runsc
// still LAUNCHES, with no RAM cap and every observable signal saying it is capped —
// a silent breach of the data-plane exposure boundary blast radius. So read the values back and fail the
// launch if they disagree; the caller unwinds the half-built placement (atomic transition rollback).
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
			return fmt.Errorf("runtime: CPU cap did NOT bind: cpu.max=%q want %q (leaf %s foreign to runsc? the runtime provider contract leaf ownership)", got, want, leaf)
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
			return fmt.Errorf("runtime: RAM cap did NOT bind: memory.max=%q want %d (leaf %s foreign to runsc? the runtime provider contract leaf ownership — this fails OPEN, hence the assertion)", got, limits.MemoryByte, leaf)
		}
		// swap.max=0 is what makes the cap a hard ceiling instead of a door into
		// zram (the runtime provider contract / ociMemory). runsc writes it from OCI swap == limit; assert it
		// rather than assume.
		if got, err := readCgroupFile(leaf, "memory.swap.max"); err != nil || got != "0" {
			return fmt.Errorf("runtime: memory.swap.max=%q want 0 (a cap with swap is not a cap, err=%v)", got, err)
		}

		// --- the two host-side knobs runsc does not write ---

		// (1) oom.group=1 — a provider security contract PROVIDER OBLIGATION, not tuning: termination must
		// be deterministic and WHOLE-GROUP. Without it the kernel picks one victim
		// (nondeterministic across identical runs), and worse, it can land in
		// "no eligible victim → retry forever", a path that writes kernel log at
		// ~66 MB/s and fills the host disk — BYPASSING the disk capacity cap
		// entirely (the data-plane exposure boundary). It is also what makes oom_group_kill a readable death
		// signal at all (health and reclamation).
		if err := writeCgroupFile(leaf, "memory.oom.group", "1"); err != nil {
			return fmt.Errorf("runtime: set memory.oom.group (the provider security contract deterministic whole-group termination): %w", err)
		}

		// (2) raise the HOST ceiling to N + overhead (the runtime provider contract two-values split, see
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
// (health and reclamation / the runtime provider contract Adopt). This is the local_runsc materialization of the abstract
// enum — the mediator never learns memcg exists.
//
// oom_group_kill, NOT oom_kill: the former counts whole-group kills (exactly the
// event oom.group=1 produces, measured as 1), while the latter counts individual
// tasks within the group and read 9 for the SAME single termination — a per-task
// counter cannot answer "was this sandbox killed for its RAM cap".
//
// Read-window discipline: only callable while the placement is intact. `runsc
// delete` inside teardownPlacement destroys the leaf and this signal with it, so
// health and reclamation has the provider hand the cause out together with its already-dead verdict.
func readDeathCause(cid string) DeathCause {
	b, err := os.ReadFile(filepath.Join(cgroupLeafPath(cid), "memory.events"))
	if err != nil {
		// No leaf (an unlimited sandbox, or already reclaimed) → we cannot tell.
		// Unknown is a legal verdict and health and reclamation self-corrects it; guessing is not
		// allowed (the provider security contract "does not guess").
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
		// Unknown: health and reclamation resumes both, but only Gone means "we actually looked".
		return CauseGone
	}
	return CauseUnknown
}

// buildNetns creates the netns + veth pair and assigns IPs (the runtime provider contract mechanism,
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
// the runtime provider contract pins the WHOLE set local_runsc depends on rather than inheriting runsc's
// defaults: each key carries one stated guarantee, and runsc's defaults drift
// across versions while the Rootfs subsystem's cross-runsc-version portability may not rest on
// "the default did not change". Two of these fail SILENTLY if dropped
// (`--platform=systrap` currently matches the default; a wrong `--root` makes
// `state`/`list` read empty and atomic transition rollback reverse reconciliation would see every live
// placement as nonexistent), which is why TestRunscGlobalFlagsArePinned pins the
// set by value rather than trusting review. Adding or removing a key here means
// updating that test's table with the guarantee the key carries.
func (p *LocalRunsc) runscGlobalFlags() []string {
	return []string{
		p.RunscBin,
		"--root=" + p.RunscRoot,
		"--platform=systrap",
		"--network=sandbox",
		"--overlay2=none",
		"--file-access=shared",
		// Explicitly pinned rather than inherited (the runtime provider contract): runsc's own default
		// already strips raw/AF_PACKET sockets, so the guarantee "the guest cannot
		// forge L2 frames" would otherwise rest on an UNDECLARED default. the network declaration's
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
