package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"engine/internal/model"
	"engine/internal/store"
	"engine/internal/volume"
)

// volFixture builds a mediator with a volume backend and no metered budget (the
// gate is exercised separately, in the admission tests below).
func volFixture(t *testing.T) (*Mediator, *store.Memory, *fakeVolume) {
	t.Helper()
	st := store.NewMemory()
	vb := newFakeVolume()
	m := New(Deps{
		Store:   st,
		Runtime: &fakeRuntime{},
		Rootfs:  &fakeRootfs{},
		Volume:  vb,
		Logf:    func(string, ...any) {},
	})
	return m, st, vb
}

// TestCreateVolumeIsBytesFirst pins the ORDER, which existence-is-state forces
// (§5.C / §4.3): a `volumes` row has no not-ready value, so the storage must be
// fully allocated before the row is inserted. A crash before the insert therefore
// leaves orphan BYTES (the sweep's job) rather than a row naming storage that is not
// there.
func TestCreateVolumeIsBytesFirst(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)

	id, err := m.CreateVolume(ctx, 32<<20)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	if !strings.HasPrefix(string(id), "vol_") {
		t.Errorf("volume id %q does not look engine-minted", id)
	}
	// The backend was asked to allocate, and the cap crossed the seam WITH the
	// allocation — one operation, because a missing cap is silent and undetectable
	// afterwards (volume.md D7.3).
	calls := vb.Calls()
	if len(calls) == 0 || !strings.HasPrefix(calls[0], "CreateVolume:") {
		t.Fatalf("backend calls = %v; want CreateVolume first", calls)
	}
	if !strings.Contains(calls[0], "cap=33554432") {
		t.Errorf("cap did not cross the seam with the allocation: %q", calls[0])
	}
	// The row exists and records BOTH the backend and the cap: the backend so a later
	// operation routes to the same one that allocated the bytes, the cap because the
	// record is its only authority.
	rec, err := st.GetVolume(ctx, id)
	if err != nil {
		t.Fatalf("record missing after create: %v", err)
	}
	if rec.Backend != "fake_volume" || rec.CapByte != 32<<20 {
		t.Errorf("record = %+v; want Backend fake_volume and CapByte %d", rec, 32<<20)
	}
}

// TestCreateVolumeUnwindsBytesWhenTheRowLoses: bytes-first means a failed row insert
// leaves allocated storage behind, so the verb reclaims it rather than leaving it for
// the sweep. The sweep is the CRASH backstop; an ordinary failure should not need it.
func TestCreateVolumeUnwindsBytesWhenTheRowLoses(t *testing.T) {
	ctx := context.Background()
	vb := newFakeVolume()
	st := store.NewMemory()
	m := New(Deps{
		Store:   &volumeInsertFailingStore{Store: st},
		Runtime: &fakeRuntime{},
		Rootfs:  &fakeRootfs{},
		Volume:  vb,
		Logf:    func(string, ...any) {},
	})
	if _, err := m.CreateVolume(ctx, 0); err == nil {
		t.Fatal("create volume succeeded despite a failing row insert")
	}
	calls := vb.Calls()
	var created, destroyed string
	for _, c := range calls {
		if strings.HasPrefix(c, "CreateVolume:") {
			created = strings.TrimPrefix(strings.Split(c, ":cap=")[0], "CreateVolume:")
		}
		if strings.HasPrefix(c, "DestroyVolume:") {
			destroyed = strings.TrimPrefix(c, "DestroyVolume:")
		}
	}
	if created == "" {
		t.Fatalf("backend was never asked to allocate: %v", calls)
	}
	if destroyed != created {
		t.Errorf("allocated %q but reclaimed %q — a failed row insert must not leak the bytes it already allocated", created, destroyed)
	}
	if len(vb.units) != 0 {
		t.Errorf("backend still holds %d unit(s) after the unwind", len(vb.units))
	}
}

// TestCreateVolumeChecksCapabilityBeforeMovingBytes pins the TIMING that volume.md
// D2 calls load-bearing. An external implementation maps "backend does not support
// this" at its HTTP boundary while the error is born deep inside, so the 400 arrives
// only after bytes have already moved — and combined with mediator-minted IDs that
// leaves a window whose residue no sweep can classify.
func TestCreateVolumeChecksCapabilityBeforeMovingBytes(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)
	vb.caps = volume.Capabilities{} // supports nothing

	_, err := m.CreateVolume(ctx, 1<<20)
	if !errors.Is(err, ErrVolumeCapUnsupported) {
		t.Fatalf("create with a cap on a quota-less backend = %v; want ErrVolumeCapUnsupported", err)
	}
	// NOTHING happened: no allocation attempt, no row. That is the property — a
	// refusal after allocation would leave residue.
	if calls := vb.Calls(); len(calls) != 0 {
		t.Errorf("backend was called before the capability refusal: %v", calls)
	}
	if vols, _ := st.ListVolumes(ctx); len(vols) != 0 {
		t.Errorf("a refused create left %d row(s) behind", len(vols))
	}
}

// TestVolumeCapWritePointRefusesLoweringBelowUsage is M7 acceptance 16 at the unit
// level: the criterion is a STRICT inequality, and `== usage` is refused too.
//
// The reason for refusing what the substrate would accept is that accepting creates a
// state the control plane cannot leave (volume.md D7.3, measured): below-usage caps
// wedge the unit permanently — afterwards even truncate-to-zero and the quota write
// ITSELF return ENOSPC, so the cap can neither be removed nor raised. And
// `cap == usage` is stored happily but the next write fails, so it means FULL rather
// than "exactly enough" — a `>=` check would return 200 for a volume that can never
// take another byte.
func TestVolumeCapWritePointRefusesLoweringBelowUsage(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)
	id, err := m.CreateVolume(ctx, 64<<20)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	vb.used = 10 << 20

	for _, tc := range []struct {
		name    string
		cap     int64
		wantErr error
	}{
		{"below usage", 5 << 20, ErrVolumeCapBelowUsage},
		{"EQUAL to usage means FULL, not exactly enough", 10 << 20, ErrVolumeCapBelowUsage},
		{"strictly above usage is accepted", (10 << 20) + 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := m.SetVolumeCapacity(ctx, id, tc.cap)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("SetVolumeCapacity(%d) = %v; want success (usage %d)", tc.cap, err, vb.used)
				}
				rec, _ := st.GetVolume(ctx, id)
				if rec.CapByte != tc.cap {
					t.Errorf("record cap = %d; want %d", rec.CapByte, tc.cap)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("SetVolumeCapacity(%d) = %v; want ErrVolumeCapBelowUsage (usage %d)", tc.cap, err, vb.used)
			}
			// A refused PATCH must not have touched the storage: the wedge it is avoiding
			// is caused by the backend write, so reaching the backend at all defeats it.
			for _, c := range vb.Calls() {
				if strings.HasPrefix(c, "SetCapacity:") && strings.Contains(c, fmt.Sprintf("cap=%d", tc.cap)) {
					t.Errorf("a refused cap still reached the backend: %q", c)
				}
			}
		})
	}

	// Removing the cap entirely is always allowed: it can only widen what the unit may
	// hold, so the wedge argument does not apply.
	if err := m.SetVolumeCapacity(ctx, id, 0); err != nil {
		t.Errorf("clearing the cap = %v; want success — 0 only widens the ceiling", err)
	}
}

// TestVolumeCapWritePointFailsClosedOnUnreadableUsage: usage is the DECISION here,
// not reporting, so an unreadable backend must refuse rather than set a cap blind.
// The state a wrong guess produces has no exit through the API at all.
func TestVolumeCapWritePointFailsClosedOnUnreadableUsage(t *testing.T) {
	ctx := context.Background()
	m, _, vb := volFixture(t)
	id, err := m.CreateVolume(ctx, 64<<20)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	vb.usageErr = errors.New("bulk quota read failed wholesale")
	if err := m.SetVolumeCapacity(ctx, id, 32<<20); err == nil {
		t.Fatal("SetVolumeCapacity succeeded with unreadable usage — an under-usage cap is irreversible, so this must fail closed")
	}
	// inspect, by contrast, DEGRADES rather than failing: there usage is reporting.
	if _, err := m.GetVolume(ctx, id); err != nil {
		t.Errorf("inspect failed on unreadable usage = %v; there usage is reporting, not a decision", err)
	}
}

// TestVolumeCapEditableWhileNothingIsMounted is the §5.C asymmetry with sandbox caps,
// asserted rather than assumed: a volume cap is editable with no placement anywhere,
// which is the exact opposite of "resource caps are editable only from paused".
//
// The reason is mechanical: a sandbox cap re-materializes at the next placement,
// while a volume HAS no placement — so deferring would mean NEVER for an unmounted
// volume.
func TestVolumeCapEditableWhileNothingIsMounted(t *testing.T) {
	ctx := context.Background()
	m, st, _ := volFixture(t)
	id, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// No sandbox exists at all, let alone a paused one.
	if recs, _ := st.ListSandboxes(ctx); len(recs) != 0 {
		t.Fatalf("fixture unexpectedly has %d sandbox(es)", len(recs))
	}
	if err := m.SetVolumeCapacity(ctx, id, 16<<20); err != nil {
		t.Fatalf("cap edit with nothing mounted = %v; want success (§5.C: no placement to defer to)", err)
	}
	rec, _ := st.GetVolume(ctx, id)
	if rec.CapByte != 16<<20 {
		t.Errorf("cap = %d; want %d", rec.CapByte, 16<<20)
	}
}

// TestAdmissionCountsVolumeCaps is M7 acceptance 17: Σ(volume caps) is in the FIRST
// term, so volumes cannot oversubscribe the node, and a metered disk axis REQUIRES a
// volume cap.
//
// The failure this guards is immediate oversell (§3.4): an uncapped volume
// contributes 0 to the left side while free to consume all of the right, which does
// not undercount so much as make the inequality describe a different device.
func TestAdmissionCountsVolumeCaps(t *testing.T) {
	ctx := context.Background()

	t.Run("a metered disk axis requires a volume cap", func(t *testing.T) {
		st := store.NewMemory()
		m := New(Deps{
			Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{}, Volume: newFakeVolume(),
			NodeCapacity: NodeCapacity{DiskByte: 100 << 20},
			Logf:         func(string, ...any) {},
		})
		_, err := m.CreateVolume(ctx, 0)
		if !errors.Is(err, ErrCapRequired) {
			t.Fatalf("uncapped volume create on a metered node = %v; want ErrCapRequired (400, not 507)", err)
		}
		// And it is NOT the capacity verdict: nothing is full.
		if errors.Is(err, ErrInsufficientCapacity) {
			t.Errorf("the refusal conflates 'you must declare a cap' with 'the node is full'")
		}
	})

	t.Run("volume caps count against the budget", func(t *testing.T) {
		st := store.NewMemory()
		m := New(Deps{
			Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{}, Volume: newFakeVolume(),
			NodeCapacity: NodeCapacity{DiskByte: 100 << 20},
			Logf:         func(string, ...any) {},
		})
		// Two 40 MiB volumes fit in 100 MiB; a third does not. Without Σ(volume caps)
		// in the first term, ALL THREE would be admitted and the node oversold.
		for i := 0; i < 2; i++ {
			if _, err := m.CreateVolume(ctx, 40<<20); err != nil {
				t.Fatalf("volume %d: %v", i+1, err)
			}
		}
		_, err := m.CreateVolume(ctx, 40<<20)
		if !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("third 40 MiB volume against a 100 MiB budget = %v; want ErrInsufficientCapacity — omitting Σ(volume caps) oversells the node on the spot (§3.4)", err)
		}
	})

	t.Run("a volume cap and a sandbox cap are LIKE TERMS", func(t *testing.T) {
		st := store.NewMemory()
		vb := newFakeVolume()
		m := New(Deps{
			Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{}, Volume: vb,
			NodeCapacity: NodeCapacity{DiskByte: 100 << 20},
			Logf:         func(string, ...any) {},
		})
		// Seeded directly: this asserts the SUM, not the create path, so the sandbox row
		// is written straight to the store.
		seedBare(t, st)
		if err := st.CreateSandbox(ctx, model.Sandbox{
			ID: "sbx_a", Template: "bare", Overlay: "ov_a", State: model.Paused,
			Limits: model.Limits{DiskByte: 70 << 20},
		}, nil); err != nil {
			t.Fatalf("seed sandbox: %v", err)
		}
		// 70 MiB of sandbox cap is already committed, so a 40 MiB volume must not fit —
		// the two are like terms on ONE left-hand side, not two independent budgets.
		if _, err := m.CreateVolume(ctx, 40<<20); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("volume admitted beside a 70 MiB sandbox cap on a 100 MiB budget = %v; want refusal — no third term (§3.4)", err)
		}
		// 20 MiB does fit.
		if _, err := m.CreateVolume(ctx, 20<<20); err != nil {
			t.Errorf("20 MiB volume beside 70 MiB of sandbox cap on a 100 MiB budget = %v; want success", err)
		}
	})

	t.Run("uncapped VOLUME bytes count in the second term", func(t *testing.T) {
		st := store.NewMemory()
		vb := newFakeVolume()
		// Bytes the volume backend holds under no declared cap. Without §5.C's half of
		// the second term these are invisible and the node is silently overcommitted.
		vb.uncappedBytes = 90 << 20
		m := New(Deps{
			Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{}, Volume: vb,
			NodeCapacity: NodeCapacity{DiskByte: 100 << 20},
			Logf:         func(string, ...any) {},
		})
		if _, err := m.CreateVolume(ctx, 20<<20); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("create against 90 MiB of UNCAPPED volume bytes on a 100 MiB budget = %v; want refusal — the second term sums §5.B AND §5.C (§3.4)", err)
		}
	})

	t.Run("an unmeasurable volume backend fails CLOSED", func(t *testing.T) {
		st := store.NewMemory()
		vb := newFakeVolume()
		vb.measureErr = errors.New("bulk read failed wholesale")
		m := New(Deps{
			Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{}, Volume: vb,
			NodeCapacity: NodeCapacity{DiskByte: 100 << 20},
			Logf:         func(string, ...any) {},
		})
		if _, err := m.CreateVolume(ctx, 1<<20); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("create with an unmeasurable volume backend = %v; want a fail-closed refusal (§2.3: failing open moves the cost onto unrelated sandboxes)", err)
		}
	})

	t.Run("a cap RAISE re-clears admission and excludes its own committed cap", func(t *testing.T) {
		st := store.NewMemory()
		vb := newFakeVolume()
		m := New(Deps{
			Store: st, Runtime: &fakeRuntime{}, Rootfs: &fakeRootfs{}, Volume: vb,
			NodeCapacity: NodeCapacity{DiskByte: 100 << 20},
			Logf:         func(string, ...any) {},
		})
		id, err := m.CreateVolume(ctx, 40<<20)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// A raise to 90 MiB fits ONLY because the volume's own committed 40 MiB is
		// excluded; counting it too (40 + 90 = 130) would refuse a raise that fits.
		if err := m.SetVolumeCapacity(ctx, id, 90<<20); err != nil {
			t.Fatalf("raise to 90 MiB on a 100 MiB budget = %v; want success — the volume's own committed cap must be excluded", err)
		}
		// A raise past the budget IS refused, so the exclusion has not disabled the gate.
		if err := m.SetVolumeCapacity(ctx, id, 120<<20); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("raise to 120 MiB on a 100 MiB budget = %v; want ErrInsufficientCapacity", err)
		}
		// A LOWERING needs no gate (Σ only shrinks) — and it is above usage, so the
		// strict-inequality check passes.
		if err := m.SetVolumeCapacity(ctx, id, 10<<20); err != nil {
			t.Errorf("lowering = %v; want success without a gate (Σ shrinks)", err)
		}
	})
}

// TestDeleteVolumeIsRowFirst pins the order, which is load-bearing rather than
// incidental: the row delete is where every reference-protection refusal surfaces
// (§6.2's Volume 409s), so destroying bytes first would mean a delete the store then
// REFUSES has already destroyed the data it was refusing to release.
func TestDeleteVolumeIsRowFirst(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)
	id, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.DeleteVolume(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetVolume(ctx, id); err != store.ErrVolumeNotFound {
		t.Errorf("record after delete = %v; want ErrVolumeNotFound", err)
	}
	if len(vb.units) != 0 {
		t.Errorf("backend still holds the unit after delete")
	}
	// Byte reclamation FAILING does not fail the verb: the record is already gone, so
	// the sweep is the retry (§4.4 licenses exactly this deferral).
	id2, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	vb.failDestroy = true
	if err := m.DeleteVolume(ctx, id2); err != nil {
		t.Errorf("delete with failing byte reclamation = %v; want success (the sweep retries, §4.4)", err)
	}
	if _, err := st.GetVolume(ctx, id2); err != store.ErrVolumeNotFound {
		t.Errorf("record survived a delete whose byte reclamation failed")
	}
}

// TestDeleteVolumeReportsNotFound: §6.2 — idempotent delete must not swallow the
// verdicts, because the RESTRICT refusals land on this same path.
func TestDeleteVolumeReportsNotFound(t *testing.T) {
	ctx := context.Background()
	m, _, _ := volFixture(t)
	if err := m.DeleteVolume(ctx, "vol_never"); !errors.Is(err, store.ErrVolumeNotFound) {
		t.Fatalf("delete of an absent volume = %v; want store.ErrVolumeNotFound (§6.2: folding this into success makes a REFUSED delete indistinguishable from a successful one)", err)
	}
}

// TestImportExportNeedsNoSandbox is the record-and-verb half of M7 acceptance 21:
// bytes go in and come back out on a node that has NEVER had a sandbox. That is what
// makes "the volume's lifecycle is decoupled from any sandbox" true in substance
// rather than only in the record layer (§5.C).
func TestImportExportNeedsNoSandbox(t *testing.T) {
	ctx := context.Background()
	m, st, _ := volFixture(t)
	id, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// No sandbox has ever existed here.
	if recs, _ := st.ListSandboxes(ctx); len(recs) != 0 {
		t.Fatalf("fixture unexpectedly has %d sandbox(es)", len(recs))
	}

	payload := bytes.Repeat([]byte("seed"), 40<<10) // 160 KiB, several stream chunks
	n, err := m.ImportVolume(ctx, id, "data/f", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("import wrote %d of %d bytes", n, len(payload))
	}
	var out bytes.Buffer
	if _, err := m.ExportVolume(ctx, id, "data/f", &out); err != nil {
		t.Fatalf("export: %v", err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Errorf("export returned %d bytes, want the %d imported byte-for-byte", out.Len(), len(payload))
	}

	// A shorter re-import REPLACES rather than overwriting in place: otherwise the old
	// tail stays attached and the result is neither the old nor the new content.
	short := []byte("short")
	if _, err := m.ImportVolume(ctx, id, "data/f", bytes.NewReader(short)); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	out.Reset()
	if _, err := m.ExportVolume(ctx, id, "data/f", &out); err != nil {
		t.Fatalf("export after re-import: %v", err)
	}
	if !bytes.Equal(out.Bytes(), short) {
		t.Errorf("after a shorter re-import the content is %q; want %q — an import must replace, not overwrite in place", out.Bytes(), short)
	}

	// An unknown volume is a clean record-level refusal, not whatever the storage
	// layer happens to say.
	if _, err := m.ExportVolume(ctx, "vol_nope", "f", &out); !errors.Is(err, store.ErrVolumeNotFound) {
		t.Errorf("export from an unknown volume = %v; want store.ErrVolumeNotFound", err)
	}
}

// TestSweepOrphanVolumesDecidesInTheMediator: the backend ENUMERATES and the mediator
// DECIDES, and the decision is made AFTER enumeration.
//
// That order closes a MEASURED window. With the diff done inside the backend from two
// pre-read sets, a volume whose whole creation fell between the `live` read and the
// `building` read was in neither and had its bytes deleted while FULLY COMMITTED
// (measured on the substrate: 3 of 6 snapshots destroyed under 458 overlapping sweeps).
// Enumerating first means every candidate's bytes predate all the checks, which is what
// makes the checks conclusive.
func TestSweepOrphanVolumesDecidesInTheMediator(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)

	live, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A true orphan: bytes with no row, the shape a crash leaves.
	orphan, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteVolume(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	// An in-flight create: registered, bytes present, row not yet inserted.
	if err := vb.CreateVolume(ctx, "vol_inflight", 0); err != nil {
		t.Fatal(err)
	}
	m.markBuildingVolume("vol_inflight")
	defer m.unmarkBuildingVolume("vol_inflight")

	if err := m.SweepOrphanVolumes(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// The orphan is gone (positive control — without it a no-op sweep passes the rest).
	vb.mu.Lock()
	stillOrphan := vb.units[string(orphan)]
	stillLive := vb.units[string(live)]
	stillInFlight := vb.units["vol_inflight"]
	vb.mu.Unlock()
	if stillOrphan {
		t.Errorf("the orphan survived the sweep — a sweep that reclaims nothing passes every negative assertion here")
	}
	if !stillLive {
		t.Errorf("a volume with a live RECORD was reclaimed")
	}
	if !stillInFlight {
		t.Errorf("an IN-FLIGHT create was reclaimed — volume creation is bytes-first, so a healthy in-flight create presents as 'bytes, no row' by construction")
	}
}

// TestSweepOrphanVolumesFailsClosedOnUnreadableEnumeration: an enumeration error must
// reclaim NOTHING. Measured, a bulk read fails wholesale and returns zero rows under
// exactly the concurrent churn a sweep runs in — so "no units" and "the read failed"
// must never be the same answer.
func TestSweepOrphanVolumesFailsClosedOnUnreadableEnumeration(t *testing.T) {
	ctx := context.Background()
	m, st, vb := volFixture(t)
	orphan, err := m.CreateVolume(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteVolume(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	vb.enumErr = errors.New("bulk read failed wholesale")

	if err := m.SweepOrphanVolumes(ctx); err == nil {
		t.Fatal("the sweep reported success on an unreadable enumeration")
	}
	vb.mu.Lock()
	survived := vb.units[string(orphan)]
	vb.mu.Unlock()
	if !survived {
		t.Errorf("the sweep reclaimed something despite an unreadable enumeration — an error is not evidence that anything is an orphan (§5.C)")
	}
}

// volumeInsertFailingStore fails only CreateVolume, so the create verb's unwind path
// (bytes allocated, row lost) can be exercised without touching any other method.
type volumeInsertFailingStore struct {
	store.Store
}

func (s *volumeInsertFailingStore) CreateVolume(context.Context, model.Volume) error {
	return errors.New("injected: volume row insert failed")
}
