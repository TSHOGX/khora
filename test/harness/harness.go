//go:build substrate

// Package harness builds the real substrate fixtures shared by the end-to-end
// substrate tests (transport round-trip, e2e). It compiles the real sandboxd,
// builds a guest rootfs Template with it (via the preflight builder), and
// constructs the two real providers. Requires root.
package harness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/TSHOGX/khora/internal/btrfs"
	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/orchestrator"
	"github.com/TSHOGX/khora/internal/rootfs"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/test/substratereq"
)

// RepoRoot resolves the engine repo root from this file's location.
func RepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// tests run with CWD = the test package dir; walk up to the module root.
	for d := wd; d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatalf("could not find repo root from %s", wd)
	return ""
}

// BuildSandboxd compiles the real cmd/sandboxd as a static binary, with the
// `testguest` tag so the guest-side fault-injection seam is present (the
// crash-loop sentinel the reaper escalation test needs). That tag is what keeps
// the fault path OUT of a production guest — see cmd/sandboxd/faultinject.go.
// Everything else about this binary is the production build.
//
// BUILT ONCE PER TEST BINARY, not per call: the inputs are identical at every call
// site, so the ~0.5 s link was paid 30× per run for the same bytes. The result is
// shared, so it must be TREATED AS READ-ONLY — it is copied into each Template by
// build-rootfs.sh, never executed or modified in place. A test that needs a
// DIFFERENT binary (the broken-Template arm) has its own builder.
func BuildSandboxd(t *testing.T, repoRoot string) string {
	t.Helper()
	sandboxdOnce.Do(func() {
		// Not t.TempDir(): that is removed when the FIRST caller's test ends, which
		// would leave every later caller holding a path to a deleted file. os.MkdirTemp
		// lives for the process, which is the lifetime the memoized value needs.
		dir, err := os.MkdirTemp("", "harness-sandboxd")
		if err != nil {
			sandboxdErr = fmt.Errorf("temp dir for sandboxd: %w", err)
			return
		}
		bin := filepath.Join(dir, "sandboxd")
		cmd := exec.Command("go", "build", "-tags", "testguest", "-o", bin, "./cmd/sandboxd")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			sandboxdErr = fmt.Errorf("build sandboxd: %w: %s", err, out)
			return
		}
		sandboxdBin = bin
	})
	// Every caller fails, not just the first: a memoized failure that only surfaced
	// once would let later tests run against an empty path and fail somewhere less
	// obvious.
	if sandboxdErr != nil {
		t.Fatalf("%v", sandboxdErr)
	}
	return sandboxdBin
}

var (
	sandboxdOnce sync.Once
	sandboxdBin  string
	sandboxdErr  error
)

// BuildTemplate builds a guest rootfs Template dir named `name` under
// templatesDir, using the preflight builder + the given sandboxd binary.
func BuildTemplate(t *testing.T, repoRoot, templatesDir, name, sandboxdBin string) {
	t.Helper()
	dst := filepath.Join(templatesDir, name)
	build := exec.Command(filepath.Join(repoRoot, "test/preflight/build-rootfs.sh"), dst, sandboxdBin)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build-rootfs: %v: %s", err, out)
	}
}

// BuildBrokenTemplate builds a Template whose /sbin/sandboxd exits immediately
// (Health never comes up) — used to inject a  cold-start failure on real runsc
// (L4). The entrypoint is a tiny static binary that just exits 0.
func BuildBrokenTemplate(t *testing.T, repoRoot, templatesDir, name string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "exiter")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	main := "package main\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "exiter-bin")
	cmd := exec.Command("go", "build", "-o", bin, filepath.Join(src, "main.go"))
	cmd.Dir = src
	// single-file build outside any module (no go.mod in the temp dir).
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build exiter: %v: %s", err, out)
	}
	dst := filepath.Join(templatesDir, name)
	build := exec.Command(filepath.Join(repoRoot, "test/preflight/build-rootfs.sh"), dst, bin)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build-rootfs(broken): %v: %s", err, out)
	}
}

// InjectStaticBin compiles a single-file static Go program (src) into the
// already-built Template dir at /usr/bin/<name>. Used to drop tiny test probes
// into the bare rootfs (which has no coreutils) — e.g. a winsize reader for the
// PTY resize acceptance (C1). CGO off so it runs in the guest with no loader deps.
//
// MEMOIZED ON SOURCE CONTENT, for the same reason BuildSandboxd is: a full run asks for
// ~100 of these from only 14 distinct sources, and a probe build costs ~138 ms against
// ~2 ms to copy one. Keyed on the source rather than on `name` because the same program
// is deliberately injected under two names (guestServeSrc as both "serve" and "serve2"),
// and a name key would rebuild identical bytes while a content key collapses them.
func InjectStaticBin(t *testing.T, templatesDir, template, name, mainSrc string) {
	t.Helper()
	built := buildStaticProbe(t, mainSrc)
	dst := filepath.Join(templatesDir, template, "usr", "bin", name)
	// Copied rather than hardlinked: the cached binary outlives this Template, and a
	// Template is wiped and rebuilt per test (build-rootfs.sh rm -rf's its dest), so a
	// link would leave later callers holding a path into a deleted tree.
	b, err := os.ReadFile(built)
	if err != nil {
		t.Fatalf("read cached probe %s: %v", name, err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatalf("install probe %s: %v", name, err)
	}
	// WriteFile does not apply the mode to an existing file, and a Template rebuilt
	// between calls can leave one behind.
	if err := os.Chmod(dst, 0o755); err != nil {
		t.Fatal(err)
	}
}

// buildStaticProbe compiles mainSrc once per process and returns the cached path. The
// result is SHARED, so callers must treat it as read-only and copy it into place.
func buildStaticProbe(t *testing.T, mainSrc string) string {
	t.Helper()
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(mainSrc)))

	probeCacheMu.Lock()
	defer probeCacheMu.Unlock()
	if e, ok := probeCache[key]; ok {
		// Every caller fails, not only the first — see BuildSandboxd.
		if e.err != nil {
			t.Fatalf("%v", e.err)
		}
		return e.bin
	}
	// os.MkdirTemp, not t.TempDir(), for the lifetime reason in BuildSandboxd.
	dir, err := os.MkdirTemp("", "harness-probe")
	if err != nil {
		e := probeCacheEntry{err: fmt.Errorf("temp dir for probe: %w", err)}
		probeCache[key] = e
		t.Fatalf("%v", e.err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		e := probeCacheEntry{err: fmt.Errorf("write probe source: %w", err)}
		probeCache[key] = e
		t.Fatalf("%v", e.err)
	}
	bin := filepath.Join(dir, "probe")
	cmd := exec.Command("go", "build", "-o", bin, filepath.Join(dir, "main.go"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		e := probeCacheEntry{err: fmt.Errorf("build probe: %w: %s", err, out)}
		probeCache[key] = e
		t.Fatalf("%v", e.err)
	}
	probeCache[key] = probeCacheEntry{bin: bin}
	return bin
}

type probeCacheEntry struct {
	bin string
	err error
}

var (
	probeCacheMu sync.Mutex
	probeCache   = map[string]probeCacheEntry{}
)

// EngineOpts are the knobs the substrate suites actually vary when building an
// engine. Everything else (building sandboxd, building the Template, seeding
// builtins) is identical in every caller, which is why it lives here rather than
// being copied per file.
//
// The zero value is the common case: one "bare" Template on a t.TempDir() state
// dir, default timeouts.
type EngineOpts struct {
	ManualColdRecovery bool
	// HealthTimeout bounds the cold-start readiness gate. Zero means 30s.
	HealthTimeout time.Duration
	// DrainGrace bounds the FS-first pause and resume pause drain. Zero → the engine default; NEGATIVE
	// disables the drain, which only the drainrace suite wants.
	DrainGrace time.Duration
	// Probes are extra static Go binaries to inject into the Template before
	// launch, keyed by the name they get under /usr/bin (the bare rootfs ships no
	// coreutils, so anything a guest must run has to be injected).
	Probes map[string]string
	// TemplateFiles are extra files to plant in the Template (lower) tree, keyed by
	// path relative to the Template root — e.g. a marker proving overlay
	// read-through.
	TemplateFiles map[string]string
	// BaseDir overrides where the engine's state lives. Empty → t.TempDir(), which
	// is tmpfs here; the capacity axis needs real btrfs and passes an explicit
	// /var/tmp path instead (the root-required deployment).
	BaseDir string
	// NodeDiskByte / NodeMemoryByte are the resource-admission budgets. Zero →
	// unmetered.
	NodeDiskByte   int64
	NodeMemoryByte int64
	// StorePath overrides the store file. Empty → <base>/store.db. A crash-pair
	// test passes the SAME path to two engines to simulate a process restart.
	StorePath string
	// FaultAfterColdStart injects a crash right before the committing CAS.
	FaultAfterColdStart func(id model.SandboxID)
	// FaultAfterVolumeBytes injects a crash inside snapshot/clone after the bytes AND
	// the cap are written but before the `volumes` row is inserted — the Volume subsystem window
	// (volume cap enforcement).
	FaultAfterVolumeBytes func(id model.VolumeID)
	// SkipReconcile leaves startup reconcile un-run, for a test that wants to
	// inspect the pre-reconcile store.
	SkipReconcile bool

	// SecondHopUID is second-hop floor deployment input: the uid of the host-local
	// process behind `egress.via[].endpoint`. Unset ⇒ no second-hop table is installed
	// and `via[]` declarations are refused, so a suite exercising `via[]` MUST set it.
	//
	// It must be a uid the ENGINE does not run as. The substrate suites run as root, so
	// passing 0 here would make orchestrator.New refuse to assemble — see
	// network.ValidateSecondHopUID for why that uid drops the engine's own guest dials.
	SecondHopUID *int

	// HostPublishIP is the host address published endpoints bind to. Empty →
	// loopback (the engine's conservative default).
	HostPublishIP string
}

// Engine builds an assembled engine on the real substrate: compiles sandboxd,
// builds the Template (plus any probes/files), constructs the orchestrator, seeds
// the builtin Template rows, and runs startup reconcile. Returns the engine and
// the base dir (whose layout tests peek at to assert byte reclamation).
//
// Seeding before serving is not optional — the sandboxes.template FK (health and reclamation) would
// reject every create otherwise — and running reconcile matches production boot
// order, so the happy path is exercised against the same sequence production uses.
func Engine(t *testing.T, opts EngineOpts) (*orchestrator.Engine, string) {
	t.Helper()
	ctx := context.Background()
	repo := RepoRoot(t)

	base := opts.BaseDir
	if base == "" {
		base = t.TempDir()
	}
	tmpl := filepath.Join(base, "templates")
	sbxd := BuildSandboxd(t, repo)
	BuildTemplate(t, repo, tmpl, "bare", sbxd)

	for name, src := range opts.Probes {
		InjectStaticBin(t, tmpl, "bare", name, src)
	}
	for rel, content := range opts.TemplateFiles {
		full := filepath.Join(tmpl, "bare", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	healthTimeout := opts.HealthTimeout
	if healthTimeout == 0 {
		healthTimeout = 30 * time.Second
	}
	storePath := opts.StorePath
	if storePath == "" {
		storePath = filepath.Join(base, "store.db")
	}

	eng, err := orchestrator.New(orchestrator.Config{
		ManualColdRecovery:    opts.ManualColdRecovery,
		VolumeProvider:        fixtureVolumeProvider(t, base),
		TemplatesDir:          tmpl,
		StateDir:              filepath.Join(base, "state"),
		StorePath:             storePath,
		HealthTimeout:         healthTimeout,
		DrainGrace:            opts.DrainGrace,
		NodeDiskByte:          opts.NodeDiskByte,
		NodeMemoryByte:        opts.NodeMemoryByte,
		FaultAfterColdStart:   opts.FaultAfterColdStart,
		FaultAfterVolumeBytes: opts.FaultAfterVolumeBytes,
		SecondHopUID:          opts.SecondHopUID,
		HostPublishIP:         opts.HostPublishIP,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	if err := eng.SeedTemplates(ctx); err != nil {
		t.Fatalf("seed builtin templates: %v", err)
	}
	// All three volume-boot entry points run in production's order and position: BEFORE
	// anything can create a storage unit. So the
	// substrate suites exercise the ordering production boots with rather than a
	// convenient one — which matters most for the cap repair, whose whole job is to fix
	// residue a previous run left.
	if err := eng.SweepOrphanVolumes(ctx); err != nil {
		t.Logf("boot orphan-volume sweep (non-fatal, reaper retries): %v", err)
	}
	if err := eng.RepairVolumeCaps(ctx); err != nil {
		t.Logf("boot volume-cap repair (non-fatal, reaper retries): %v", err)
	}
	if err := eng.ReclaimVolumeStorage(ctx); err != nil {
		t.Logf("boot quota-row reclamation (non-fatal, reaper retries): %v", err)
	}
	// the engine-level second-hop floor, in production's position: installed once, BEFORE
	// serving and before Reconcile. With no uid supplied it installs nothing and
	// removes a stale table from a previous run — which is what makes a suite that
	// does NOT exercise `via[]` still exercise the removal path.
	if err := eng.InstallSecondHopTable(ctx); err != nil {
		t.Fatalf("install the second-hop table: %v", err)
	}
	if !opts.SkipReconcile {
		// production parity: reconcile runs at boot. On a fresh store it is a no-op,
		// which also asserts the atomic transition rollback path never disturbs a clean start.
		if err := eng.Reconcile(ctx); err != nil {
			t.Fatalf("startup reconcile: %v", err)
		}
	}
	return eng, base
}

// NftCounter reads a named counter out of one sandbox's rule-set table.
//
// EVERY DENY ASSERTION IN THIS SUITE NEEDS IT: "unreachable" alone is not a valid
// assertion, because a counter reading 0 is indistinguishable from a bypassed rule
// (the provider security contract). A deny is proved by "unreachable" AND "the counter
// moved after a deliberate probe".
//
// Returns -1 when the counter does not exist, which a caller must treat as a failed
// assertion rather than as zero: a missing counter means the rule was never
// rendered, which is the same silent pass in a different disguise.
func NftCounter(t *testing.T, sandbox model.SandboxID, counter string) int64 {
	t.Helper()
	table := "sbxnet_" + string(sandbox)
	out, err := exec.Command("nft", "-j", "list", "counters", "table", "inet", table).Output()
	if err != nil {
		return -1
	}
	var doc struct {
		Nftables []struct {
			Counter *struct {
				Name    string `json:"name"`
				Packets int64  `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parse nft counters for %s: %v", table, err)
	}
	for _, o := range doc.Nftables {
		if o.Counter != nil && o.Counter.Name == counter {
			return o.Counter.Packets
		}
	}
	return -1
}

// NftCounterIn reads a named counter out of an arbitrary table — used for the
// ENGINE-LEVEL second-hop table, which is not per-sandbox.
func NftCounterIn(t *testing.T, table, counter string) int64 {
	t.Helper()
	out, err := exec.Command("nft", "-j", "list", "counters", "table", "inet", table).Output()
	if err != nil {
		return -1
	}
	var doc struct {
		Nftables []struct {
			Counter *struct {
				Name    string `json:"name"`
				Packets int64  `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parse nft counters for %s: %v", table, err)
	}
	for _, o := range doc.Nftables {
		if o.Counter != nil && o.Counter.Name == counter {
			return o.Counter.Packets
		}
	}
	return -1
}

// DumpRuleSet returns a sandbox's rule-set table as text, for a failure message. A
// deny assertion that fails is almost always an ORDER problem, and the ordered
// listing is the only thing that shows it.
func DumpRuleSet(sandbox model.SandboxID) string {
	out, err := exec.Command("nft", "list", "table", "inet", "sbxnet_"+string(sandbox)).CombinedOutput()
	if err != nil {
		return "(no rule-set table: " + err.Error() + ")"
	}
	return string(out)
}

// CountRuleSetTables counts engine per-sandbox rule-set tables on this host — the
// leak assertion for the rule set, shaped exactly like CountEngineNetns. A rule set
// is crash-surviving host state (the provider security contract), so it leaks the same way a netns does.
func CountRuleSetTables(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("nft", "list", "tables", "inet").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 3 && f[0] == "table" && f[1] == "inet" &&
			strings.HasPrefix(f[2], "sbxnet_") && f[2] != "sbxnet_hop2" {
			n++
		}
	}
	return n
}

// Restart builds a SECOND engine over an existing engine's state — what a
// control-plane restart looks like from the host's point of view: a new process, an
// empty live-map, and every placement the previous one built still on the host.
//
// It deliberately does NOT rebuild the Template or sandboxd, unlike Engine: re-running
// the rootfs builder rewrites `<base>/templates/bare`, the LOWER DIR of every live
// sandbox's overlay mount — so a survivor's guest loses the files it is running from,
// and the resulting failure (a guest process that produces no output at all) looks
// nothing like the reconciliation defect a test using it is trying to detect.
// Production never rebuilds a Template under a live sandbox.
//
// It runs the real boot sequence in production's order: second-hop table → reconcile.
func Restart(t *testing.T, base string, opts EngineOpts) *orchestrator.Engine {
	t.Helper()
	ctx := context.Background()
	storePath := opts.StorePath
	if storePath == "" {
		storePath = filepath.Join(base, "store.db")
	}
	healthTimeout := opts.HealthTimeout
	if healthTimeout == 0 {
		healthTimeout = 30 * time.Second
	}
	eng, err := orchestrator.New(orchestrator.Config{
		ManualColdRecovery: opts.ManualColdRecovery,
		VolumeProvider:     fixtureVolumeProvider(t, base),
		TemplatesDir:       filepath.Join(base, "templates"),
		StateDir:           filepath.Join(base, "state"),
		StorePath:          storePath,
		HealthTimeout:      healthTimeout,
		DrainGrace:         opts.DrainGrace,
		NodeDiskByte:       opts.NodeDiskByte,
		NodeMemoryByte:     opts.NodeMemoryByte,
		SecondHopUID:       opts.SecondHopUID,
		HostPublishIP:      opts.HostPublishIP,
	})
	if err != nil {
		t.Fatalf("restart: orchestrator.New: %v", err)
	}
	if err := eng.InstallSecondHopTable(ctx); err != nil {
		t.Fatalf("restart: install the second-hop table: %v", err)
	}
	if !opts.SkipReconcile {
		if err := eng.Reconcile(ctx); err != nil {
			t.Fatalf("restart: reconcile: %v", err)
		}
	}
	return eng
}

// Fixtures on tmpfs exercise ordinary directories; quota fixtures use native btrfs.
func fixtureVolumeProvider(t *testing.T, base string) orchestrator.VolumeProvider {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(base, &st); err != nil {
		t.Fatal(err)
	}
	if st.Type == 0x9123683e {
		return orchestrator.VolumeProviderLocalBtrfs
	}
	return orchestrator.VolumeProviderPlainDir
}

// CountEngineNetns counts engine-owned netns on this host (the placement marker
// local_runsc enumerates, atomic transition rollback). Tests use it to assert no orphan leaked.
func CountEngineNetns(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/var/run/netns")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sbxns_") {
			n++
		}
	}
	return n
}

// BtrfsBase creates a state base on usable squota under /var/tmp, fails when the
// prerequisite is absent, and registers teardown followed by removal. Returns the base and
// its state dir.
//
// It exists because t.TempDir() is tmpfs here, so a suite whose subject is quotas, caps
// or subvolume reclaim would exercise a configuration the engine does not support
// (the root-required deployment).
//
// teardown runs BEFORE the RemoveAll and may be nil. It is a callback rather than
// something this helper does itself because os.RemoveAll CANNOT delete a btrfs
// subvolume (rmdir(2) refuses one), so the units have to go through the tool first —
// and which units exist is the caller's knowledge, not the harness's.
func BtrfsBase(t *testing.T, prefix, why string, teardown func(stateDir string)) (base, stateDir string) {
	t.Helper()
	base, err := os.MkdirTemp("/var/tmp", prefix)
	if err != nil {
		t.Fatal(err)
	}
	stateDir = filepath.Join(base, "state")
	// Registered before the prerequisite check so a failure removes the dir too. /var/tmp on this
	// machine already carries one leaked fixture dir from an earlier run, and a scratch
	// dir that silently survives costs the next run the ability to tell "the engine
	// leaks" from "my fixtures leak".
	t.Cleanup(func() {
		if teardown != nil {
			teardown(stateDir)
		}
		_ = os.RemoveAll(base)
	})
	mode, probeErr := (btrfs.Client{}).QuotaStatus(context.Background(), base)
	substratereq.Squota(t, base, mode == btrfs.QuotaSimple, probeErr)
	return base, stateDir
}

// Providers builds the two real providers rooted under base.
func Providers(t *testing.T, base, templatesDir string) (*rootfs.HostOverlayFS, *runtime.LocalRunsc) {
	t.Helper()
	rp, err := rootfs.NewHostOverlayFS(filepath.Join(base, "overlays"), templatesDir, filepath.Join(base, "layers"))
	if err != nil {
		t.Fatalf("rootfs provider: %v", err)
	}
	rt, err := runtime.NewLocalRunsc(filepath.Join(base, "runsc-root"), filepath.Join(base, "runsc-state"))
	if err != nil {
		t.Fatalf("runtime provider: %v", err)
	}
	return rp, rt
}
