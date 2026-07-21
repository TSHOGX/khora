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
	"strings"
	"sync"

	"engine/internal/transport"
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
// which resources back which sandbox). This is what closes the old ipSeq
// cross-restart TODO: a restarted process learns which IPs survivors hold (from
// Adopt) and its allocator skips them.
//
// Contract discipline (§5.A): the Provider interface carries only policy +
// handles. Host primitives — cgroup path, netns fd, the merged host path — live
// ONLY inside this implementation. A remote provider would carry different handle
// shapes; nothing here leaks upward.
type LocalRunsc struct {
	// RunscRoot is the pinned canonical `runsc --root` (R2: wrong root → `list`
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

	mu    sync.Mutex
	ipSeq uint32          // monotonic /30 subnet allocator within 10.66.0.0/16
	inUse map[string]bool // guest IPs currently held (seeded by Adopt across restart)
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

// placementHandle is provider-private state for stop/start (§5.A: callers
// address by SandboxID, never by placement internals). Carries exactly what
// teardown needs — the placement-local resource kinds relevant here (§4.4-A
// reclaim order: runsc → veth → netns).
type placementHandle struct {
	cid       string
	netns     string
	vethHost  string
	guestIP   string
	bundleDir string
}

// namesFor derives the stable placement-local resource names for a SandboxID
// (§4.4-A SandboxID labeling). Deterministic: the same SandboxID always maps to
// the same names, which is exactly what lets Adopt re-find a crashed sandbox's
// resources.
func namesFor(sandbox string) placementHandle {
	sum := sha256.Sum256([]byte(sandbox))
	short := hex.EncodeToString(sum[:])[:12] // 12 hex → "vh"+12 = 14 ≤ 15 (IFNAMSIZ)
	return placementHandle{
		cid:      cidPrefix + sandbox,
		netns:    nsPrefix + sandbox,
		vethHost: "vh" + short,
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
		ipSeq: uint32(rand.Intn(4000)),
		inUse: make(map[string]bool),
	}, nil
}

func (p *LocalRunsc) Name() string { return "local_runsc" }

// allocSubnet hands out a fresh /30 (host .1, guest .2) each call — resume gets
// a NEW IP (§3.3 / §4.3 IP-as-epoch). It skips IPs already held (inUse), so a
// freshly-restarted process cannot hand a new sandbox an IP a survivor adopted
// (§4.4-A). 10.66.<hi>.<lo*4>/30.
func (p *LocalRunsc) allocSubnet() (hostIP, guestIP string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		seq := p.ipSeq
		p.ipSeq++
		block := seq % 16000 // keep within 10.66.0.0/16 (64 /30s per third octet)
		third := (block / 64) % 256
		base := (block % 64) * 4
		hostIP = fmt.Sprintf("10.66.%d.%d", third, base+1)
		guestIP = fmt.Sprintf("10.66.%d.%d", third, base+2)
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

	// ④b: write OCI bundle (root.path = merged, entrypoint = sandboxd, adopt netns).
	h.bundleDir = filepath.Join(p.StateDir, h.cid)
	if err = os.MkdirAll(h.bundleDir, 0o755); err != nil {
		return nil, fmt.Errorf("runtime: bundle dir: %w", err)
	}
	netnsPath := filepath.Join("/var/run/netns", h.netns)
	oci := buildOCISpec(spec.Rootfs.MergedPath, netnsPath)
	b, _ := json.MarshalIndent(oci, "", "  ")
	if err = os.WriteFile(filepath.Join(h.bundleDir, "config.json"), b, 0o644); err != nil {
		return nil, fmt.Errorf("runtime: write config.json: %w", err)
	}

	// ④c: launch runsc with pinned flags (R3), detached.
	if err = p.runscLaunch(ctx, &h); err != nil {
		return nil, fmt.Errorf("runtime: runsc run: %w", err)
	}

	pl := &Placement{TransportEndpoint: fmt.Sprintf("%s:%d", guestIP, transport.Port)}
	pl.SetHandle(&h)
	return pl, nil
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
func (p *LocalRunsc) Adopt(ctx context.Context, sandbox string) (*Placement, bool, error) {
	if sandbox == "" {
		return nil, false, fmt.Errorf("runtime: Adopt empty sandbox")
	}
	h := namesFor(sandbox)
	h.bundleDir = filepath.Join(p.StateDir, h.cid)

	alive, err := p.containerRunning(ctx, h.cid)
	if err != nil {
		return nil, false, err
	}
	if !alive {
		return nil, false, nil
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
		return nil, false, fmt.Errorf("runtime: Adopt %s: container alive but endpoint unrecoverable: %w", sandbox, err)
	}
	h.guestIP = ip
	p.markInUse(ip) // survivor holds this IP — the allocator must skip it (§4.4-A)

	pl := &Placement{TransportEndpoint: fmt.Sprintf("%s:%d", ip, transport.Port)}
	pl.SetHandle(&h)
	return pl, true, nil
}

// ReclaimOrphans tears down every engine placement whose SandboxID is NOT in
// keep (§4.4-A: an orphan is a placement backing no sandbox the store says
// should hold one). keep is the store-derived set the mediator computes; the
// provider only enumerates its own host resources and reverse-dep-order tears
// down the rest. Persistent state (Overlay upper) is never touched — that is not
// a placement resource (§4.4-A 持久态分毫不动).
func (p *LocalRunsc) ReclaimOrphans(ctx context.Context, keep []string) error {
	keepSet := make(map[string]bool, len(keep))
	for _, s := range keep {
		keepSet[s] = true
	}
	found, err := p.enumeratePlacements()
	if err != nil {
		return err
	}
	for _, sandbox := range found {
		if keepSet[sandbox] {
			continue
		}
		h := namesFor(sandbox)
		h.bundleDir = filepath.Join(p.StateDir, h.cid)
		// best-effort read of the held IP so we free the allocator slot too.
		if ip, e := p.rediscoverGuestIP(&h); e == nil {
			h.guestIP = ip
		}
		p.teardownPlacement(&h)
	}
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

var inetRe = regexp.MustCompile(`inet (10\.66\.\d+)\.(\d+)/`)

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

// teardownPlacement is the idempotent reclaim primitive (§4.4-A): reverse
// dependency order runsc → overlay(mediator owns) → veth → netns. Every step is
// best-effort so a partially-built placement (Launch failure) cleans fully.
func (p *LocalRunsc) teardownPlacement(h *placementHandle) {
	if h == nil {
		return
	}
	// runsc kill+delete (releases the container's cgroup leaf on delete).
	_ = p.runsc(context.Background(), "kill", h.cid, "KILL")
	_ = p.runsc(context.Background(), "delete", "--force", h.cid)
	// netns delete cascades the peer veth; also drop the host veth in case not.
	if h.netns != "" {
		_ = p.ipCmd("netns", "del", h.netns)
	}
	if h.vethHost != "" {
		_ = p.ipCmd("link", "del", h.vethHost)
	}
	if h.bundleDir != "" {
		_ = os.RemoveAll(h.bundleDir)
	}
	p.freeIP(h.guestIP)
}

// buildNetns creates the netns + veth pair and assigns IPs (§5.A mechanism,
// proven by the Task A spike / m1-substrate-livespec).
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

// runscGlobalFlags returns `runsc --root=<pinned>` + the five global pins (R3;
// all global, before the subcommand).
func (p *LocalRunsc) runscGlobalFlags() []string {
	return []string{
		p.RunscBin,
		"--root=" + p.RunscRoot,
		"--platform=systrap",
		"--network=sandbox",
		"--overlay2=none",
		"--file-access=shared",
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
