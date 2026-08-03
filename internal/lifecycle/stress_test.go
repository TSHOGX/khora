package lifecycle

import (
	"context"
	"errors"
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
