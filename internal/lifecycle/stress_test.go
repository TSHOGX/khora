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
	"engine/internal/store"
	"engine/internal/transport"
)

// storeFactory builds a fresh Store for a stress subtest. Both backends run the
// SAME §4.4-B assertions — the transition lock + CAS are the mediator's
// orchestration obligation (§4 开头), so they must hold identically no matter
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
	if err := st.CreateSandbox(ctx, model.Sandbox{
		ID: model.SandboxID(sid), Template: model.TemplateID(tmpl),
		Overlay: model.OverlayID(oid), State: model.Creating,
		CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSetState(ctx, model.SandboxID(sid), model.Creating, model.Running, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSetState(ctx, model.SandboxID(sid), model.Running, model.Paused, nil); err != nil {
		t.Fatal(err)
	}
}

// B3: N goroutines race resume/pause/destroy on the SAME paused sandbox under
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

// B4: the lock is held for the WHOLE resume cold-start, so a concurrent destroy
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

// B3 (CAS rejects illegal source): a burst of pauses on a paused sandbox — all
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
