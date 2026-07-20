package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"engine/internal/transport"
)

// LocalRunsc is the M1 single-node runtime provider (§5.A). It materializes
// placement — a pre-built netns + veth pair + allocated guest IP — and
// cold-starts runsc with sandboxd (init mode) as entrypoint, adopting the netns
// (§5.A: --network=sandbox only adopts, never self-builds). The transport
// endpoint is the guest veth IP:port; ⑥ IP discovery for local_runsc degenerates
// to reading back our own allocation (§5.A network placement mechanism).
//
// Contract discipline (§5.A): the Provider interface carries only policy +
// handles. Host primitives — cgroup path, netns fd, the merged host path — live
// ONLY inside this implementation (MergedPath arrives via the opaque
// RootfsSource; netns name / cid live in the private placement handle). A remote
// provider would carry different handle shapes; nothing here leaks upward.
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
	ipSeq uint32 // monotonic /30 subnet allocator within 10.66.0.0/16
}

// placementHandle is provider-private state for stop/start (§5.A: callers
// address by SandboxID, never by placement internals). Carries exactly what
// teardown needs — the five placement-local resource kinds relevant to M1
// (§4.4-A reclaim order: runsc → veth → netns).
type placementHandle struct {
	cid       string
	netns     string
	vethHost  string
	guestIP   string
	bundleDir string
}

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
		// random start reduces subnet collision across restarts within the
		// process lifetime (M1 in-memory; M2's reconcile owns cross-restart).
		ipSeq: uint32(rand.Intn(4000)),
	}, nil
}

func (p *LocalRunsc) Name() string { return "local_runsc" }

// allocSubnet hands out a fresh /30 (host .1, guest .2) each call — resume gets
// a NEW IP (§3.3 / §4.3 IP-as-epoch). 10.66.<hi>.<lo*4>/30.
func (p *LocalRunsc) allocSubnet() (hostIP, guestIP string, seq uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seq = p.ipSeq
	p.ipSeq++
	block := seq % 16000 // keep within 10.66.0.0/16 (64 /30s per third octet)
	third := (block / 64) % 256
	base := (block % 64) * 4
	hostIP = fmt.Sprintf("10.66.%d.%d", third, base+1)
	guestIP = fmt.Sprintf("10.66.%d.%d", third, base+2)
	return hostIP, guestIP, seq
}

// Launch materializes placement and cold-starts the unit (④ of the cold-start
// chain). Returns the placement with TransportEndpoint = guest veth IP:port.
// On any error it unwinds whatever it half-built (§4.4-A: failure recycles the
// half-built placement; the caller's persistent state is untouched).
func (p *LocalRunsc) Launch(ctx context.Context, spec LaunchSpec) (_ *Placement, err error) {
	if spec.Rootfs.MergedPath == "" {
		return nil, fmt.Errorf("runtime: LaunchSpec.Rootfs.MergedPath empty")
	}
	hostIP, guestIP, seq := p.allocSubnet()
	tok := fmt.Sprintf("%08x", seq)
	h := &placementHandle{
		cid:      "sbx-" + tok,
		netns:    "sbxns-" + tok,
		vethHost: "sbxh" + tok[:8], // <=15 chars for an iface name
		guestIP:  guestIP,
	}

	// unwind half-built placement on any failure path.
	defer func() {
		if err != nil {
			p.teardownPlacement(h)
		}
	}()

	// ④a: build netns + veth + assign guest IP (engine pre-builds; runsc adopts).
	if err = p.buildNetns(h, hostIP, guestIP); err != nil {
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
	if err = p.runscLaunch(ctx, h); err != nil {
		return nil, fmt.Errorf("runtime: runsc run: %w", err)
	}

	pl := &Placement{TransportEndpoint: fmt.Sprintf("%s:%d", guestIP, transport.Port)}
	pl.SetHandle(h)
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
// as Stop for M1 (idempotent). It does NOT delete the Overlay (Rootfs's job,
// invoked separately by the mediator; §4 subsystems don't call each other).
func (p *LocalRunsc) Destroy(ctx context.Context, pl *Placement) error {
	h, ok := pl.Handle().(*placementHandle)
	if !ok || h == nil {
		return nil
	}
	p.teardownPlacement(h)
	return nil
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
}

// buildNetns creates the netns + veth pair and assigns IPs (§5.A mechanism,
// proven by the Task A spike / m1-substrate-livespec).
func (p *LocalRunsc) buildNetns(h *placementHandle, hostIP, guestIP string) error {
	vethGuest := "sbxg" + h.vethHost[4:]
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
	if p.Sudo {
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %w: %s", argv, err, out)
	}
	return nil
}
