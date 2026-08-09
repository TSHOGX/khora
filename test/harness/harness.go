//go:build substrate

// Package harness builds the real substrate fixtures shared by the L2/L3
// substrate tests (transport round-trip, e2e). It compiles the real sandboxd,
// builds a guest rootfs Template with it (via the preflight builder), and
// constructs the two real providers. Requires root.
package harness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/orchestrator"
	"engine/internal/rootfs"
	"engine/internal/runtime"
)

// RepoRoot resolves the engine repo root from this file's location.
func RepoRoot(t *testing.T) string {
	t.Helper()
	// test/harness/harness.go → ../.. is the repo root.
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
func BuildSandboxd(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sandboxd")
	cmd := exec.Command("go", "build", "-tags", "testguest", "-o", bin, "./cmd/sandboxd")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sandboxd: %v: %s", err, out)
	}
	return bin
}

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
// (Health never comes up) — used to inject a ⑦ cold-start failure on real runsc
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
func InjectStaticBin(t *testing.T, templatesDir, template, name, mainSrc string) {
	t.Helper()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(templatesDir, template, "usr", "bin", name)
	cmd := exec.Command("go", "build", "-o", dst, filepath.Join(srcDir, "main.go"))
	cmd.Dir = srcDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("InjectStaticBin %s: %v: %s", name, err, out)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		t.Fatal(err)
	}
}

// EngineOpts are the knobs the substrate suites actually vary when building an
// engine. Everything else (building sandboxd, building the Template, seeding
// builtins) is identical in every caller, which is why it lives here rather than
// being copied per file.
//
// The zero value is the common case: one "bare" Template on a t.TempDir() state
// dir, default timeouts.
type EngineOpts struct {
	// HealthTimeout bounds the ⑦ readiness gate. Zero → 30s.
	HealthTimeout time.Duration
	// DrainGrace bounds the §4.2 pause drain. Zero → the engine default; NEGATIVE
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
	// /var/tmp path instead (ADR-0002).
	BaseDir string
	// NodeDiskByte / NodeMemoryByte are the §3.4 admission budgets. Zero →
	// unmetered.
	NodeDiskByte   int64
	NodeMemoryByte int64
	// StorePath overrides the store file. Empty → <base>/store.db. A crash-pair
	// test passes the SAME path to two engines to simulate a process restart.
	StorePath string
	// FaultAfterColdStart injects a crash right before the committing CAS.
	FaultAfterColdStart func(id model.SandboxID)
	// FaultAfterVolumeBytes injects a crash inside snapshot/clone after the bytes AND
	// the cap are written but before the `volumes` row is inserted — the §5.C window
	// (volume.md D7.3).
	FaultAfterVolumeBytes func(id model.VolumeID)
	// SkipReconcile leaves startup reconcile un-run, for a test that wants to
	// inspect the pre-reconcile store.
	SkipReconcile bool

	// SecondHopUID is the §2.4 invariant-2 deployment input: the uid of the host-local
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
// Seeding before serving is not optional — the sandboxes.template FK (§4.3) would
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
	// Production parity, and it matters more than parity usually does: the
	// orphan-VOLUME sweep runs BEFORE anything can create a storage unit (§5.C /
	// volume.md D7.1). A leaked quota row reserves no unit id, so a restart can hand
	// that id — along with its cap — to a brand-new volume. Running it here means the
	// substrate suites exercise the same ordering production boots with, rather than a
	// convenient one.
	if err := eng.SweepOrphanVolumes(ctx); err != nil {
		t.Logf("boot orphan-volume sweep (non-fatal, reaper retries): %v", err)
	}
	// Cap repair and quota-row reclamation, in production's order and position (§5.C /
	// volume.md D7.3, D7): both run BEFORE anything can create a storage unit. Running
	// them here means the substrate suites exercise the sequence production boots with
	// rather than a convenient one — which matters most for the cap repair, whose whole
	// job is to fix residue a previous run left.
	if err := eng.RepairVolumeCaps(ctx); err != nil {
		t.Logf("boot volume-cap repair (non-fatal, reaper retries): %v", err)
	}
	if err := eng.ReclaimQuotaRows(ctx); err != nil {
		t.Logf("boot quota-row reclamation (non-fatal, reaper retries): %v", err)
	}
	// §2.4 invariant 2's second half, in production's position: installed once, BEFORE
	// serving and before Reconcile. With no uid supplied it installs nothing and
	// removes a stale table from a previous run — which is what makes a suite that
	// does NOT exercise `via[]` still exercise the removal path.
	if err := eng.InstallSecondHopTable(ctx); err != nil {
		t.Fatalf("install the second-hop table: %v", err)
	}
	if !opts.SkipReconcile {
		// production parity: reconcile runs at boot. On a fresh store it is a no-op,
		// which also asserts the §4.4-A path never disturbs a clean start.
		if err := eng.Reconcile(ctx); err != nil {
			t.Fatalf("startup reconcile: %v", err)
		}
	}
	return eng, base
}

// NftCounter reads a named counter out of one sandbox's rule-set table.
//
// EVERY DENY ASSERTION IN THIS SUITE NEEDS IT, and that is the acceptance discipline
// rather than thoroughness: "unreachable" alone is not a valid assertion, because a
// rule set with the right rules in the wrong order behaves perfectly on ordinary
// traffic and differs only when it is being bypassed — and a floor whose counter
// reads 0 is INDISTINGUISHABLE from a floor that was bypassed (§2.4 / network.md
// D4.1, D5 ⑧). So a deny is proved by "unreachable" AND "the counter moved after a
// deliberate probe".
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
// is crash-surviving host state (§2.4), so it leaks the same way a netns does.
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
// It deliberately does NOT rebuild the Template or sandboxd, unlike Engine. That is
// the whole reason it exists: re-running the rootfs builder rewrites
// `<base>/templates/bare`, which is the LOWER DIR of every live sandbox's overlay
// mount — so a survivor's guest loses the files it is running from, and the resulting
// failure (a guest process that produces no output at all) looks nothing like the
// reconciliation defect a test using it is trying to detect. Production never rebuilds
// a Template under a live sandbox; a test that did would be measuring its own harness.
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
		TemplatesDir:   filepath.Join(base, "templates"),
		StateDir:       filepath.Join(base, "state"),
		StorePath:      storePath,
		HealthTimeout:  healthTimeout,
		DrainGrace:     opts.DrainGrace,
		NodeDiskByte:   opts.NodeDiskByte,
		NodeMemoryByte: opts.NodeMemoryByte,
		SecondHopUID:   opts.SecondHopUID,
		HostPublishIP:  opts.HostPublishIP,
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

// CountEngineNetns counts engine-owned netns on this host (the placement marker
// local_runsc enumerates, §4.4-A). Tests use it to assert no orphan leaked.
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
