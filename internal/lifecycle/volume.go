package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"engine/internal/model"
	"engine/internal/volume"
)

// Volume verbs (§5.C), owned by the §4 mediator like every other verb: the
// mediator reads the authoritative store and drives the backend's mechanisms, and
// the backend never reads the store (§4).
//
// A Volume's whole point is that its lifecycle is DECOUPLED from every sandbox
// (§5.C) — created before one exists, surviving after the last one is gone,
// operable while nothing has it mounted. That is why none of these verbs takes a
// SandboxID, and why the content path below goes through the backend's own
// root-confined accessor rather than through §5.E `fs`: `fs` lives inside a guest
// and is reachable only through a live placement, so an unmounted volume has no
// route through it at all (volume.md D5.1).
//
// None of them publishes an event. A volume is not a sandbox, so its CRUD is not a
// §4.1 lifecycle transition — the same rule Template CRUD follows (§4.3).

// ErrVolumeCapBelowUsage is the §5.C / §6.1 PATCH refusal: the requested cap is not
// STRICTLY greater than the volume's current usage.
//
// It is 400 rather than 507 (§6.2): nothing is full and no retry helps — the
// request itself is unacceptable. And it is refused even though THE SUBSTRATE WOULD
// HAPPILY ACCEPT IT, which is the whole reason this check exists in the engine.
// Measured (volume.md D7.3): a cap below current usage wedges the storage unit
// PERMANENTLY — afterwards even `mkdir` and truncating an existing file to zero
// return ENOSPC, and the quota operation ITSELF returns ENOSPC, so the cap can
// neither be removed nor raised, not even back above current usage. The only exit
// is deleting the guest's bytes, and §6 gives the engine no way to do that on the
// caller's behalf. So accepting would manufacture a state the control plane cannot
// leave.
//
// The boundary case is included deliberately: `cap == usage` is ACCEPTED by the
// substrate and stored, but the very next write gets ENOSPC — so it means FULL, not
// "exactly enough". Hence a strict inequality, not >=. A `>=` check would hand the
// caller a 200 and a volume that can never take another byte.
var ErrVolumeCapBelowUsage = errors.New("lifecycle: volume cap must be strictly greater than current usage")

// ErrVolumeCapUnsupported is the §6.2 400 for a cap the chosen backend cannot
// enforce. It wraps the backend's own typed refusal so the boundary does not have to
// re-derive the verdict, and it exists as a distinct sentinel because "this backend
// cannot do that" is a different thing from "that number is wrong".
var ErrVolumeCapUnsupported = errors.New("lifecycle: backend cannot enforce a capacity cap")

// CreateVolume allocates a volume: gate, then BYTES, then the row (§5.C
// bytes-first / §4.3 existence-is-state).
//
// The ORDER is forced by existence-is-state. A `volumes` row has no not-ready
// value, so it may only be inserted once the storage truly exists; a crash before
// the insert therefore leaves orphan BYTES, which the sweep reclaims, instead of a
// row naming storage that is not there. The in-flight keep-set covers exactly that
// window — and it matters MORE here than for an Overlay unit, because there the row
// is inserted first, whereas here a perfectly healthy in-flight create presents as
// "bytes, no row" even to a completely fresh keep-set.
//
// CAPABILITIES ARE ASKED BEFORE AN ID IS MINTED OR A BYTE MOVES. That ordering is
// load-bearing rather than tidy (volume.md D2): an external implementation maps
// "backend does not support this" at its HTTP boundary while the error is born deep
// inside, so the 400 arrives only after bytes have already moved — a window whose
// residue no sweep can classify.
//
// admitMu spans {gate → allocate → insert}, which is the same
// {gate → make the promise visible} discipline create uses. Unlike Promote's byte
// copy the middle step here is bounded metadata work (allocate the unit, write the
// cap), so holding the node-global gate across it does not serialize the node behind
// a long operation.
func (m *Mediator) CreateVolume(ctx context.Context, capBytes int64) (model.VolumeID, error) {
	if capBytes < 0 {
		return "", fmt.Errorf("create volume: cap must be non-negative (0 = uncapped), got %d", capBytes)
	}
	be := m.deps.Volume
	if be == nil {
		return "", fmt.Errorf("create volume: no volume backend registered")
	}
	// Capability check FIRST — before the ID, before the gate, before any bytes.
	if capBytes > 0 && !be.Capabilities().Quota {
		return "", fmt.Errorf("create volume: %w: backend %q reports no capacity-cap capability, and a cap that cannot be enforced is refused rather than silently dropped (§3.4)",
			ErrVolumeCapUnsupported, be.Name())
	}

	vid := model.VolumeID("vol_" + randToken())
	rec := model.Volume{ID: vid, Backend: be.Name(), CapByte: capBytes, CreatedAt: time.Now()}

	// Protect the unit for the whole create window (§4.4-A). Registered BEFORE the
	// gate so there is no instant in which bytes could exist unprotected.
	m.markBuildingVolume(vid)
	defer m.unmarkBuildingVolume(vid)

	m.admitMu.Lock()
	if err := m.admitVolume(ctx, capBytes); err != nil {
		m.admitMu.Unlock()
		return "", err
	}
	if err := be.CreateVolume(ctx, string(vid), capBytes); err != nil {
		m.admitMu.Unlock()
		if errors.Is(err, volume.ErrUnsupported) {
			return "", fmt.Errorf("create volume: %w: %v", ErrVolumeCapUnsupported, err)
		}
		return "", fmt.Errorf("create volume: allocate: %w", err)
	}
	err := m.deps.Store.CreateVolume(ctx, rec)
	m.admitMu.Unlock()
	if err != nil {
		// The bytes exist and the row does not. Reclaim now rather than leaving it to
		// the sweep — the sweep is the crash backstop, not the ordinary unwind path.
		if derr := be.DestroyVolume(ctx, string(vid)); derr != nil {
			m.deps.Logf("create volume: unwind %s left bytes behind (the orphan sweep will retry): %v", vid, derr)
		}
		return "", fmt.Errorf("create volume: insert record: %w", err)
	}
	return vid, nil
}

// VolumeInfo is the §6.1 inspect shape: the record plus the backend's own usage
// reading.
//
// Usage comes from the backend and the cap comes from the RECORD, and they are not
// interchangeable. The backend's limit read-back is diagnostic only: the substrate
// maps "no cap" and 0 onto each other in both directions, so "never capped" and
// "deliberately uncapped" are indistinguishable when read from storage (volume.md
// D7.3). Reporting the record's value keeps inspect honest about what the engine
// PROMISED, which is also what the admission gate counts.
type VolumeInfo struct {
	Volume model.Volume
	// UsedByte is the backend's own statement of bytes consumed. Zero is a legitimate
	// answer for an empty volume.
	UsedByte int64
}

// GetVolume reads one volume's record and asks its backend for current usage
// (§6.1 inspect).
func (m *Mediator) GetVolume(ctx context.Context, id model.VolumeID) (VolumeInfo, error) {
	rec, err := m.deps.Store.GetVolume(ctx, id)
	if err != nil {
		return VolumeInfo{}, fmt.Errorf("inspect volume: %w", err)
	}
	info := VolumeInfo{Volume: rec}
	if be := m.deps.Volume; be != nil {
		used, _, err := be.Usage(ctx, string(id))
		if err != nil {
			// Usage is REPORTING, not a decision here, so an unreadable backend degrades
			// the response rather than failing the read. The strict-inequality check in
			// SetVolumeCapacity treats the same failure as fatal, because there it IS the
			// decision.
			m.deps.Logf("inspect volume %s: usage unavailable: %v", id, err)
		} else {
			info.UsedByte = used
		}
	}
	return info, nil
}

// ListVolumes returns every volume record (§6.1 list).
func (m *Mediator) ListVolumes(ctx context.Context) ([]model.Volume, error) {
	return m.deps.Store.ListVolumes(ctx)
}

// SetVolumeCapacity is the §6.1 PATCH write point — the cap's only write point
// besides create.
//
// Three disciplines, all from §5.C, and none of them optional:
//
//  1. IT WORKS WHETHER OR NOT ANYTHING HAS THE VOLUME MOUNTED, which is the exact
//     opposite of a sandbox cap (editable only while `paused`). The reason is
//     mechanical: a sandbox cap re-materializes at the next placement, while a
//     volume HAS no placement of its own — so "defer to the next placement" would
//     mean NEVER for an unmounted volume. There is nothing to defer, so there is no
//     "pause it first" refusal to make.
//  2. A RAISE RE-CLEARS ADMISSION (§3.4: any write to a cap changes Σ at THIS
//     commit, not at some later placement), and may be refused with 507. A lowering
//     does not need the gate — Σ only shrinks.
//  3. LOWERING BELOW CURRENT USAGE IS REFUSED, with a STRICT inequality. See
//     ErrVolumeCapBelowUsage: the substrate accepts such a cap and is then wedged
//     permanently, and `cap == usage` means FULL rather than "exactly enough".
//
// It is deliberately NOT subject to the write-regime invariant that governs direct
// content changes: this moves an allocation ceiling, not bytes, so there is nothing
// for a concurrent writer to tear (§5.C).
func (m *Mediator) SetVolumeCapacity(ctx context.Context, id model.VolumeID, capBytes int64) error {
	if capBytes < 0 {
		return fmt.Errorf("patch volume: cap must be non-negative (0 = uncapped), got %d", capBytes)
	}
	be := m.deps.Volume
	if be == nil {
		return fmt.Errorf("patch volume: no volume backend registered")
	}
	rec, err := m.deps.Store.GetVolume(ctx, id)
	if err != nil {
		return fmt.Errorf("patch volume: %w", err)
	}
	if capBytes > 0 && !be.Capabilities().Quota {
		return fmt.Errorf("patch volume: %w: backend %q reports no capacity-cap capability", ErrVolumeCapUnsupported, be.Name())
	}

	// The strict-inequality check, only when a cap is actually being imposed. Removing
	// the cap (0) is always safe — it can only widen what the unit may hold.
	if capBytes > 0 {
		used, _, err := be.Usage(ctx, string(id))
		if err != nil {
			// FAIL CLOSED. Unlike inspect, usage here IS the decision: admitting a cap
			// without knowing usage risks the irreversible wedge described on
			// ErrVolumeCapBelowUsage, and that state has no exit through the API at all.
			return fmt.Errorf("patch volume: cannot read current usage of %s, refusing to set a cap blind (an under-usage cap wedges the volume irreversibly, §5.C): %w", id, err)
		}
		if capBytes <= used {
			return fmt.Errorf("patch volume: %w: requested %d, current usage %d — a cap at or below usage wedges the volume irreversibly (even truncate-to-zero and the quota write itself then fail), and `cap == usage` means FULL rather than exactly enough (§5.C / volume.md D7.3)",
				ErrVolumeCapBelowUsage, capBytes, used)
		}
	}

	// A raise is a new promise → re-gate, holding admitMu across
	// {gate → apply → commit} so a concurrent gate run cannot spend the same
	// capacity. `delta` excludes this volume's own committed cap, exactly as resume
	// excludes a sandbox's own, so the check is against the increase rather than
	// double-charging the existing promise.
	raising := capBytes > rec.CapByte || (capBytes > 0 && rec.CapByte == 0)
	if raising {
		m.admitMu.Lock()
		defer m.admitMu.Unlock()
		if err := m.admitVolumeRaise(ctx, id, capBytes); err != nil {
			return err
		}
	}

	// Backend first, then the record. If the backend write fails the record is
	// unchanged and still describes the storage, which keeps the record authoritative
	// (§3.4). The reverse order would leave a record promising a cap the storage does
	// not carry — and that divergence is UNDETECTABLE afterwards (volume.md D7.3), so
	// it must not be reachable.
	if err := be.SetCapacity(ctx, string(id), capBytes); err != nil {
		if errors.Is(err, volume.ErrUnsupported) {
			return fmt.Errorf("patch volume: %w: %v", ErrVolumeCapUnsupported, err)
		}
		return fmt.Errorf("patch volume: apply cap: %w", err)
	}
	if err := m.deps.Store.SetVolumeCapacity(ctx, id, capBytes); err != nil {
		return fmt.Errorf("patch volume: commit cap: %w", err)
	}
	return nil
}

// DeleteVolume removes the record, then reclaims the bytes (§5.C / §4.4).
//
// ROW FIRST, and that order is load-bearing rather than incidental. The row delete
// is where every reference-protection refusal surfaces (§6.2's Volume 409s), so
// destroying bytes first would mean a delete the store then REFUSES has already
// destroyed the data it was refusing to let go — the guard would be worse than
// absent. Row-first makes the refusal free of side effects.
//
// A crash between the two leaves orphan bytes, which is exactly the deferral §4.4
// licenses and the orphan sweep retries. Byte reclamation failing does NOT fail the
// verb, for the same reason: the record is gone, so the sweep is the retry.
//
// NOT idempotent-by-swallowing: an absent volume reports store.ErrVolumeNotFound
// (§6.2). Folding it into success is how an external implementation made
// "the guard refused this delete" indistinguishable from "the delete succeeded".
func (m *Mediator) DeleteVolume(ctx context.Context, id model.VolumeID) error {
	if err := m.deps.Store.DeleteVolume(ctx, id); err != nil {
		return fmt.Errorf("delete volume: %w", err)
	}
	if be := m.deps.Volume; be != nil {
		if err := be.DestroyVolume(ctx, string(id)); err != nil {
			m.deps.Logf("delete volume %s: byte reclamation failed, deferring to the orphan sweep (§4.4): %v", id, err)
		}
	}
	return nil
}

// ImportVolume streams src into the volume at the root-relative path (§6.1
// POST /v1/volumes/{id}/import).
//
// NEEDS NO SANDBOX, and that is the point rather than a convenience: this and
// ExportVolume are how "a volume's lifecycle is decoupled from any sandbox"
// becomes true in SUBSTANCE instead of only in the record layer (§5.C). Seeding a
// volume from scratch before any sandbox exists, and reading one after the last
// sandbox is gone, have no other route.
//
// It STREAMS through a fixed buffer rather than materializing the payload, matching
// the §6.1 requirement that the bytes never sit whole at either end.
//
// Any existing file at that path is REMOVED first, so an import is a replacement
// rather than an overwrite-in-place: writing a shorter payload over a longer file
// would otherwise leave the old tail attached and produce a file that is neither
// the old nor the new content.
func (m *Mediator) ImportVolume(ctx context.Context, id model.VolumeID, path string, src io.Reader) (int64, error) {
	r, err := m.openVolumeRoot(ctx, id)
	if err != nil {
		return 0, err
	}
	defer r.Close()

	// Create the parent chain. Root.WriteAt deliberately does NOT create parents (a
	// write is a write), so the mediator does it here — seeding a volume from scratch
	// is import's whole reason to exist, and requiring the caller to pre-create every
	// directory through separate calls would make the primary use case the awkward
	// one. Each segment is created and an already-existing one is tolerated.
	if err := mkdirChain(r, path); err != nil {
		return 0, fmt.Errorf("import volume %s: prepare %s: %w", id, path, err)
	}
	if err := r.Remove(path); err != nil {
		return 0, fmt.Errorf("import volume %s: clear %s: %w", id, path, err)
	}
	buf := make([]byte, 256<<10)
	var written int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := r.WriteAt(path, buf[:n], written)
			written += int64(w)
			if werr != nil {
				// A capacity cap surfaces here as ENOSPC and is passed through rather than
				// reclassified: past the cap a write failing is an EXPECTED data-plane
				// result (§3.4), and the partial content is what the caller asked to write
				// as far as it got.
				return written, fmt.Errorf("import volume %s: write %s: %w", id, path, werr)
			}
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, fmt.Errorf("import volume %s: read source: %w", id, rerr)
		}
	}
}

// ExportVolume streams the volume's file at the root-relative path into dst (§6.1
// GET /v1/volumes/{id}/export). Needs no sandbox, for the same reason ImportVolume
// does not.
func (m *Mediator) ExportVolume(ctx context.Context, id model.VolumeID, path string, dst io.Writer) (int64, error) {
	r, err := m.openVolumeRoot(ctx, id)
	if err != nil {
		return 0, err
	}
	defer r.Close()

	fi, err := r.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("export volume %s: %s: %w", id, path, err)
	}
	if fi.IsDir {
		return 0, fmt.Errorf("export volume %s: %s is a directory, not a file", id, path)
	}
	buf := make([]byte, 256<<10)
	var read int64
	for {
		n, rerr := r.ReadAt(path, buf, read)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return read, fmt.Errorf("export volume %s: write response: %w", id, werr)
			}
			read += int64(n)
		}
		if rerr == io.EOF {
			return read, nil
		}
		if rerr != nil {
			return read, fmt.Errorf("export volume %s: read %s: %w", id, path, rerr)
		}
	}
}

// mkdirChain creates every parent directory of a root-relative file path, tolerating
// ones that already exist.
//
// It walks the segments rather than asking for a recursive create because Root has no
// MkdirAll, and it should not: the accessor's whole value is that each operation is
// one root-confined resolution, and a recursive helper on the seam would be a second
// place where path semantics could disagree with the first. Confinement is unaffected
// — every segment goes through the same resolution as any other path.
func mkdirChain(r volume.Root, filePath string) error {
	segs := strings.Split(strings.Trim(pathClean(filePath), "/"), "/")
	if len(segs) < 2 {
		return nil // the file sits directly in the volume root
	}
	var chain string
	for _, seg := range segs[:len(segs)-1] {
		if seg == "" {
			continue
		}
		if chain == "" {
			chain = seg
		} else {
			chain += "/" + seg
		}
		if err := r.Mkdir(chain, 0o755); err != nil {
			// Already there is the common case (a second import into the same directory),
			// and it is indistinguishable from success for this purpose.
			if fi, statErr := r.Stat(chain); statErr == nil && fi.IsDir {
				continue
			}
			return err
		}
	}
	return nil
}

// pathClean normalizes a caller path the same way the accessor does, so the chain
// walked here matches the one the write will resolve.
func pathClean(p string) string { return path.Clean("/" + strings.TrimSpace(p)) }

// openVolumeRoot resolves a volume ID to its backend content accessor, checking the
// RECORD first.
//
// The record check is not redundant with the backend's own error: Materialize is
// create-if-absent by contract (§5.C), and although OpenRoot is not, keeping the
// authority check in the mediator is what makes an unknown ID a clean 404 instead
// of whatever the storage layer happens to say (§4: the store is authoritative, the
// backend answers about bytes).
func (m *Mediator) openVolumeRoot(ctx context.Context, id model.VolumeID) (volume.Root, error) {
	be := m.deps.Volume
	if be == nil {
		return nil, fmt.Errorf("volume content: no volume backend registered")
	}
	if _, err := m.deps.Store.GetVolume(ctx, id); err != nil {
		return nil, fmt.Errorf("volume content: %w", err)
	}
	r, err := be.OpenRoot(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("volume content: open %s: %w", id, err)
	}
	return r, nil
}

// SweepOrphanVolumes reclaims volume storage with no live record (§4.4-A) — the
// §5.C mirror of SweepOrphanOverlays. The mediator reads the AUTHORITATIVE store
// for the live set, snapshots the in-flight keep-set as LATE as possible, and hands
// both to the backend, which enumerates only its own storage (§4: subsystems never
// read the store).
//
// Skip-and-log per §4.4-A: a sweep failure is not fatal and retries next pass.
//
// ITS POSITION IN THE BOOT SEQUENCE IS LOAD-BEARING, not housekeeping — it must run
// BEFORE any storage unit is created. A leaked quota row does not reserve its own
// unit id, so a restart can hand that id, ALONG WITH ITS CAP, to a brand-new volume,
// which then meets ENOSPC on a limit nobody set for it (or is silently
// over-provisioned) with no way for the engine to notice. Sweeping only on the
// periodic tick would leave a window in which exactly that inheritance happens
// (§5.C / volume.md D7.1).
func (m *Mediator) SweepOrphanVolumes(ctx context.Context) error {
	be := m.deps.Volume
	if be == nil {
		return nil
	}
	vols, err := m.deps.Store.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("sweep volumes: list volumes: %w", err)
	}
	live := make([]string, 0, len(vols))
	for _, v := range vols {
		live = append(live, string(v.ID))
	}
	building := m.buildingVolumeIDs()
	n, err := be.SweepOrphanVolumes(ctx, live, building)
	if err != nil {
		return fmt.Errorf("sweep volumes: %w", err)
	}
	if n > 0 {
		m.deps.Logf("sweep volumes: reclaimed %d orphan volume unit(s)", n)
	}
	return nil
}

// markBuildingVolume / unmarkBuildingVolume manage the in-flight-create keep-set.
//
// More necessary than the Overlay equivalent: an Overlay's row is inserted BEFORE
// its unit is allocated, so `building` there only guards a stale keep-set snapshot,
// whereas volume creation is BYTES-FIRST — the ID must name the storage before any
// row exists — so a healthy in-flight create presents as "bytes, no row" even to a
// perfectly fresh snapshot.
func (m *Mediator) markBuildingVolume(id model.VolumeID) {
	m.mu.Lock()
	m.buildingVolumes[id] = true
	m.mu.Unlock()
}

func (m *Mediator) unmarkBuildingVolume(id model.VolumeID) {
	m.mu.Lock()
	delete(m.buildingVolumes, id)
	m.mu.Unlock()
}

func (m *Mediator) buildingVolumeIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.buildingVolumes))
	for id := range m.buildingVolumes {
		out = append(out, string(id))
	}
	return out
}
