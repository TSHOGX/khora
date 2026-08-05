package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/runtime"
	"engine/internal/store"
	"engine/internal/transport"
)

// storeFactory builds a fresh Store for a stress subtest. Both backends run the
// SAME §4.4-B assertions — the transition lock + CAS are the mediator's
// orchestration obligation (§4: verbs belong to the mediator), so they must hold
// identically no matter
// which store backs them.
type storeFactory struct {
	name string
	mk   func(t *testing.T) store.Store
}

func stressBackends() []storeFactory {
	return []storeFactory{
		{"memory", func(t *testing.T) store.Store { return store.NewMemory() }},
		{"sqlite", func(t *testing.T) store.Store {
			s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "stress.db"))
			if err != nil {
				t.Fatalf("OpenSQLite: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		}},
	}
}

func stressMediator(st store.Store, healthy bool) (*Mediator, *fakeRootfs, *fakeRuntime) {
	rp, rt := &fakeRootfs{}, &fakeRuntime{}
	m := New(Deps{
		Store:         st,
		Runtime:       rt,
		Rootfs:        rp,
		Volume:        newFakeVolume(),
		Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: healthy} },
		HealthTimeout: 2 * time.Second,
	})
	return m, rp, rt
}

func seedPausedStore(t *testing.T, st store.Store, sid, oid, tmpl string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	// §4.3 template FK: ensure the referenced Template exists (idempotent).
	_ = st.CreateTemplate(ctx, model.Template{ID: model.TemplateID(tmpl), Builtin: true, CreatedAt: now})
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: model.SandboxID(sid), Template: model.TemplateID(tmpl),
		Overlay: model.OverlayID(oid), State: model.Creating,
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSetState(ctx, model.SandboxID(sid), model.Creating, model.Running, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSetState(ctx, model.SandboxID(sid), model.Running, model.Paused, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// N goroutines race resume/pause/destroy on the SAME paused sandbox under
// -race. The transition lock serializes them and CAS rejects the ones facing an
// already-changed state — exactly one verb advances the sandbox, no data race,
// no torn state. Runs on both store backends (§4.4-B is mediator-layer, backend
// independent).
func TestStressConcurrentVerbsOneSandbox(t *testing.T) {
	for _, bk := range stressBackends() {
		t.Run(bk.name, func(t *testing.T) {
			st := bk.mk(t)
			m, _, _ := stressMediator(st, true)
			seedPausedStore(t, st, "sbx_s", "ov_s", "bare")

			const N = 24
			var wg sync.WaitGroup
			errs := make([]error, N)
			results := make([]string, N) // which verb each goroutine ran
			for i := 0; i < N; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					ctx := context.Background()
					switch i % 3 {
					case 0:
						results[i] = "resume"
						errs[i] = m.Resume(ctx, "sbx_s")
					case 1:
						results[i] = "pause"
						errs[i] = m.Pause(ctx, "sbx_s")
					case 2:
						results[i] = "destroy"
						errs[i] = m.Destroy(ctx, "sbx_s")
					}
				}(i)
			}
			wg.Wait()

			// Every error must be a clean CAS/precondition rejection or nil — never
			// a torn/unexpected state. (destroy is idempotent → may be nil even as a
			// loser once the record is gone.)
			for i, e := range errs {
				if e == nil {
					continue
				}
				if errors.Is(e, store.ErrConflict) || errors.Is(e, store.ErrNotFound) ||
					strings.Contains(e.Error(), "want paused") {
					continue
				}
				t.Fatalf("goroutine %d (%s): unexpected error %v", i, results[i], e)
			}

			// Final state must be a legal resting state — running, paused, or gone
			// (destroyed). Never creating, never a garbage value.
			rec, gerr := st.GetSandbox(context.Background(), "sbx_s")
			if errors.Is(gerr, store.ErrNotFound) {
				return // destroyed won — legal
			}
			if gerr != nil {
				t.Fatalf("final GetSandbox: %v", gerr)
			}
			if rec.State != model.Running && rec.State != model.Paused {
				t.Fatalf("final state = %s; want running/paused/destroyed", rec.State)
			}
		})
	}
}

// the lock is held for the WHOLE resume cold-start, so a concurrent destroy
// cannot acquire the lock and therefore cannot delete the Overlay in-flight
// resume is mounting. Only after resume lands (or unwinds) does destroy proceed.
// This is the data-race §4.4-B closes; run on the persistent store too.
func TestStressDestroyBlockedDuringResume(t *testing.T) {
	for _, bk := range stressBackends() {
		t.Run(bk.name, func(t *testing.T) {
			st := bk.mk(t)
			gate := make(chan struct{})
			rp, rt := &fakeRootfs{}, &fakeRuntime{launchGate: gate}
			m := New(Deps{
				Store: st, Runtime: rt, Rootfs: rp,
				Volume:        newFakeVolume(),
				Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
				HealthTimeout: 2 * time.Second,
			})
			seedPausedStore(t, st, "sbx_b4", "ov_b4", "bare")

			resumeDone := make(chan error, 1)
			go func() { resumeDone <- m.Resume(context.Background(), "sbx_b4") }()
			waitFor(t, func() bool { return contains(rt.Calls(), "Launch:sbx_b4") })

			destroyDone := make(chan error, 1)
			go func() { destroyDone <- m.Destroy(context.Background(), "sbx_b4") }()

			// destroy must NOT have deleted the Overlay while resume holds the lock.
			//
			// A fixed sleep is SOUND here specifically because the assertion is
			// NEGATIVE: it only gives destroy a window in which to (wrongly) proceed.
			// Load can make destroy slower, i.e. produce a false PASS, never a false
			// failure — so this cannot flake red. Do not copy the pattern to a positive
			// assertion, where the polarity reverses and the sleep becomes a race.
			time.Sleep(150 * time.Millisecond)
			if contains(rp.Calls(), "DestroyOverlay:ov_b4") {
				t.Fatal("destroy deleted Overlay mid-resume (lock not held whole transition)")
			}
			select {
			case <-destroyDone:
				t.Fatal("destroy completed while resume held the lock")
			default:
			}

			close(gate) // let resume finish
			if err := <-resumeDone; err != nil {
				t.Fatalf("resume: %v", err)
			}
			if err := <-destroyDone; err != nil {
				t.Fatalf("destroy: %v", err)
			}
			// after both, record is gone and Overlay was destroyed AFTER resume.
			if _, err := st.GetSandbox(context.Background(), "sbx_b4"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("record should be gone; got %v", err)
			}
		})
	}
}

// seedRunningStore inserts a `running` record and installs a live-map handle so
// the reaper's classify can resolve an endpoint (managed by this orchestrator).
func seedRunningStore(t *testing.T, m *Mediator, st store.Store, sid, oid, tmpl string, idle, retention time.Duration) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	// §4.3 template FK: ensure the referenced Template exists (idempotent).
	_ = st.CreateTemplate(ctx, model.Template{ID: model.TemplateID(tmpl), Builtin: true, CreatedAt: now})
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: model.SandboxID(sid), Template: model.TemplateID(tmpl),
		Overlay: model.OverlayID(oid), State: model.Creating,
		IdleTimeout: idle, Retention: retention, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSetState(ctx, model.SandboxID(sid), model.Creating, model.Running, nil, nil); err != nil {
		t.Fatal(err)
	}
	m.put(model.SandboxID(sid), &liveState{placement: &runtime.Placement{TransportEndpoint: "10.0.0.1:9000"}})
}

// The reaper-WINS-the-lock direction (symmetric to TestStressConcurrentVerbsOneSandbox,
// which races only user verbs). A reaper Sweep loop runs concurrently with user
// pause/resume/destroy on the SAME sandbox: the reaper can auto-pause (idle) or
// evict (paused past retention) while user verbs race it. §4.4-B's per-sandbox
// lock serializes reaper dispositions against user verbs, and CAS rejects the
// loser facing an already-changed state — so no data race (under -race), no torn
// state, exactly one verb advances the sandbox at a time. Both backends.
func TestStressReaperRacesUserVerbs(t *testing.T) {
	for _, bk := range stressBackends() {
		t.Run(bk.name, func(t *testing.T) {
			st := bk.mk(t)
			// idle client (idle far exceeds the tiny idle_timeout) so classify →
			// HealthIdle and the reaper tries to auto-pause; tiny retention so a
			// paused sandbox is immediately evict-eligible → maximal reaper pressure.
			rp, rt := &fakeRootfs{}, &fakeRuntime{adoptAlive: map[string]bool{}}
			m := New(Deps{
				Store: st, Runtime: rt, Rootfs: rp,
				Volume:        newFakeVolume(),
				Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true, idle: time.Hour} },
				HealthTimeout: 2 * time.Second,
			})
			r := NewReaper(m, time.Hour, time.Hour)
			seedRunningStore(t, m, st, "sbx_rr", "ov_rr", "bare", time.Millisecond, time.Nanosecond)

			ctx := context.Background()
			stop := make(chan struct{})
			var reaperWG sync.WaitGroup
			reaperWG.Add(1)
			go func() {
				defer reaperWG.Done()
				for {
					select {
					case <-stop:
						return
					default:
						r.Sweep(ctx) // classify + auto-pause/evict, all lock+CAS guarded
					}
				}
			}()

			const N = 24
			var wg sync.WaitGroup
			errs := make([]error, N)
			results := make([]string, N)
			for i := 0; i < N; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					switch i % 3 {
					case 0:
						results[i] = "resume"
						errs[i] = m.Resume(ctx, "sbx_rr")
					case 1:
						results[i] = "pause"
						errs[i] = m.Pause(ctx, "sbx_rr")
					case 2:
						results[i] = "destroy"
						errs[i] = m.Destroy(ctx, "sbx_rr")
					}
				}(i)
			}
			wg.Wait()
			close(stop)
			reaperWG.Wait()

			// Every user-verb error must be a clean CAS/precondition rejection or nil
			// — never torn/unexpected, even though the reaper was mutating state
			// underneath (it lost/won the lock cleanly each round).
			for i, e := range errs {
				if e == nil {
					continue
				}
				if errors.Is(e, store.ErrConflict) || errors.Is(e, store.ErrNotFound) ||
					strings.Contains(e.Error(), "want paused") {
					continue
				}
				t.Fatalf("goroutine %d (%s): unexpected error %v", i, results[i], e)
			}
			// Final state legal: running / paused / gone — never creating or garbage.
			rec, gerr := st.GetSandbox(ctx, "sbx_rr")
			if errors.Is(gerr, store.ErrNotFound) {
				return // destroyed/evicted won — legal
			}
			if gerr != nil {
				t.Fatalf("final GetSandbox: %v", gerr)
			}
			if rec.State != model.Running && rec.State != model.Paused {
				t.Fatalf("final state = %s; want running/paused/destroyed", rec.State)
			}
		})
	}
}

// CAS rejects illegal source: a burst of pauses on a paused sandbox — all
// must be rejected (pause requires running), none may corrupt state.
func TestStressPauseRejectedOnPaused(t *testing.T) {
	for _, bk := range stressBackends() {
		t.Run(bk.name, func(t *testing.T) {
			st := bk.mk(t)
			m, _, _ := stressMediator(st, true)
			seedPausedStore(t, st, "sbx_p2", "ov_p2", "bare")

			const N = 16
			var wg sync.WaitGroup
			errs := make([]error, N)
			for i := 0; i < N; i++ {
				wg.Add(1)
				go func(i int) { defer wg.Done(); errs[i] = m.Pause(context.Background(), "sbx_p2") }(i)
			}
			wg.Wait()
			for i, e := range errs {
				if !errors.Is(e, store.ErrConflict) {
					t.Fatalf("pause %d = %v; want ErrConflict (pause requires running)", i, e)
				}
			}
			rec, _ := st.GetSandbox(context.Background(), "sbx_p2")
			if rec.State != model.Paused {
				t.Fatalf("state corrupted by rejected pauses: %s", rec.State)
			}
		})
	}
}

// §3.4 no-oversubscribe under TRUE CONCURRENCY — the crack the per-sandbox
// transition lock cannot cover.
//
// Every other §4.4-B race in this file is about ONE sandbox, so the per-sandbox
// lock serializes it. The admission gate is different in kind: its invariant is
// node-GLOBAL (a sum over every record), while `Create` mints a FRESH SandboxID
// and therefore takes a brand-new, uncontended lock. So N concurrent creates hold
// N different locks and never serialize against each other at all.
//
// The failure this pins down is silent: each create is individually admissible,
// every per-sandbox cap is honored, and the node still ends up over its declared
// budget — precisely the oversubscription the gate exists to prevent (§2.3 from
// the other direction: a sandbox's write failing because of OTHER sandboxes).
// Without admitMu spanning {gate → insert}, all N read the store before any of
// them inserts, all N pass, and the sum overshoots.
//
// Falsified by removing the admitMu Lock/Unlock around Create's gate: this fails
// with ~7 admitted instead of 4.
func TestConcurrentCreatesCannotOversubscribeTheNode(t *testing.T) {
	for _, be := range stressBackends() {
		t.Run(be.name, func(t *testing.T) {
			const gib = int64(1) << 30
			st := be.mk(t)
			seedBare(t, st)
			rp, rt := &fakeRootfs{}, &fakeRuntime{}
			m := New(Deps{
				Store: st, Runtime: rt, Rootfs: rp,
				Volume:        newFakeVolume(),
				Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
				HealthTimeout: 500 * time.Millisecond,
				// Budget fits EXACTLY 4 sandboxes of 1 GiB. Nothing else consumes it:
				// the fake reports zero uncapped bytes.
				NodeCapacity: NodeCapacity{DiskByte: 4 * gib},
			})

			const N = 12
			var wg sync.WaitGroup
			errs := make([]error, N)
			for i := 0; i < N; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_, errs[i] = m.Create(context.Background(), CreateSpec{
						Template: "bare", Limits: model.Limits{DiskByte: gib},
					})
				}(i)
			}
			wg.Wait()

			admitted, refused := 0, 0
			for i, e := range errs {
				switch {
				case e == nil:
					admitted++
				case errors.Is(e, ErrInsufficientCapacity):
					refused++
				default:
					t.Fatalf("create %d failed for an unexpected reason: %v", i, e)
				}
			}
			// The invariant, stated as the gate states it: admitted promises must fit.
			if int64(admitted)*gib > 4*gib {
				t.Fatalf("admitted %d sandboxes × 1 GiB against a 4 GiB budget — the node is oversubscribed (§3.4)", admitted)
			}
			// And the gate must not be needlessly stingy: the budget fits 4, so all 4
			// slots should be taken. (A gate that refuses everything would satisfy the
			// check above while being useless.)
			if admitted != 4 {
				t.Fatalf("admitted %d; want exactly 4 (budget fits 4 × 1 GiB)", admitted)
			}
			if refused != N-4 {
				t.Fatalf("refused %d; want %d", refused, N-4)
			}
			// The store must agree — the sum over COMMITTED records is the thing the
			// next gate run will read.
			recs, err := st.ListSandboxes(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var committed int64
			for _, r := range recs {
				if r.State != model.Destroyed {
					committed += r.Limits.DiskByte
				}
			}
			if committed > 4*gib {
				t.Fatalf("committed caps in the store sum to %d > budget %d", committed, 4*gib)
			}
		})
	}
}

// slowCommitStore widens ONE seam — the Binding commit — so the {gate → commit}
// window becomes observable instead of merely present. Everything else delegates.
//
// This is not making a race up: the window is real at any width, but at in-memory
// speed eight goroutines finish gate-then-commit before the scheduler interleaves
// them, so an unwidened test passes with the bug in place (measured: 10/10 runs on
// both backends). A real commit — SQLite fsync under load, or any store behind a
// network — is orders of magnitude slower than the gate's arithmetic, so the delay
// makes the test resemble production rather than depart from it.
//
// The delay is deliberately on the COMMIT and not on the gate: it is the interval
// between "this raise passed" and "this raise is visible to the next gate run"
// that admitMu has to cover, per §3.4's "a write to a cap changes Σ at that
// commit".
type slowCommitStore struct {
	store.Store
	delay time.Duration
}

func (s slowCommitStore) UpdateBinding(ctx context.Context, id model.SandboxID, expected model.Lifecycle, b store.Binding, cause *model.DeathCause) error {
	time.Sleep(s.delay)
	return s.Store.UpdateBinding(ctx, id, expected, b, cause)
}

// The same §3.4 crack reached by the OTHER route: a cap RAISE via PATCH.
//
// TestConcurrentCreatesCannotOversubscribeTheNode covers create, where the fresh
// SandboxID makes the disjoint-lock argument obvious. A raise looks safer at a
// glance because the sandbox already exists and holds a transition lock — but that
// lock is PER-SANDBOX (§4.4-B), so N raises on N DIFFERENT sandboxes hold N
// disjoint locks and serialize against each other exactly as little as N creates
// do. The node-global sum has one arbiter, admitMu, and it must span
// {gate → commit}: §3.4 states that any write to a cap changes Σ AT THAT COMMIT,
// so a gate that returns before committing has measured a Σ that is already stale.
//
// FALSIFIED, measured. Narrowing UpdateBinding's critical section back to the gate
// alone (`m.admitMu.Lock(); err := m.admit(...); m.admitMu.Unlock()`, i.e. release
// before Store.UpdateBinding) fails on both backends in 10/10 subtest runs:
//
//	committed disk caps sum to 42949672960 > node budget 21474836480
//	(8 of 8 raises admitted)
//
// All eight raises pass a gate that each evaluated against a Σ missing the other
// seven, committing 40 GiB of promises against a 20 GiB budget — 2× oversubscribed
// with every individual cap honored. With admitMu held through the commit: 3
// admitted, 5 refused, 20/20 runs green (and under -race).
func TestConcurrentCapRaisesCannotOversubscribeTheNode(t *testing.T) {
	for _, be := range stressBackends() {
		t.Run(be.name, func(t *testing.T) {
			const gib = int64(1) << 30
			// Eight sandboxes, each starting at a 1 GiB cap and asking for 5 GiB,
			// against a 20 GiB budget. Σ(committed) starts at 8 GiB, so the budget can
			// absorb some raises but nowhere near all eight — which is the condition
			// that makes the race observable rather than merely present.
			const (
				N        = 8
				startCap = gib
				wantCap  = 5 * gib
				budget   = 20 * gib
				// The k-th raise to be gated sees the OTHER seven caps: (k-1) already
				// raised, (N-k) still at start (its own is excluded, like resume's).
				// So it is admitted iff (k-1)*wantCap + (N-k)*startCap + wantCap ≤ budget:
				//   k=3 → 10 + 5 + 5 = 20 ≤ 20  ✓
				//   k=4 → 15 + 4 + 5 = 24 > 20  ✗
				// Exactly 3 fit, in any interleaving — the count is order-independent
				// because admitMu totally orders the {gate → commit} sections.
				wantAdmitted = 3
			)
			st := be.mk(t)
			seedBare(t, st)
			// The mediator commits through the widened seam; the test seeds and reads
			// through the bare store, so setup and assertions pay no delay.
			m := New(Deps{
				Store:   slowCommitStore{Store: st, delay: 20 * time.Millisecond},
				Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{},
				Volume:        newFakeVolume(),
				Dial:          func(transport.Endpoint) transport.Client { return &fakeClient{healthy: true} },
				HealthTimeout: 500 * time.Millisecond,
				// The fake reports zero uncapped bytes, so the caps are the whole Σ.
				NodeCapacity: NodeCapacity{DiskByte: budget, MemoryByte: 0},
			})

			// Caps are editable only from `paused` (§3.4), which is also the state a
			// raise is expected to arrive in — so seed paused records directly rather
			// than driving create, keeping the test about the gate and not cold start.
			ids := make([]model.SandboxID, N)
			ctx := context.Background()
			for i := 0; i < N; i++ {
				sid := model.SandboxID(fmt.Sprintf("sbx_raise_%d", i))
				ids[i] = sid
				seedPausedStore(t, st, string(sid), fmt.Sprintf("ov_raise_%d", i), "bare")
				start := startCap
				if err := st.UpdateBinding(ctx, sid, model.Paused, store.Binding{
					DiskByte: &start,
				}, nil); err != nil {
					t.Fatalf("seed cap for %s: %v", sid, err)
				}
			}

			// All N goroutines are released at once so they reach the gate together —
			// the arrangement the race needs, and the one a burst of PATCHes produces.
			start := make(chan struct{})
			var wg sync.WaitGroup
			errs := make([]error, N)
			for i := 0; i < N; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					want := wantCap
					errs[i] = m.UpdateBinding(context.Background(), ids[i], store.Binding{
						DiskByte: &want,
					})
				}(i)
			}
			close(start)
			wg.Wait()

			admitted, refused := 0, 0
			for i, e := range errs {
				switch {
				case e == nil:
					admitted++
				case errors.Is(e, ErrInsufficientCapacity):
					refused++
				default:
					t.Fatalf("raise %d failed for an unexpected reason: %v", i, e)
				}
			}

			// THE POST-CONDITION, read from the authoritative store rather than from
			// the return values: Σ over committed caps is what the next gate run sees,
			// so that is where oversubscription is either present or absent.
			recs, err := st.ListSandboxes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var committed int64
			for _, r := range recs {
				if r.State != model.Destroyed {
					committed += r.Limits.DiskByte
				}
			}
			if committed > budget {
				t.Fatalf("committed disk caps sum to %d > node budget %d (%d of %d raises admitted) — the node is oversubscribed (§3.4)",
					committed, budget, admitted, N)
			}

			// Each admitted raise costs (wantCap - startCap); the rest stay at startCap.
			// Stating it as arithmetic rather than a magic number keeps the check honest
			// if the constants above move.
			if want := int64(N)*startCap + int64(admitted)*(wantCap-startCap); committed != want {
				t.Fatalf("committed %d; want %d for %d admitted raises — the store does not match the verbs' outcomes",
					committed, want, admitted)
			}

			// NON-VACUITY. A gate that refused every raise would satisfy the inequality
			// above while being useless, so pin the exact number the budget affords —
			// derived on wantAdmitted above.
			if admitted != wantAdmitted {
				t.Fatalf("admitted %d raises; want exactly %d (budget %d against %d × %d requested beside %d)",
					admitted, wantAdmitted, budget, N, wantCap, startCap)
			}
			if refused != N-wantAdmitted {
				t.Fatalf("refused %d; want %d", refused, N-wantAdmitted)
			}
		})
	}
}
