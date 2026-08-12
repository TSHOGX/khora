package lifecycle

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TSHOGX/warren/internal/model"
	"github.com/TSHOGX/warren/internal/store"
)

// patchBinding drives the real PATCH verb with a Binding-only spec — what
// `PATCH /v1/sandboxes/{id}` carries when the request names no `volumes`.
//
// It goes THROUGH Rebind rather than around it because there is no binding-only write
// point any more: §4.4-B requires one PATCH to be one acquisition of the transition
// lock, so the three former entry points (UpdateBinding / SetNetwork / RebindVolumes)
// were deleted rather than kept as wrappers. This helper is a test-side spelling
// convenience, not a surviving seam.
func patchBinding(m *Mediator, ctx context.Context, id model.SandboxID, b store.Binding) error {
	_, err := m.Rebind(ctx, id, RebindSpec{Binding: b})
	return err
}

// patchMounts drives the same verb with a mounts-only spec. It always passes a
// non-nil pointer, so a nil slice means DETACH EVERYTHING — which is what §6.2 says
// `"volumes": []` means, and the distinction the pointer exists to carry.
func patchMounts(m *Mediator, ctx context.Context, id model.SandboxID, mounts []model.Mount) (bool, error) {
	return m.Rebind(ctx, id, RebindSpec{Mounts: &mounts})
}

// TestRebind4xxCommitsNothing is the core assertion for the defect that made Rebind one
// verb: a PATCH that answers 4xx left the record EXACTLY as it was.
//
// The shape is what makes it sharp, and it is not the shape an ordering-only test has.
// Every case pairs a LEGAL Binding field with a mount binding that the STORE refuses at
// commit time — a contended exclusive lease (409) or a self-contradictory binding (400).
// Those two verdicts cannot be hoisted into a pre-check by contract
// (store.Store.CreateMounts: judgment and insert are inseparable, measured), so they are
// exactly the refusals that arrive AFTER the request has been accepted as well-formed.
//
// With the old three-call shape the cap or TTL was already committed by the time the
// mount commit lost, so the caller received a 4xx describing a request that had half
// happened. Reading the record back is the only witness — the status code alone looks
// identical either way.
func TestRebind4xxCommitsNothing(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		// bad builds the mount binding that must lose, given the two volumes the fixture
		// created (`held` is exclusively held by ANOTHER sandbox).
		bad  func(mine, held model.VolumeID) []model.Mount
		want error
	}{
		{
			// 409: the lease is arbitrated by the store, at commit.
			name: "contended exclusive lease",
			bad: func(mine, held model.VolumeID) []model.Mount {
				return []model.Mount{
					{Volume: mine, Path: "/data", Access: model.Exclusive},
					{Volume: held, Path: "/other", Access: model.Exclusive},
				}
			},
			want: store.ErrMountLeaseConflict,
		},
		{
			// 400: the submission contradicts itself (same destination twice).
			name: "self-contradictory binding",
			bad: func(mine, held model.VolumeID) []model.Mount {
				return []model.Mount{
					{Volume: mine, Path: "/dup", Access: model.SharedReadWrite},
					{Volume: held, Path: "/dup", Access: model.SharedReadWrite},
				}
			},
			want: store.ErrMountBindingInvalid,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, st, _ := volFixture(t)
			// PAUSED, so the cap edit below is legal on its own terms (§3.4). Without that
			// the caps write would be refused by the STATE gate and the test would pass for
			// the wrong reason — it would never reach the mount commit at all.
			seedRec(t, st, "sbx_p", "ov_p", model.Paused)
			seedRec(t, st, "sbx_holder", "ov_h", model.Running)

			mine, err := m.CreateVolume(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			held, err := m.CreateVolume(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.CreateMounts(ctx, []model.Mount{
				{Sandbox: "sbx_p", Volume: mine, Path: "/data", Access: model.Exclusive},
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateMounts(ctx, []model.Mount{
				{Sandbox: "sbx_holder", Volume: held, Path: "/x", Access: model.Exclusive},
			}); err != nil {
				t.Fatal(err)
			}
			// A starting memory cap, so the raise below is a real change with a known prior
			// value to still be holding afterwards.
			start := int64(128 << 20)
			if err := st.UpdateBinding(ctx, "sbx_p", model.Paused, store.Binding{MemoryByte: &start}, nil); err != nil {
				t.Fatal(err)
			}

			before, err := st.GetSandbox(ctx, "sbx_p")
			if err != nil {
				t.Fatal(err)
			}

			// The multi-field PATCH: a cap edit AND a TTL edit that would both be accepted
			// alone, riding with a mount binding the store must refuse.
			raise, ttl := int64(512<<20), 7*time.Minute
			mounts := tc.bad(mine, held)
			released, err := m.Rebind(ctx, "sbx_p", RebindSpec{
				Binding: store.Binding{MemoryByte: &raise, IdleTimeout: &ttl},
				Mounts:  &mounts,
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Rebind = %v; want %v (the store arbitrates this verdict at commit)", err, tc.want)
			}
			if released {
				t.Errorf("a REFUSED rebind reported a released lease — nothing was committed, so nothing was released")
			}

			after, err := st.GetSandbox(ctx, "sbx_p")
			if err != nil {
				t.Fatal(err)
			}
			if after.Limits != before.Limits {
				t.Errorf("limits = %+v after a refused PATCH; want %+v unchanged — the cap committed and "+
					"the mount binding was THEN refused, so the caller was told the request failed while "+
					"the record disagrees (§4.4-B: one PATCH is one lock and one decision)",
					after.Limits, before.Limits)
			}
			if after.IdleTimeout != before.IdleTimeout {
				t.Errorf("idle_timeout = %s after a refused PATCH; want %s unchanged", after.IdleTimeout, before.IdleTimeout)
			}
			if after.State != before.State {
				t.Errorf("state = %s after a refused PATCH; want %s (a refused PATCH is not a transition)", after.State, before.State)
			}
			if !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("UpdatedAt moved on a refused PATCH (%s → %s)", before.UpdatedAt, after.UpdatedAt)
			}
			// The mount table kept the old binding, which ReplaceMounts guarantees by being
			// one commit — asserted here too because "nothing changed" covers all four field
			// classes, not just the record row.
			rows, err := st.ListMounts(ctx, "sbx_p")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Volume != mine || rows[0].Path != "/data" {
				t.Errorf("mount rows = %+v after a refused PATCH; want the original exclusive /data mount", rows)
			}
		})
	}
}

// TestRebindStateGateIsTheUnionAndPrecedesEveryWrite: the gate over the field classes is
// evaluated IN FULL before any write, so a request whose state-refused class is checked
// LAST does not commit the classes checked earlier.
//
// THE MOUNT BINDING IS IN EVERY CASE, and that is what gives the test teeth. The record
// row is now a single conditional write, so a TTL riding with a refused cap cannot leak
// through it whatever the gate order is — a `{TTL, cap}` case passes against a per-class
// gate too, and would be a test that looks right and proves nothing. The MOUNT TABLE is
// the separate commit, and mounts are legal on `running` while caps and declarations are
// not: so `{volumes, cap}` and `{volumes, declaration}` on a `running` sandbox are exactly
// the requests a per-class gate serves half of — ReplaceMounts commits, then the later
// class's gate refuses, and the caller is told nothing happened.
//
// The declaration case is also the second-order divergence the merge closed: the old
// `UpdateBinding` gate barred caps on `running` but let a declaration through, while
// `SetNetwork` barred it — one field, two write points, two criteria. There is one gate
// over one spec now, so the two cannot drift.
func TestRebindStateGateIsTheUnionAndPrecedesEveryWrite(t *testing.T) {
	ctx := context.Background()
	cap512 := int64(512 << 20)
	allowAll := model.NetworkDecl{Egress: model.NetEgress{Default: model.NetAllow}}

	for _, tc := range []struct {
		name string
		bind store.Binding
	}{
		{name: "mounts + a cap edit", bind: store.Binding{MemoryByte: &cap512}},
		{name: "mounts + a declaration", bind: store.Binding{Network: &allowAll}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, st, _ := volFixture(t)
			// RUNNING: `volumes` is legal here, a cap and a declaration are not — which is
			// what makes the ordering observable at all.
			seedRec(t, st, "sbx_run", "ov_run", model.Running)
			vid, err := m.CreateVolume(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			other, err := m.CreateVolume(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.CreateMounts(ctx, []model.Mount{
				{Sandbox: "sbx_run", Volume: vid, Path: "/data", Access: model.Exclusive},
			}); err != nil {
				t.Fatal(err)
			}
			before, err := st.GetSandbox(ctx, "sbx_run")
			if err != nil {
				t.Fatal(err)
			}

			// A mount binding that WOULD be accepted alone (it detaches one volume and
			// attaches another, both uncontended), riding with a state-refused class.
			ttl := 9 * time.Minute
			bind := tc.bind
			bind.IdleTimeout = &ttl
			mounts := []model.Mount{{Volume: other, Path: "/other", Access: model.Exclusive}}
			released, err := m.Rebind(ctx, "sbx_run", RebindSpec{Binding: bind, Mounts: &mounts})
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("Rebind on `running` = %v; want ErrConflict (caps and the declaration are editable only from `paused`, §3.4/§5.D)", err)
			}
			if released {
				t.Errorf("a state-refused rebind reported a released lease — nothing was committed, so nothing was released")
			}

			// THE MOUNT TABLE, which is the separate commit and therefore the witness.
			rows, err := st.ListMounts(ctx, "sbx_run")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Volume != vid || rows[0].Path != "/data" {
				t.Errorf("mount rows = %+v after a state-refused PATCH; want the original exclusive "+
					"/data mount. The mount binding is legal on `running` on its own, so committing it "+
					"and THEN refusing a later class is the partial write §6.2 forbids — and it also "+
					"released an exclusive lease the caller never learned they gave up (§5.C: the "+
					"lease IS the row)", rows)
			}

			after, err := st.GetSandbox(ctx, "sbx_run")
			if err != nil {
				t.Fatal(err)
			}
			if after.IdleTimeout != before.IdleTimeout {
				t.Errorf("idle_timeout = %s after a state-refused PATCH; want %s unchanged", after.IdleTimeout, before.IdleTimeout)
			}
			if after.Limits != before.Limits {
				t.Errorf("limits = %+v; want %+v unchanged", after.Limits, before.Limits)
			}
			if after.Network.Egress.Default != before.Network.Egress.Default {
				t.Errorf("egress.default = %q; want %q unchanged", after.Network.Egress.Default, before.Network.Egress.Default)
			}
		})
	}
}

// TestRebindValidatesTheDeclarationBeforeTheStateGate: an INVALID declaration is refused
// as a 400 regardless of state — not answered with the 409 "pause first, then PATCH".
//
// The ordering matters to the caller, not just to the code: pausing cannot make an
// invalid declaration legal, so a 409 sends them to do work that changes nothing. It is
// the same reasoning that puts the disk `0 → N` refusal ahead of the admission gate.
//
// This also pins WHERE the validation lives: it is a write-point obligation on the
// mediator, not an API-layer one, so a caller reaching the mediator directly (the
// substrate suites do) cannot bypass §6.2's classes.
func TestRebindValidatesTheDeclarationBeforeTheStateGate(t *testing.T) {
	ctx := context.Background()
	m, st, _ := volFixture(t)
	// RUNNING: the state gate would refuse a declaration edit with ErrConflict, so this
	// is the state that tells the two orderings apart.
	seedRec(t, st, "sbx_run", "ov_run", model.Running)

	// §6.2's first class: `network` given with no `egress.default`.
	bad := model.NetworkDecl{}
	err := patchBinding(m, ctx, "sbx_run", store.Binding{Network: &bad})
	if !IsDeclarationInvalid(err) {
		t.Fatalf("Rebind with an invalid declaration = %v; want a §6.2 declaration refusal (400). A "+
			"state-gate 409 here would tell the caller to pause first for a request that pausing "+
			"cannot make legal", err)
	}
	if errors.Is(err, store.ErrConflict) {
		t.Errorf("the refusal is also an ErrConflict, so the API maps it to 409 rather than 400")
	}
}

// TestRebindMutualExclusionWithResumeAndEvict is §4.4-B's promise for PATCH, measured
// rather than assumed: a Rebind never interleaves with a resume or an evict.
//
// THE WITNESS IS THE STORE-CALL SEQUENCE, not wall-clock overlap of the mediator calls.
// That distinction is the whole test. Wrapping the verbs from outside would measure
// something the broken shape already satisfied — each of the three former calls WAS
// individually locked — so it would pass with the defect in place. What the defect
// actually produced is another verb's commit landing BETWEEN two store operations of one
// PATCH, in the window between call N's Unlock and call N+1's Lock.
//
// So the spy records one totally-ordered log of store operations tagged with the
// goroutine that made them, and the assertion replays it: for every PATCH whose sequence
// spans two or more operations, no OTHER goroutine's lifecycle-state commit may appear
// between its first and its last. Reconstructing after the fact rather than tracking a
// live span is deliberate — a span opened by a read and closed by a write leaks whenever
// the verb refuses in between (the state gate does exactly that on a `creating` record),
// and a leaked span reports interleavings that are really just its own bookkeeping.
func TestRebindMutualExclusionWithResumeAndEvict(t *testing.T) {
	st := store.NewMemory()
	seedBare(t, st)
	m, _, _ := stressMediator(st, true)
	spy := &interleaveSpy{Store: st}
	m.deps.Store = spy
	r := NewReaper(m, time.Hour, time.Hour)

	const (
		id      = model.SandboxID("sbx_excl")
		rounds  = 120
		patches = 8
	)
	seedPausedStore(t, st, string(id), "ov_excl", "bare")

	ctx := context.Background()
	ok := func(err error) bool {
		// Every legal outcome of racing verbs: the CAS loses, the record is gone (evicted),
		// or the state gate refuses because another verb moved the record first.
		return err == nil || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// The competing verbs: real resume / pause / destroy plus the reaper's tryLock traffic.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, step := range []func() error{
				func() error { return m.Resume(ctx, id) },
				func() error { return m.Pause(ctx, id) },
				func() error { return m.Destroy(ctx, id) },
			} {
				if err := step(); !ok(err) {
					t.Errorf("verb driver: %v", err)
					return
				}
			}
			// Re-seed so the next round has a record; a NotFound above is a legal
			// interleaving, not a failure.
			_ = st.CreateSandbox(ctx, model.Sandbox{
				ID: id, Template: "bare", Overlay: "ov_excl", State: model.Creating,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}, nil)
			_ = st.CompareAndSetState(ctx, id, model.Creating, model.Paused, nil, nil)
			r.Sweep(ctx)
		}
	}()

	// The PATCHes: a multi-field spec, so each performs its whole store sequence
	// (Get → ReplaceMounts → UpdateBinding) and the span being checked is a real one.
	var patchWG sync.WaitGroup
	for p := 0; p < patches; p++ {
		patchWG.Add(1)
		go func() {
			defer patchWG.Done()
			for i := 0; i < rounds; i++ {
				ttl := time.Duration(i+1) * time.Second
				var none []model.Mount
				if _, err := m.Rebind(ctx, id, RebindSpec{
					Binding: store.Binding{IdleTimeout: &ttl},
					Mounts:  &none,
				}); !ok(err) {
					t.Errorf("rebind: %v", err)
					return
				}
				runtime.Gosched()
			}
		}()
	}
	patchWG.Wait()
	close(stop)
	wg.Wait()

	spans, transitions, interleaved := spy.replay()
	if interleaved != 0 {
		t.Fatalf("%d transition commit(s) landed INSIDE a PATCH's store sequence — §4.4-B requires "+
			"PATCH to hold this sandbox's transition lock for its whole duration, so a resume or an "+
			"evict cannot interleave with it. This is the defect three separate mediator calls "+
			"(UpdateBinding / SetNetwork / RebindVolumes) produced: two windows between three lock "+
			"acquisitions, each wide enough for a whole verb", interleaved)
	}
	// Non-vacuity, both halves. Without multi-operation PATCH spans there is no window to
	// interleave WITH, and without competing commits there is nothing that could have
	// landed in one — either alone would make a zero above prove nothing.
	if spans == 0 {
		t.Fatal("no PATCH completed a multi-operation sequence — there was no window to interleave " +
			"with, so the zero above is vacuous")
	}
	if transitions == 0 {
		t.Fatal("no competing lifecycle-state commit landed during the run — the exclusion result is " +
			"vacuous (it would also hold if the verb driver never ran)")
	}
	if n := lockMapLen(m); n != 0 {
		t.Errorf("transition-lock map holds %d entries after the run; want 0 (Rebind took a ref and did not release it)", n)
	}
}

// interleaveSpy records a totally-ordered log of the store operations relevant to
// §4.4-B, tagged with the goroutine that made each one, so the exclusion property can be
// reconstructed after the run (see the test above for why post-hoc beats a live span).
type interleaveSpy struct {
	store.Store

	mu  sync.Mutex
	ops []spyOp
}

// spyOp is one store operation. `goid` distinguishes concurrent callers; `kind` says
// which operation it was, and PATCH operations are tagged as such by CALL STACK rather
// than by a flag the test sets, so a future edit that adds a store call to Rebind is
// covered automatically.
type spyOp struct {
	goid uint64
	kind spyKind
}

type spyKind int

const (
	// The three operations one PATCH performs, in this order (see Rebind: one read, then
	// mounts, then the binding).
	patchRead spyKind = iota
	patchMountWrite
	patchBindWrite
	// Any §4.1 lifecycle-state commit by any verb: CompareAndSetState or DeleteSandbox.
	stateCommit
)

func (s *interleaveSpy) record(k spyKind) {
	s.mu.Lock()
	s.ops = append(s.ops, spyOp{goid: goID(), kind: k})
	s.mu.Unlock()
}

// replay reconstructs the property: for every COMPLETED PATCH — one that reached its
// binding write, i.e. one that actually committed something — no other goroutine's
// lifecycle-state commit may appear between its opening read and that write.
//
// A span is delimited BY OPERATION KIND, not by adjacency in the log: `patchRead` opens
// it and `patchBindWrite` closes it, per goroutine. Two properties fall out, and both are
// needed. A PATCH that REFUSES never reaches the write, so it forms no span — correctly,
// since a refusal commits nothing and nothing can have interleaved with it; and its
// dangling open is simply replaced by the next call's read, so a refusal cannot leak a
// span that swallows the whole rest of the log. (Tracking a live span instead leaks
// exactly there: the state gate refuses every PATCH that finds the record `creating`.)
func (s *interleaveSpy) replay() (spans, transitions, interleaved int) {
	s.mu.Lock()
	ops := append([]spyOp(nil), s.ops...)
	s.mu.Unlock()

	open := map[uint64]int{} // goroutine → index of its most recent patchRead
	for i, op := range ops {
		switch op.kind {
		case stateCommit:
			transitions++
		case patchRead:
			open[op.goid] = i
		case patchBindWrite:
			start, ok := open[op.goid]
			if !ok {
				continue // no opening read recorded (cannot happen; not worth asserting on)
			}
			delete(open, op.goid)
			if i-start <= 1 {
				continue // adjacent: no window at all
			}
			spans++
			for j := start + 1; j < i; j++ {
				if ops[j].kind == stateCommit && ops[j].goid != op.goid {
					interleaved++
				}
			}
		}
	}
	return spans, transitions, interleaved
}

func (s *interleaveSpy) GetSandbox(ctx context.Context, id model.SandboxID) (model.Sandbox, error) {
	if calledFromRebind() {
		s.record(patchRead)
	}
	return s.Store.GetSandbox(ctx, id)
}

func (s *interleaveSpy) ReplaceMounts(ctx context.Context, id model.SandboxID, next []model.Mount) error {
	if calledFromRebind() {
		s.record(patchMountWrite)
	}
	return s.Store.ReplaceMounts(ctx, id, next)
}

func (s *interleaveSpy) UpdateBinding(ctx context.Context, id model.SandboxID, expected model.Lifecycle, b store.Binding, cause *model.DeathCause) error {
	if calledFromRebind() {
		s.record(patchBindWrite)
	}
	return s.Store.UpdateBinding(ctx, id, expected, b, cause)
}

// CompareAndSetState is every §4.1 transition's commit (resume, pause, auto-pause).
func (s *interleaveSpy) CompareAndSetState(ctx context.Context, id model.SandboxID, expected, next model.Lifecycle, ev *store.Event, cause *model.DeathCause) error {
	s.record(stateCommit)
	return s.Store.CompareAndSetState(ctx, id, expected, next, ev, cause)
}

// DeleteSandbox is destroy's and evict's commit — the 404-shaped half of the defect.
func (s *interleaveSpy) DeleteSandbox(ctx context.Context, id model.SandboxID, ev *store.Event) error {
	s.record(stateCommit)
	return s.Store.DeleteSandbox(ctx, id, ev)
}

// calledFromRebind reports whether Rebind is on the current call stack, so an operation is
// attributed to the PATCH verb and not to the many other callers of these methods (the
// reaper's sweep, reconcile, every other verb).
func calledFromRebind() bool {
	var pcs [32]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "(*Mediator).Rebind") {
			return true
		}
		if !more {
			return false
		}
	}
}

// goID extracts the current goroutine's id from its stack header. Ugly, and the honest
// alternative is worse: threading an id through the mediator would mean production code
// carrying a field that exists only for this test.
func goID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// TestRebindReleasedSemanticsUnchanged: `released` still means "a volume held
// EXCLUSIVELY before this rebind is not held exclusively now", which is the fact §6.1's
// `lease_released` reports and the one `applies_at` cannot express (the lease IS the row,
// so a detach releases immediately, while the guest topology waits for the next resume).
//
// All four transitions are asserted, because the flag is a comparison and a comparison
// has directions: dropping an exclusive mount releases, keeping it does not, adding one
// does not, and downgrading exclusive → shared does (the exclusive hold really is gone).
func TestRebindReleasedSemanticsUnchanged(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		beforeAcc  model.AccessMode
		next       func(v model.VolumeID) []model.Mount
		wantReleas bool
	}{
		{
			name:       "detaching an exclusive mount releases",
			beforeAcc:  model.Exclusive,
			next:       func(model.VolumeID) []model.Mount { return nil },
			wantReleas: true,
		},
		{
			name:      "keeping it does not",
			beforeAcc: model.Exclusive,
			next: func(v model.VolumeID) []model.Mount {
				return []model.Mount{{Volume: v, Path: "/data", Access: model.Exclusive}}
			},
			wantReleas: false,
		},
		{
			name:      "downgrading exclusive → shared releases",
			beforeAcc: model.Exclusive,
			next: func(v model.VolumeID) []model.Mount {
				return []model.Mount{{Volume: v, Path: "/data", Access: model.SharedReadWrite}}
			},
			wantReleas: true,
		},
		{
			name:       "detaching a SHARED mount releases nothing",
			beforeAcc:  model.SharedReadWrite,
			next:       func(model.VolumeID) []model.Mount { return nil },
			wantReleas: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, st, _ := volFixture(t)
			seedRec(t, st, "sbx_rel", "ov_rel", model.Running)
			vid, err := m.CreateVolume(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.CreateMounts(ctx, []model.Mount{
				{Sandbox: "sbx_rel", Volume: vid, Path: "/data", Access: tc.beforeAcc},
			}); err != nil {
				t.Fatal(err)
			}

			released, err := patchMounts(m, ctx, "sbx_rel", tc.next(vid))
			if err != nil {
				t.Fatalf("rebind: %v", err)
			}
			if released != tc.wantReleas {
				t.Errorf("released = %v; want %v — the flag is what lets a caller waiting to attach "+
					"the volume elsewhere proceed at once (§6.1), so a false positive sends them into a "+
					"409 and a false negative makes them wait for a resume that is not coming",
					released, tc.wantReleas)
			}
		})
	}
}
