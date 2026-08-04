package lifecycle

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"engine/internal/model"
)

// Nothing in flight is the common case, and it must not cost the pause anything —
// a drain that slept even briefly on every idle sandbox would make every pause
// slower for no benefit.
func TestBarrierDrainReturnsImmediatelyWhenIdle(t *testing.T) {
	b := newBarrier()
	start := time.Now()
	if stragglers := b.drain("sbx_idle", time.Hour); len(stragglers) != 0 {
		t.Errorf("idle drain reported stragglers: %v", stragglers)
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("idle drain took %s; must return immediately with nothing in flight", el)
	}
}

// The barrier's core job: a pause waits for an in-flight write to finish.
func TestBarrierDrainWaitsForInFlight(t *testing.T) {
	b := newBarrier()
	const id model.SandboxID = "sbx_busy"

	release, ok := b.enter(id, "fs.Write /root/x")
	if !ok {
		t.Fatal("enter refused on an idle barrier")
	}

	released := make(chan struct{})
	go func() {
		// Hold the operation briefly, then finish it. The drain must observe the
		// release rather than time out.
		time.Sleep(150 * time.Millisecond)
		close(released)
		release()
	}()

	start := time.Now()
	stragglers := b.drain(id, 10*time.Second)
	el := time.Since(start)

	select {
	case <-released:
	default:
		t.Fatal("drain returned BEFORE the in-flight operation released — the write was not waited for")
	}
	if len(stragglers) != 0 {
		t.Errorf("clean drain reported stragglers: %v", stragglers)
	}
	if el < 100*time.Millisecond {
		t.Errorf("drain returned after %s; it should have waited ~150ms for the operation", el)
	}
}

// A stuck operation must not make a sandbox un-pausable: §4.4 puts the drain on the
// "optimization" side, so the timeout proceeds and only reports what it left behind.
func TestBarrierDrainTimesOutAndNamesStragglers(t *testing.T) {
	b := newBarrier()
	const id model.SandboxID = "sbx_stuck"

	release, _ := b.enter(id, "fs.Write /root/stuck")
	defer release() // never released before the drain: that is the point

	start := time.Now()
	stragglers := b.drain(id, 200*time.Millisecond)
	el := time.Since(start)

	if len(stragglers) != 1 {
		t.Fatalf("timeout reported %d stragglers; want 1: %v", len(stragglers), stragglers)
	}
	// Naming the straggler is the whole diagnostic value — "1 operation outstanding"
	// would send the reader nowhere.
	if !strings.Contains(stragglers[0], "/root/stuck") {
		t.Errorf("straggler %q does not name the operation that blocked the drain", stragglers[0])
	}
	if el < 150*time.Millisecond {
		t.Errorf("drain gave up after %s; want ~200ms (it must actually wait)", el)
	}
	if el > 3*time.Second {
		t.Errorf("drain took %s; the grace must bound it", el)
	}
}

// Once draining, new writes are REFUSED rather than queued. Queueing would starve
// the drain: the thing it waits for would be arriving work.
func TestBarrierRefusesNewOpsWhileDraining(t *testing.T) {
	b := newBarrier()
	const id model.SandboxID = "sbx_draining"

	b.drain(id, time.Millisecond) // marks draining; nothing in flight
	if _, ok := b.enter(id, "fs.Write /root/late"); ok {
		t.Error("enter succeeded while draining — a pause could be starved by arriving writes")
	}

	// undrain re-opens the gate, or a sandbox would refuse writes forever after one
	// pause.
	b.undrain(id)
	if _, ok := b.enter(id, "fs.Write /root/after"); !ok {
		t.Error("enter still refused after undrain — the sandbox is wedged")
	}
}

// The barrier is per-sandbox: pausing one must not block writes to another. If it
// did, one paused sandbox would stall the whole node.
func TestBarrierIsPerSandbox(t *testing.T) {
	b := newBarrier()
	b.drain("sbx_a", time.Millisecond)
	defer b.undrain("sbx_a")

	if _, ok := b.enter("sbx_b", "fs.Write /root/x"); !ok {
		t.Error("draining sbx_a refused a write to sbx_b — the barrier must be per-sandbox")
	}
}

// Concurrency safety under -race: many operations entering and releasing while
// drains run. Asserts no deadlock and no lost wakeup (every drain returns).
func TestBarrierConcurrentEnterRelease(t *testing.T) {
	b := newBarrier()
	const id model.SandboxID = "sbx_race"

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, ok := b.enter(id, "fs.Write /root/x"); ok {
				time.Sleep(time.Millisecond)
				release()
			}
		}()
	}
	// Drains racing the operations. Each must return within its grace.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := make(chan struct{})
			go func() {
				b.drain(id, 2*time.Second)
				b.undrain(id)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("drain never returned — lost wakeup or deadlock")
			}
		}()
	}
	wg.Wait()

	// Every operation released, so the sandbox's entry is gone entirely (the map
	// deletes empty sets, which is what keeps it from growing per-write).
	b.mu.Lock()
	remaining := len(b.inFlight[id])
	b.mu.Unlock()
	if remaining != 0 {
		t.Errorf("%d operations still registered after all released", remaining)
	}
}

// --- reclaim invariant (barrier's doc comment) --------------------------------
//
// Both barrier maps must return to EMPTY, because both are keyed by a SandboxID
// the caller mints without bound: an entry that outlives its operation is a leak
// proportional to sandbox churn, and nothing else in the package would notice one.
// The three tests below pin the invariant from both ends — the runtime behavior
// (churn over distinct ids leaves nothing behind, on the failure path too) and the
// call-site discipline that behavior depends on (every Enter's release is
// deferred).

// barrierLens reports the live entry counts of both maps.
func barrierLens(b *barrier) (inFlight, draining int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.inFlight), len(b.draining)
}

// Collection under churn: many DISTINCT SandboxIDs each driven through the full
// cycle (enter → release → drain → undrain) must leave both maps empty.
//
// Distinct ids are what make this sharp. Residue is then exactly the number of ids
// whose entry outlived its operation, so a single missed delete shows as a count
// proportional to the churn rather than as a race that may not trigger. Reusing
// one id would hide a leak entirely: the map would hold one entry either way.
//
// The two maps fail differently and both are checked. An inFlight leak grows the
// operation set per sandbox; a draining leak ALSO wedges the sandbox — a stranded
// draining entry makes enter refuse that sandbox's mutating RPCs forever, so it is
// a permanent functional break, not merely memory.
func TestBarrierMapsCollectAfterChurn(t *testing.T) {
	b := newBarrier()
	const sandboxes = 1000

	for i := 0; i < sandboxes; i++ {
		id := model.SandboxID("sbx_churn_" + strconv.Itoa(i))
		release, ok := b.enter(id, "fs.Write /root/x")
		if !ok {
			t.Fatalf("enter refused for a fresh sandbox %s", id)
		}
		release()
		// A pause over the same id, so the draining half of the cycle is exercised
		// too — and it must reclaim even though drain itself only ever adds.
		if stragglers := b.drain(id, time.Second); len(stragglers) != 0 {
			t.Fatalf("drain after release reported stragglers for %s: %v", id, stragglers)
		}
		b.undrain(id)
	}

	inFlight, draining := barrierLens(b)
	if inFlight != 0 {
		t.Errorf("inFlight holds %d entries after %d churned sandboxes; want 0 "+
			"(an entry outliving its operation is a leak keyed by caller-supplied "+
			"SandboxID — some release path stopped deleting the empty set)",
			inFlight, sandboxes)
	}
	if draining != 0 {
		t.Errorf("draining holds %d entries after %d churned sandboxes; want 0 "+
			"(a stranded draining entry refuses that sandbox's mutating RPCs FOREVER, "+
			"not just leaks memory)", draining, sandboxes)
	}
}

// Reclamation must not depend on the happy path. Two cases where the operation
// does NOT end with a clean drain:
//
//   - a drain that TIMES OUT still holds the entry (correctly — the op is live),
//     and the late release must then still collect it. Otherwise every pause that
//     hit its grace would leave permanent residue, i.e. the leak would appear
//     exactly under the load that causes timeouts.
//   - a REFUSED enter must not create anything. It returns (nil, false), so there
//     is no release closure to call and an entry made before the refusal check
//     could never be reclaimed by anyone.
func TestBarrierReclaimsOnTimeoutAndRefusalPaths(t *testing.T) {
	b := newBarrier()
	const id model.SandboxID = "sbx_late"

	release, ok := b.enter(id, "fs.Write /root/slow")
	if !ok {
		t.Fatal("enter refused on an idle barrier")
	}
	if stragglers := b.drain(id, 50*time.Millisecond); len(stragglers) != 1 {
		t.Fatalf("drain reported %d stragglers (%v); want 1 (the op is deliberately still held)",
			len(stragglers), stragglers)
	}

	// Refused while draining: no entry may be created for the refused op.
	if _, ok := b.enter(id, "fs.Write /root/refused"); ok {
		t.Fatal("enter succeeded while draining")
	}
	if n := len(b.inFlight[id]); n != 1 {
		t.Errorf("inFlight[%s] has %d ops after a REFUSED enter; want 1 "+
			"(a refused enter must register nothing — it hands back no release closure, "+
			"so anything it created would be unreclaimable)", id, n)
	}

	// The straggler finally finishes, after the drain already gave up.
	release()
	b.undrain(id)

	inFlight, draining := barrierLens(b)
	if inFlight != 0 || draining != 0 {
		t.Errorf("after a timed-out drain and a late release: inFlight=%d draining=%d; want 0/0 "+
			"(the timeout path is the one that runs under load — residue here scales with "+
			"exactly the traffic that causes timeouts)", inFlight, draining)
	}
}

// STRUCTURAL guard: every production acquirer of a barrier entry must release it
// with a defer.
//
// This is the assertion the lane exists for. The reclaim invariant above is a
// property of barrier.go, but it is only REACHED if callers run the closure enter
// hands back; a caller that drops it strands an entry forever — which is the leak
// the register row wrongly attributed to barrier.go itself. Nothing else catches
// that: the closure is legitimately ignorable in Go (bind it, never call it, and
// the build is clean), the stranded entry is invisible until it accumulates, and
// the runtime tests above drive the barrier directly, so they cannot see a call
// site at all.
//
// A source scan rather than a runtime check, because the failure is a MISSING
// statement — there is no execution in which "the defer nobody wrote" can be
// observed. Scanning also covers the SIXTH call site the day it is added, with
// nobody remembering to extend a test.
//
// Two acquirer shapes exist, and the second is why this is not a one-liner:
//
//  1. DIRECT — `release, ok := ...Enter(...)`, then guard ok, then defer release.
//     files.go's streaming upload.
//  2. HANDED OFF — a helper returns the release to its caller, which owns the
//     defer. dataplane.go's mutatingClientFor: one barrier acquisition shared by
//     the four mutating fs RPCs. Its own body deliberately does NOT defer (that
//     would release before the write it is protecting even starts), so requiring a
//     defer at the Enter statement would be wrong; the obligation moves to each
//     caller of the helper, and this guard follows it there.
//
// For both shapes the required shape is the same: guard, then `defer release()` as
// the next statement. Deliberately narrow — a release run conditionally, or parked
// at the end of a long body, is exactly what survives the happy path and strands
// the entry on an early return. A future acquirer that genuinely cannot defer (a
// release outliving its function, say handed to a stream) is a real design
// decision: record it here as an exemption with its reason, the way
// runtime/seam_test.go's fieldExemptions do.
func TestEnterCallSitesDeferRelease(t *testing.T) {
	files := goProductionFiles(t, "..")

	// Pass 1: find the helpers that hand a barrier release to their caller, so pass
	// 2 can hold their callers to the same obligation as a direct acquirer.
	handoffs := map[string]token.Position{}
	for path, f := range files {
		for _, decl := range f.decls {
			if decl.Body == nil {
				continue
			}
			var acquires, returns bool
			ast.Inspect(decl.Body, func(n ast.Node) bool {
				if name, ok := enterReleaseName(n); ok {
					acquires = true
					// Returning the closure by name is the handoff. (An Enter whose result
					// is deferred locally is shape 1 and handled in pass 2.)
					ast.Inspect(decl.Body, func(m ast.Node) bool {
						ret, ok := m.(*ast.ReturnStmt)
						if !ok {
							return true
						}
						for _, res := range ret.Results {
							if id, ok := res.(*ast.Ident); ok && id.Name == name {
								returns = true
							}
						}
						return true
					})
				}
				return true
			})
			if acquires && returns {
				handoffs[decl.Name.Name] = f.fset.Position(decl.Pos())
				_ = path
			}
		}
	}

	// Pass 2: every acquisition — direct or via a handoff helper — must be
	// ok-guarded and then deferred.
	var checked int
	for _, f := range files {
		for _, decl := range f.decls {
			if decl.Body == nil {
				continue
			}
			// A handoff helper's OWN body is exempt by construction: it must not release
			// what it is about to return.
			if _, isHandoff := handoffs[decl.Name.Name]; isHandoff {
				continue
			}
			ast.Inspect(decl.Body, func(n ast.Node) bool {
				blk, ok := n.(*ast.BlockStmt)
				if !ok {
					return true
				}
				for i, st := range blk.List {
					name, how, ok := releaseAcquisition(st, handoffs)
					if !ok {
						continue
					}
					checked++
					pos := f.fset.Position(st.Pos())
					// Contract: guard, then hand the entry back on EVERY exit from here.
					if i+2 >= len(blk.List) {
						t.Errorf("%s: barrier release %q (%s) is not followed by an ok/err guard plus a deferred release",
							pos, name, how)
						continue
					}
					if _, isIf := blk.List[i+1].(*ast.IfStmt); !isIf {
						t.Errorf("%s: barrier release %q (%s) is not immediately guarded — a refused "+
							"enter must reject the request, never fall through as though it held an entry",
							pos, name, how)
						continue
					}
					def, isDefer := blk.List[i+2].(*ast.DeferStmt)
					if !isDefer || !callsIdent(def.Call, name) {
						t.Errorf("%s: barrier release %q (%s) is not handed back with `defer %s()` on the "+
							"statement after its guard — an unreleased entry is a PERMANENT leak in the "+
							"barrier's inFlight map, keyed by a caller-supplied SandboxID, and no other "+
							"test in this repo would fail (see barrier's doc comment for the reclaim "+
							"invariant)", pos, name, how, name)
					}
				}
				return true
			})
		}
	}

	// Non-vacuity, both halves. A scan that matched nothing would pass forever —
	// including after the seam is renamed or the result bound in a shape this scan
	// does not recognize. Five production acquisitions today: the streaming upload
	// directly, plus the four mutating fs RPCs through mutatingClientFor. The floors
	// only have to prove the scan still bites.
	if checked == 0 {
		t.Fatal("found no barrier release acquisitions to check — the guard has gone blind " +
			"(renamed seam, or the result is now bound in a shape this scan does not match)")
	}
	if len(handoffs) == 0 {
		t.Error("found no helper that hands a barrier release to its caller — mutatingClientFor " +
			"was this shape; if it is gone the handoff half of this guard is now dead code, and " +
			"if it was merely renamed the scan is silently missing its four callers")
	}
}

// parsedFile is one production .go file's AST plus its position info.
type parsedFile struct {
	fset  *token.FileSet
	decls []*ast.FuncDecl
}

// goProductionFiles parses every non-test .go file under root. Test files are
// excluded because a test may hold a release deliberately — the drain-timeout
// cases above exist to keep one outstanding — so the discipline is a production
// one.
func goProductionFiles(t *testing.T, root string) map[string]*parsedFile {
	t.Helper()
	out := map[string]*parsedFile{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		pf := &parsedFile{fset: fset}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				pf.decls = append(pf.decls, fn)
			}
		}
		out[path] = pf
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// releaseAcquisition reports the identifier a statement binds a barrier release
// to, and how it was acquired ("Enter" directly, or via a named handoff helper).
func releaseAcquisition(stmt ast.Stmt, handoffs map[string]token.Position) (name, how string, ok bool) {
	if n, isEnter := enterReleaseName(stmt); isEnter {
		return n, "direct Enter", true
	}
	as, isAssign := stmt.(*ast.AssignStmt)
	if !isAssign || len(as.Rhs) != 1 {
		return "", "", false
	}
	call, isCall := as.Rhs[0].(*ast.CallExpr)
	if !isCall {
		return "", "", false
	}
	fn := calleeName(call)
	if _, isHandoff := handoffs[fn]; !isHandoff {
		return "", "", false
	}
	// The release is whichever result is a func() — by position, matching the
	// helper's signature. Identify it as the lone non-error, non-client ident: in
	// practice the middle result. Scan all idents so a reordered signature still
	// finds it.
	for _, lhs := range as.Lhs {
		id, isIdent := lhs.(*ast.Ident)
		if !isIdent {
			continue
		}
		if strings.Contains(strings.ToLower(id.Name), "release") {
			return id.Name, "via " + fn, true
		}
	}
	return "", "", false
}

// enterReleaseName reports the identifier a `release, ok := ...Enter(...)` node
// binds the release closure to, if n is such an assignment.
func enterReleaseName(n ast.Node) (string, bool) {
	as, ok := n.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 || len(as.Lhs) != 2 {
		return "", false
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Enter" {
		return "", false
	}
	id, ok := as.Lhs[0].(*ast.Ident)
	if !ok || id.Name == "_" {
		return "", false
	}
	return id.Name, true
}

// calleeName renders the called function's bare name, method or plain func.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// callsIdent reports whether call invokes the plain identifier name.
func callsIdent(call *ast.CallExpr, name string) bool {
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == name
}
