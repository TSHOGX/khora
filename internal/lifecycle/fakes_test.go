package lifecycle

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TSHOGX/warren/internal/model"
	"github.com/TSHOGX/warren/internal/rootfs"
	"github.com/TSHOGX/warren/internal/ruleset"
	"github.com/TSHOGX/warren/internal/runtime"
	"github.com/TSHOGX/warren/internal/transport"
	"github.com/TSHOGX/warren/internal/volume"
)

// seedBare inserts the "bare" builtin Template into st so a sandbox create /
// insert satisfies the §4.3 template FK. Production does this via the boot seed;
// the unit tests do it explicitly. Extra ids can be seeded too.
func seedBare(t *testing.T, st interface {
	CreateTemplate(context.Context, model.Template) error
}, ids ...string) {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{"bare"}
	}
	for _, id := range ids {
		if err := st.CreateTemplate(context.Background(), model.Template{
			ID: model.TemplateID(id), Builtin: true, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("seed template %q: %v", id, err)
		}
	}
}

// fakeRootfs / fakeRuntime / fakeClient are UNIT fakes (no substrate): they record
// call order and inject failures so the mediator's lock + CAS + unwind logic can be
// tested as pure logic. The real providers are covered by the substrate suites
// (internal/*/**_substrate_test.go and test/).
//
// NB: "L1/L2" elsewhere in this package means the §4.3 PROBE LAYERS (substrate
// liveness / transport Health), not a test tier — hence the wording here.

type fakeRootfs struct {
	mu        sync.Mutex
	calls     []string
	failMount bool
	// The remaining failure knobs cover the rest of the cold-start / teardown chain, so
	// a provider call can be failed at EVERY point the mediator calls one.
	failCreateOverlay  bool
	failPromote        bool
	failDestroyOverlay bool
	failUnmount        bool
	// maxLowerDepth is the §5.B mount ceiling MaxLowerDepth reports. 0 = no limit,
	// the default every test that is not about lineage depth wants;
	// failMaxLowerDepth makes the query itself fail.
	maxLowerDepth     int
	failMaxLowerDepth bool
	builtins          []string // boot-seed list returned by ListBuiltinTemplates
	// uncappedBytes is what UncappedBytes reports (the §3.4 admission gate's
	// second term); measureErr makes it fail, so the fail-closed path can be
	// asserted.
	uncappedBytes int64
	measureErr    error
	// promoteCost is what PromoteCostBytes forecasts a freeze of the source Overlay
	// would add to the uncapped term (§3.4 Promote admission); promoteCostErr makes
	// the forecast fail, so that fail-closed path can be asserted too.
	promoteCost    int64
	promoteCostErr error
	// overlayUsed/overlayLimit are what Usage reports for ANY Overlay ID — the
	// per-sandbox number the §6.1 disk-cap write point checks its strict inequality
	// against (§3.4 / ErrDiskCapBelowUsage). usageErr makes the read fail, so that
	// write point's fail-closed path can be asserted. Zero used is the common case:
	// every cap a test writes is then strictly above it.
	overlayUsed  int64
	overlayLimit int64
	usageErr     error
	// promoteGate blocks PromoteToTemplate mid-freeze (bytes-down, row-not-yet
	// window) until closed; promoteReached (if set) is closed when the freeze is
	// entered, so a test can synchronize a concurrent sweep against it (§4.4-A).
	promoteGate    chan struct{}
	promoteReached chan struct{}
}

func (f *fakeRootfs) log(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}
func (f *fakeRootfs) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRootfs) Name() string { return "fake_rootfs" }

// capNote renders the capacity-cap argument into the call log so tests can assert
// the §3.4 Binding really crossed the seam at each placement, and that
// handle-only rebuilds pass CapUnchanged rather than 0 (which would clear a live
// cap).
func capNote(capByte int64) string {
	if capByte == rootfs.CapUnchanged {
		return ";cap=unchanged"
	}
	return fmt.Sprintf(";cap=%d", capByte)
}

func (f *fakeRootfs) CreateOverlay(_ context.Context, id string, lower []string, capByte int64) (*rootfs.Overlay, error) {
	f.log("CreateOverlay:" + id + ";lower=" + strings.Join(lower, "|") + capNote(capByte))
	if f.failCreateOverlay {
		return nil, fmt.Errorf("injected CreateOverlay failure")
	}
	tmpl := ""
	if len(lower) > 0 {
		tmpl = lower[0]
	}
	return &rootfs.Overlay{ID: id, Template: tmpl, Lower: append([]string(nil), lower...)}, nil
}
func (f *fakeRootfs) OpenOverlay(_ context.Context, id string, lower []string, capByte int64) (*rootfs.Overlay, error) {
	f.log("OpenOverlay:" + id + ";lower=" + strings.Join(lower, "|") + capNote(capByte))
	tmpl := ""
	if len(lower) > 0 {
		tmpl = lower[0]
	}
	return &rootfs.Overlay{ID: id, Template: tmpl, Lower: append([]string(nil), lower...)}, nil
}

// Usage backs the §6.1 disk-cap write point's strict inequality (§3.4): the
// per-sandbox usage UncappedBytes deliberately cannot report, since a capped unit
// contributes zero there by contract. Logged so a test can assert both that the
// write point consulted the provider AND that an unrelated PATCH did NOT.
func (f *fakeRootfs) Usage(_ context.Context, overlayID string) (int64, int64, error) {
	f.log("Usage:" + overlayID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.usageErr != nil {
		return 0, 0, f.usageErr
	}
	return f.overlayUsed, f.overlayLimit, nil
}

// UncappedBytes backs the §3.4 admission gate's second term — bytes under no
// declared cap (rootfs-overlay D6.5: squota does not charge layer bytes to any
// sandbox, so they are accounted separately from the caps).
func (f *fakeRootfs) UncappedBytes(context.Context) (int64, error) {
	f.log("UncappedBytes")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.measureErr != nil {
		return 0, f.measureErr
	}
	return f.uncappedBytes, nil
}

// PromoteCostBytes backs the Promote admission gate (§3.4 / D6.5): the forecast of
// what freezing this Overlay would ADD to the uncapped term. Logged so a test can
// assert the gate really consulted the provider before the freeze.
func (f *fakeRootfs) PromoteCostBytes(_ context.Context, o *rootfs.Overlay, excludedMounts []string) (int64, error) {
	f.log("PromoteCostBytes:" + o.ID + ";excluded=" + strings.Join(excludedMounts, "|"))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.promoteCostErr != nil {
		return 0, f.promoteCostErr
	}
	return f.promoteCost, nil
}

func (f *fakeRootfs) PromoteToTemplate(ctx context.Context, o *rootfs.Overlay, newID string, excludedMounts []string) error {
	f.log("PromoteToTemplate:" + o.ID + "->" + newID + ";excluded=" + strings.Join(excludedMounts, "|"))
	// promoteGate, if non-nil, blocks the freeze (bytes-down window) until a test
	// signals — used to prove a concurrent sweep respects buildingLayers (§4.4-A).
	if f.promoteGate != nil {
		if f.promoteReached != nil {
			close(f.promoteReached)
			f.promoteReached = nil
		}
		select {
		case <-f.promoteGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.failPromote {
		return fmt.Errorf("injected PromoteToTemplate failure")
	}
	return nil
}

func (f *fakeRootfs) MaxLowerDepth(context.Context) (int, error) {
	if f.failMaxLowerDepth {
		return 0, fmt.Errorf("injected MaxLowerDepth failure")
	}
	return f.maxLowerDepth, nil
}
func (f *fakeRootfs) Mount(_ context.Context, o *rootfs.Overlay) (*rootfs.Merged, error) {
	f.log("Mount:" + o.ID)
	if f.failMount {
		return nil, fmt.Errorf("injected mount failure")
	}
	return &rootfs.Merged{MergedPath: "/fake/merged/" + o.ID}, nil
}
func (f *fakeRootfs) Unmount(_ context.Context, m *rootfs.Merged) error {
	f.log("Unmount:" + m.MergedPath)
	if f.failUnmount {
		return fmt.Errorf("injected Unmount failure")
	}
	return nil
}
func (f *fakeRootfs) DestroyOverlay(_ context.Context, o *rootfs.Overlay) error {
	f.log("DestroyOverlay:" + o.ID)
	if f.failDestroyOverlay {
		return fmt.Errorf("injected DestroyOverlay failure")
	}
	return nil
}
func (f *fakeRootfs) AdoptMerged(_ context.Context, o *rootfs.Overlay) (*rootfs.Merged, error) {
	f.log("AdoptMerged:" + o.ID)
	return &rootfs.Merged{MergedPath: "/fake/merged/" + o.ID}, nil
}

// builtins is the list ListBuiltinTemplates returns (boot seed). Empty by
// default; a test sets it to drive the seed. Unit tests seed the store directly
// via seedBare, so most leave this empty.
func (f *fakeRootfs) ListBuiltinTemplates(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.builtins...), nil
}

// SweepOrphanOverlays records the (live,building) keep-sets so the mediator's
// wiring can be asserted as pure logic (§4.4-A unit sweep). Reclaims nothing.
func (f *fakeRootfs) SweepOrphanOverlays(_ context.Context, live, building []string) (int, error) {
	f.log("SweepOrphanOverlays:live=" + strings.Join(append([]string(nil), live...), "|") +
		";building=" + strings.Join(append([]string(nil), building...), "|"))
	return 0, nil
}

// SweepOrphanLayers records the (live,building) keep-sets it was called with
// (§5.B / §4.4-A) so the mediator's sweep wiring can be asserted as pure logic. Reclaims
// nothing in the fake (no real layer dirs).
func (f *fakeRootfs) SweepOrphanLayers(_ context.Context, live, building []string) (int, error) {
	f.log("SweepOrphanLayers:live=" + strings.Join(append([]string(nil), live...), "|") +
		";building=" + strings.Join(append([]string(nil), building...), "|"))
	return 0, nil
}

// fakeVolume is the §5.C unit fake, the twin of fakeRootfs: it records call order
// and injects failures so the mediator's volume verbs (gate → bytes → row, and the
// unwind when the row loses) can be tested as pure logic. The real backends are
// covered by internal/volume's shared contract suite, on both arms.
//
// EVERY mediator Deps in this package must carry one, because the §3.4 disk inequality
// sums BOTH subsystems' uncapped bytes: with the disk axis metered, a nil backend makes
// the gate blind to every volume's bytes and it fails CLOSED — a configuration
// production cannot have.
type fakeVolume struct {
	mu    sync.Mutex
	calls []string

	// caps is what Capabilities reports. Defaults (zero value) to a backend that
	// supports NOTHING, so a test asserting the capability-negative 400 path does not
	// have to opt in; the constructor below flips all three on for the common case.
	caps volume.Capabilities

	// uncappedBytes is the §5.C half of the §3.4 second term; measureErr makes it
	// fail so the gate's fail-closed path can be asserted from this side too.
	uncappedBytes int64
	measureErr    error

	// used / usageErr drive Usage, which the cap write point's STRICT-inequality
	// refusal reads (§5.C / volume.md D7.3). usageErr specifically exercises
	// "refuse to set a cap blind", since an under-usage cap is irreversible.
	used     int64
	usageErr error

	failCreate  bool
	failCap     bool
	failDestroy bool

	// units tracks which volume storage the fake believes exists, so a test can
	// assert bytes-first ordering and the create unwind (row lost → bytes reclaimed)
	// rather than only reading the call log.
	units map[string]bool

	failSnapshot bool
	reclaimErr   error
	enumErr      error

	// appliedCaps records the cap each volume was created or derived WITH — i.e. what
	// actually reached the "storage" — so a test can assert the cap crossed the seam ON
	// the operation that made the bytes rather than trusting the record. That is the
	// distinction the whole cap-write invariant turns on (volume.md D7.3).
	appliedCaps map[string]int64
	// deletedRows stands in for the substrate's leaked quota rows: a test seeds ids and
	// asserts the reclamation round consumed them.
	deletedRows map[uint64]bool

	// roots keeps ONE fakeRoot per volume, so content PERSISTS across separate
	// OpenRoot calls. Handing out a fresh root each time would make an import
	// invisible to the following export — and the property being tested (bytes go in
	// and come back out with no sandbox anywhere, §5.C) is precisely that they
	// persist.
	roots map[string]*fakeRoot
}

func newFakeVolume() *fakeVolume {
	return &fakeVolume{
		caps:        volume.Capabilities{Snapshot: true, Clone: true, Quota: true},
		units:       map[string]bool{},
		roots:       map[string]*fakeRoot{},
		appliedCaps: map[string]int64{},
		deletedRows: map[uint64]bool{},
	}
}

func (f *fakeVolume) log(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeVolume) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeVolume) Name() string { return "fake_volume" }

func (f *fakeVolume) Capabilities() volume.Capabilities {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caps
}

func (f *fakeVolume) CreateVolume(_ context.Context, id string, capBytes int64) error {
	f.log(fmt.Sprintf("CreateVolume:%s:cap=%d", id, capBytes))
	if f.failCreate {
		return fmt.Errorf("fake: create volume %s failed", id)
	}
	if capBytes > 0 && !f.Capabilities().Quota {
		return fmt.Errorf("fake: %w", volume.ErrUnsupported)
	}
	f.mu.Lock()
	f.units[id] = true
	if f.appliedCaps == nil {
		f.appliedCaps = map[string]int64{}
	}
	f.appliedCaps[id] = capBytes
	f.mu.Unlock()
	return nil
}

func (f *fakeVolume) SetCapacity(_ context.Context, id string, capBytes int64) error {
	f.log(fmt.Sprintf("SetCapacity:%s:cap=%d", id, capBytes))
	if f.failCap {
		return fmt.Errorf("fake: set capacity on %s failed", id)
	}
	if capBytes > 0 && !f.Capabilities().Quota {
		return fmt.Errorf("fake: %w", volume.ErrUnsupported)
	}
	f.mu.Lock()
	if f.appliedCaps == nil {
		f.appliedCaps = map[string]int64{}
	}
	f.appliedCaps[id] = capBytes
	f.mu.Unlock()
	return nil
}

func (f *fakeVolume) Usage(_ context.Context, id string) (int64, int64, error) {
	f.log("Usage:" + id)
	if f.usageErr != nil {
		return 0, 0, f.usageErr
	}
	f.mu.Lock()
	limit := f.appliedCaps[id]
	f.mu.Unlock()
	return f.used, limit, nil
}

func (f *fakeVolume) Materialize(_ context.Context, id string) (*volume.Materialized, error) {
	f.log("Materialize:" + id)
	f.mu.Lock()
	f.units[id] = true // create-if-absent, matching the contract (§5.C)
	f.mu.Unlock()
	return &volume.Materialized{Volume: id, SourcePath: "/fake/volumes/" + id}, nil
}

func (f *fakeVolume) Release(_ context.Context, m *volume.Materialized) error {
	if m == nil {
		return nil
	}
	f.log("Release:" + m.Volume)
	return nil
}

func (f *fakeVolume) AdoptMaterialized(_ context.Context, id string) (*volume.Materialized, error) {
	f.log("AdoptMaterialized:" + id)
	f.mu.Lock()
	present := f.units[id]
	f.mu.Unlock()
	if !present {
		return nil, fmt.Errorf("fake: volume %s not present", id)
	}
	return &volume.Materialized{Volume: id, SourcePath: "/fake/volumes/" + id}, nil
}

func (f *fakeVolume) OpenRoot(_ context.Context, id string) (volume.Root, error) {
	f.log("OpenRoot:" + id)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.units[id] {
		return nil, fmt.Errorf("fake: volume %s not present", id)
	}
	r, ok := f.roots[id]
	if !ok {
		r = &fakeRoot{files: map[string][]byte{}, dirs: map[string]bool{}}
		f.roots[id] = r
	}
	return r, nil
}

func (f *fakeVolume) DestroyVolume(_ context.Context, id string) error {
	f.log("DestroyVolume:" + id)
	if f.failDestroy {
		return fmt.Errorf("fake: destroy volume %s failed", id)
	}
	f.mu.Lock()
	delete(f.units, id)
	f.mu.Unlock()
	return nil
}

// Snapshot / Clone record the cap they were handed, which is the assertion that
// matters: the cap must be written by THIS call, not by a follow-up the caller might
// skip — a missed cap is silent and undetectable afterwards (volume.md D7.3).
func (f *fakeVolume) Snapshot(_ context.Context, srcID, newID string, capBytes int64) error {
	f.log(fmt.Sprintf("Snapshot:%s->%s:cap=%d", srcID, newID, capBytes))
	return f.derive(srcID, newID, capBytes, "snapshot")
}

func (f *fakeVolume) Clone(_ context.Context, srcID, newID string, capBytes int64) error {
	f.log(fmt.Sprintf("Clone:%s->%s:cap=%d", srcID, newID, capBytes))
	return f.derive(srcID, newID, capBytes, "clone")
}

func (f *fakeVolume) derive(srcID, newID string, capBytes int64, kind string) error {
	if !f.Capabilities().Snapshot {
		return fmt.Errorf("fake: %s: %w", kind, volume.ErrUnsupported)
	}
	if f.failSnapshot {
		return fmt.Errorf("fake: %s %s failed", kind, srcID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.units[srcID] {
		return fmt.Errorf("fake: %s: source %s not present", kind, srcID)
	}
	f.units[newID] = true
	if f.appliedCaps == nil {
		f.appliedCaps = map[string]int64{}
	}
	f.appliedCaps[newID] = capBytes
	// Content copies, so the shared property "the copy carries the source's bytes" is
	// testable through the fake too.
	if src, ok := f.roots[srcID]; ok {
		cp := &fakeRoot{files: map[string][]byte{}, dirs: map[string]bool{}}
		src.mu.Lock()
		for n, b := range src.files {
			cp.files[n] = append([]byte(nil), b...)
		}
		src.mu.Unlock()
		f.roots[newID] = cp
	}
	return nil
}

// ReclaimQuotaRows / DeletedRowIDs back the §5.C leaked-row reclamation round. The fake
// records what it was asked to reclaim so the mediator's wiring is assertable as pure
// logic; it reclaims everything it is given.
func (f *fakeVolume) ReclaimQuotaRows(_ context.Context, ids []uint64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("ReclaimQuotaRows:n=%d", len(ids)))
	n := 0
	for _, id := range ids {
		if f.deletedRows[id] {
			delete(f.deletedRows, id)
			n++
		}
	}
	return n, f.reclaimErr
}

func (f *fakeVolume) DeletedRowIDs(context.Context) []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uint64, 0, len(f.deletedRows))
	for id := range f.deletedRows {
		out = append(out, id)
	}
	return out
}

func (f *fakeVolume) UncappedBytes(context.Context) (int64, error) {
	f.log("UncappedBytes")
	if f.measureErr != nil {
		return 0, f.measureErr
	}
	return f.uncappedBytes, nil
}

// ListUnitIDs is the backend's half of orphan reclamation: enumerate, and let the
// mediator decide. enumErr lets a test drive the fail-closed path, where an unreadable
// enumeration must reclaim NOTHING rather than be read as "no units".
func (f *fakeVolume) ListUnitIDs(context.Context) ([]string, error) {
	f.log("ListUnitIDs")
	if f.enumErr != nil {
		return nil, f.enumErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.units))
	for id := range f.units {
		out = append(out, id)
	}
	sort.Strings(out) // deterministic, so a test can assert on the list
	return out, nil
}

// fakeRoot is an in-memory volume.Root for the import/export verbs. It is
// deliberately NOT root-confinement-checked: confinement is a property of the real
// openat2 accessor and is asserted in internal/volume's own unit test, so faking it
// here would only assert that the fake agrees with itself.
type fakeRoot struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool
}

func (r *fakeRoot) Stat(name string) (volume.FileInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dirs[name] || name == "." {
		return volume.FileInfo{Name: name, IsDir: true}, nil
	}
	b, ok := r.files[name]
	if !ok {
		return volume.FileInfo{}, fs.ErrNotExist
	}
	return volume.FileInfo{Name: name, Size: int64(len(b))}, nil
}

func (r *fakeRoot) List(string) ([]volume.FileInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]volume.FileInfo, 0, len(r.files))
	for n, b := range r.files {
		out = append(out, volume.FileInfo{Name: n, Size: int64(len(b))})
	}
	return out, nil
}

func (r *fakeRoot) ReadAt(name string, p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.files[name]
	if !ok {
		return 0, fs.ErrNotExist
	}
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *fakeRoot) WriteAt(name string, p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.files[name]
	if need := int(off) + len(p); need > len(b) {
		grown := make([]byte, need)
		copy(grown, b)
		b = grown
	}
	copy(b[off:], p)
	r.files[name] = b
	return len(p), nil
}

func (r *fakeRoot) Mkdir(name string, _ fs.FileMode) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dirs == nil {
		r.dirs = map[string]bool{}
	}
	r.dirs[name] = true
	return nil
}

func (r *fakeRoot) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, name)
	delete(r.dirs, name)
	return nil // idempotent, matching the contract
}

func (r *fakeRoot) Rename(oldName, newName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.files[oldName]
	if !ok {
		return fs.ErrNotExist
	}
	r.files[newName] = b
	delete(r.files, oldName)
	return nil
}

func (r *fakeRoot) Close() error { return nil }

type fakeRuntime struct {
	mu         sync.Mutex
	calls      []string
	failLaunch bool
	// launchMounts holds the §5.C mount specs the LAST Launch carried, so a test can
	// assert what actually crossed the seam (the bind destination and the read-only
	// bit) rather than trusting that the mediator assembled it.
	launchMounts []runtime.MountSource
	// failStop / failDestroy fail the TEARDOWN half of the seam. The mediator
	// deliberately ignores these errors (`_ =` at every call site), so the property
	// they let a test pin is that the control-plane transition still lands correctly
	// when placement teardown fails (§4.4-A: byte/placement reclamation is decoupled
	// from the state transition).
	failStop    bool
	failDestroy bool
	// ruleHits / ruleHitsErr back RuleHits (§6.1's per-entry counters). A nil map with a
	// nil error is a legitimate "no counters", distinct from ruleHitsErr's "unreadable" —
	// the two must stay separable, since §6.1 omits the key for both but a test asserting
	// the omission rule needs to drive each independently.
	ruleHits    map[string]uint64
	ruleHitsErr error
	// launchGate, if non-nil, blocks Launch until closed (to hold the lock).
	launchGate chan struct{}
	seq        int
	// adoptAlive drives fakeRuntime.Adopt per-sandbox (§4.4-A liveness).
	adoptAlive map[string]bool
	// adoptErr drives a per-sandbox Adopt PROBE error (uncertain → mediator
	// protects the placement, skip-and-log; §4.4-A).
	adoptErr map[string]error
	// adoptCause drives the per-sandbox DEATH CAUSE Adopt hands out with an
	// alive=false verdict (§4.3): it is what separates dead-recoverable from
	// resource-terminated. Absent → CauseUnknown, itself a legal verdict.
	adoptCause map[string]runtime.DeathCause
	// launchLimits records the ResourceLimits each Launch was called with, so tests
	// can assert the §3.4 Binding is re-materialized at every placement from the
	// record rather than read back from a frozen value.
	launchLimits []runtime.ResourceLimits
	// launchSteering holds the §5.D steering spec the LAST Launch carried. The
	// network declaration is a Binding too (§3.4), so the same property applies: it
	// must be re-derived from the current record at every placement, not carried over.
	launchSteering runtime.SteeringSpec
	// reinstalled records the specs ReinstallRuleSet received (§4.4-A: adopting a
	// survivor must rebuild its rule set, which is crash-surviving host state).
	reinstalled []runtime.SteeringSpec
	// failReinstall fails the adopt-side rule-set install. The mediator must treat
	// that as UNCERTAINTY and protect the placement rather than demote — tearing down
	// a provably-live container is irreversible (§4.4-A).
	failReinstall bool
	// secondHopUID records what InstallSecondHop was handed, so a test can assert the
	// engine-level table is installed once at startup with the deployment's uid.
	secondHopUID *int
	// publishBound is the per-placement endpoint binding, keyed on `{guest_port, proto}`
	// like the real provider's (see bindPublish). Keyed by placement pointer because an
	// endpoint is a PLACEMENT product: a new placement binds afresh, the same one reuses.
	publishBound map[*runtime.Placement]map[runtime.PublishPort]string
	pubSeq       int
}

func (f *fakeRuntime) log(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}
func (f *fakeRuntime) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRuntime) Name() string { return "fake_runsc" }

// LastMounts returns the mount specs the most recent Launch received.
func (f *fakeRuntime) LastMounts() []runtime.MountSource {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.MountSource(nil), f.launchMounts...)
}
func (f *fakeRuntime) Launch(ctx context.Context, spec runtime.LaunchSpec) (*runtime.Placement, error) {
	// The call log keeps the bare "Launch:<id>" form — several tests match it
	// EXACTLY as a synchronization predicate. The limits are asserted from
	// launchLimits instead of by string-matching the log.
	f.log("Launch:" + spec.Sandbox)
	f.mu.Lock()
	f.launchLimits = append(f.launchLimits, spec.Limits)
	f.launchSteering = spec.Steering
	// The §5.C mount specs from the LAST launch, asserted from LastMounts.
	f.launchMounts = append([]runtime.MountSource(nil), spec.Mounts...)
	f.mu.Unlock()
	if f.launchGate != nil {
		select {
		case <-f.launchGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.failLaunch {
		return nil, fmt.Errorf("injected launch failure")
	}
	f.mu.Lock()
	f.seq++
	ep := fmt.Sprintf("10.0.0.%d:9000", f.seq)
	f.mu.Unlock()
	pl := &runtime.Placement{TransportEndpoint: ep}
	// Launch binds the publish endpoints too, like the real provider — they are a
	// PLACEMENT product (§3.3), so they appear with the placement rather than only when
	// a later hot-add reinstalls the rule set. Without this the fake reports no endpoint
	// for a publish that was already in the record at launch, which is the shape a
	// resume takes.
	pl.PublishEndpoints = f.bindPublish(pl, spec.Steering)
	return pl, nil
}
func (f *fakeRuntime) Stop(_ context.Context, _ *runtime.Placement) error {
	f.log("Stop")
	if f.failStop {
		return fmt.Errorf("injected Stop failure")
	}
	return nil
}
func (f *fakeRuntime) Destroy(_ context.Context, _ *runtime.Placement) error {
	f.log("Destroy")
	if f.failDestroy {
		return fmt.Errorf("injected Destroy failure")
	}
	return nil
}

// Adopt/ReclaimOrphans back the §4.4-A reconciliation path. The fake is driven
// by adoptAlive (per-sandbox liveness the test sets up) and records reclaim
// calls so the reconciliation orchestration can be tested as pure logic.
func (f *fakeRuntime) Adopt(_ context.Context, sandbox string) (*runtime.Placement, bool, runtime.DeathCause, error) {
	f.log("Adopt:" + sandbox)
	f.mu.Lock()
	alive := f.adoptAlive[sandbox]
	err := f.adoptErr[sandbox]
	cause := f.adoptCause[sandbox] // zero value = CauseUnknown, a legal verdict
	f.seq++
	ep := fmt.Sprintf("10.9.9.%d:9000", f.seq)
	f.mu.Unlock()
	if err != nil {
		return nil, false, runtime.CauseUnknown, err
	}
	if !alive {
		// dead → hand out the cause with the verdict, the real provider's contract
		// (§4.3: read it while the placement is still intact, there is no later).
		return nil, false, cause, nil
	}
	return &runtime.Placement{TransportEndpoint: ep}, true, runtime.CauseUnknown, nil
}
func (f *fakeRuntime) ReclaimOrphans(_ context.Context, keep []string) error {
	f.log("ReclaimOrphans:keep=" + strings.Join(append([]string(nil), keep...), "|"))
	return nil
}

// ReinstallRuleSet / InstallSecondHop are the §5.D enforcement face of the
// provider seam (§2.4). The fake records the calls and the rule counts, so the
// mediator's ORCHESTRATION of them can be tested as pure logic — the rendering and
// the kernel install are local_runsc's business and are covered on the real
// substrate.
//
// The reinstall log line carries the rule COUNT rather than just the sandbox id,
// because the property the mediator owes here is that Adopt re-installs a rule set
// generated from the CURRENT record — an adopt that installed an empty one would
// leave a survivor unenforced with every other adopt assertion still passing.
func (f *fakeRuntime) ReinstallRuleSet(_ context.Context, pl *runtime.Placement, spec runtime.SteeringSpec) error {
	f.log(fmt.Sprintf("ReinstallRuleSet:rules=%d", len(spec.Rules.Rules)))
	f.mu.Lock()
	f.reinstalled = append(f.reinstalled, spec)
	f.mu.Unlock()
	if f.failReinstall {
		return fmt.Errorf("injected ReinstallRuleSet failure")
	}
	if pl != nil {
		pl.PublishEndpoints = f.bindPublish(pl, spec)
	}
	return nil
}

// bindPublish models the provider's endpoint binding closely enough for the mediator's
// orchestration to be tested: an endpoint is drawn ONCE per `{guest_port, proto}` per
// placement and REUSED afterwards, and the list comes back in declaration order.
//
// Both properties are contract: §6.1 makes a repeated publish idempotent ("return the
// existing one"), so a fake that re-drew on every reinstall would answer a second
// identical POST with a different endpoint and let a mediator bug through. The pair is
// the key because tcp/8080 and udp/8080 are two publishes (§5.D).
func (f *fakeRuntime) bindPublish(pl *runtime.Placement, spec runtime.SteeringSpec) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishBound == nil {
		f.publishBound = map[*runtime.Placement]map[runtime.PublishPort]string{}
	}
	bound := f.publishBound[pl]
	if bound == nil {
		bound = map[runtime.PublishPort]string{}
		f.publishBound[pl] = bound
	}
	out := make([]string, 0, len(spec.PublishPorts))
	for _, pp := range spec.PublishPorts {
		ep, ok := bound[pp]
		if !ok {
			f.pubSeq++
			ep = fmt.Sprintf("127.0.0.1:%d", 30000+f.pubSeq)
			bound[pp] = ep
		}
		out = append(out, ep)
	}
	return out
}

func (f *fakeRuntime) InstallSecondHop(_ context.Context, rs ruleset.RuleSet, uid *int) error {
	got := "nil"
	if uid != nil {
		got = fmt.Sprintf("%d", *uid)
	}
	f.log(fmt.Sprintf("InstallSecondHop:uid=%s:rules=%d", got, len(rs.Rules)))
	f.mu.Lock()
	f.secondHopUID = uid
	f.mu.Unlock()
	return nil
}

// Reinstalled returns the steering specs ReinstallRuleSet received.
func (f *fakeRuntime) Reinstalled() []runtime.SteeringSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.SteeringSpec(nil), f.reinstalled...)
}

// LastSteering returns the steering spec the most recent Launch received — the
// mediator's obligation is to derive it from the CURRENT record at every placement
// (§3.4 Binding), so tests assert on it rather than on the log line.
func (f *fakeRuntime) LastSteering() runtime.SteeringSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launchSteering
}

// fakeClient satisfies transport.Client; only Health matters for the mediator.
// idle is the sandbox-global idle duration a successful Health reports back
// (reaper-health D2 activity signal) — the classifier reads it to tell healthy
// from idle. Zero unless a test sets it. idleFn, if set, overrides idle and is
// called per probe — used to simulate a sandbox that becomes active BETWEEN the
// sweep classify and the lock-held re-read (the D3 activity-race guard test).
type fakeClient struct {
	healthy   bool
	idle      time.Duration
	idleFn    func() time.Duration
	healthyFn func() bool // if set, overrides healthy per probe (D4b escalation test)
}

func (c *fakeClient) Health(context.Context) (transport.HealthStatus, error) {
	healthy := c.healthy
	if c.healthyFn != nil {
		healthy = c.healthyFn()
	}
	if !healthy {
		return transport.HealthStatus{}, fmt.Errorf("not healthy")
	}
	idle := c.idle
	if c.idleFn != nil {
		idle = c.idleFn()
	}
	return transport.HealthStatus{Idle: idle}, nil
}

// RecordActivity is present to satisfy transport.Client. The mediator never calls
// it — the write half of the activity signal belongs to §6.1's activity endpoint,
// and the classifier reading it here must never stamp it (reaper-health D3).
func (c *fakeClient) RecordActivity(context.Context) (transport.HealthStatus, error) {
	return transport.HealthStatus{}, nil
}
func (c *fakeClient) Start(context.Context, transport.StartSpec) (<-chan transport.ProcessEvent, error) {
	return nil, nil
}
func (c *fakeClient) Connect(context.Context, int) (<-chan transport.ProcessEvent, error) {
	return nil, nil
}
func (c *fakeClient) StreamInput(context.Context, int) (transport.InputWriter, error) {
	return nil, nil
}
func (c *fakeClient) SendInput(context.Context, int, []byte, bool) error   { return nil }
func (c *fakeClient) SendSignal(context.Context, int, int) error           { return nil }
func (c *fakeClient) Update(context.Context, int, transport.PtySize) error { return nil }
func (c *fakeClient) CloseStdin(context.Context, int) error                { return nil }
func (c *fakeClient) List(context.Context) ([]int, error)                  { return nil, nil }
func (c *fakeClient) Read(context.Context, string, int64, int64) ([]byte, error) {
	return nil, nil
}
func (c *fakeClient) Write(context.Context, string, []byte) error { return nil }

// The fs metadata surface is inert here: these unit tests exercise the §4 mediator,
// which never calls fs — only the reaper's Health probe and the cold-start gate
// matter. Present to satisfy transport.Client (and so a signature change breaks
// the build rather than drifting silently).
func (c *fakeClient) Stat(context.Context, string) (transport.EntryInfo, error) {
	return transport.EntryInfo{}, nil
}
func (c *fakeClient) ListDir(context.Context, string, uint32) ([]transport.EntryInfo, error) {
	return nil, nil
}
func (c *fakeClient) MakeDir(context.Context, string) (transport.EntryInfo, error) {
	return transport.EntryInfo{}, nil
}
func (c *fakeClient) Remove(context.Context, string) error { return nil }
func (c *fakeClient) Move(context.Context, string, string) (transport.EntryInfo, error) {
	return transport.EntryInfo{}, nil
}
func (c *fakeClient) OpenRead(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (c *fakeClient) WriteStream(_ context.Context, _ string, src io.Reader) (int64, error) {
	// Drain src so a caller that streams into this fake sees its reader consumed,
	// as the real client would.
	return io.Copy(io.Discard, src)
}

// RuleHits backs §6.1's per-entry hit counters. The fake returns whatever the test
// planted, and nil means "unreadable" — which is a distinct case from an empty map (see
// runtime.Provider.RuleHits), so tests can exercise the omission rule.
func (f *fakeRuntime) RuleHits(context.Context, *runtime.Placement) (map[string]uint64, error) {
	f.log("RuleHits")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ruleHitsErr != nil {
		return nil, f.ruleHitsErr
	}
	return f.ruleHits, nil
}
