package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/runtime"
	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/volume"
)

// Volume verbs (the Volume subsystem), owned by the lifecycle mediator like every other verb: the
// mediator reads the authoritative store and drives the backend's mechanisms, and
// the backend never reads the store (the lifecycle).
//
// A Volume's whole point is that its lifecycle is DECOUPLED from every sandbox
// (the Volume subsystem) — created before one exists, surviving after the last one is gone,
// operable while nothing has it mounted. That is why none of these verbs takes a
// SandboxID, and why the content path below goes through the backend's own
// root-confined accessor rather than through the transport data plane `fs`: `fs` lives inside a guest
// and is reachable only through a live placement, so an unmounted volume has no
// route through it at all; the backend accessor is the only valid path in that state.
//
// None of them publishes an event. A volume is not a sandbox, so its CRUD is not a
// the lifecycle state machine lifecycle transition — the same rule Template CRUD follows (health and reclamation).

// ErrVolumeCapBelowUsage is the Volume subsystem / the control-plane API PATCH refusal: the requested cap is not
// STRICTLY greater than the volume's current usage.
//
// It is 400 rather than 507 (the API error contract): nothing is full and no retry helps — the
// request itself is unacceptable. And it is refused even though THE SUBSTRATE WOULD
// HAPPILY ACCEPT IT, which is the whole reason this check exists in the engine.
// Measured (volume cap enforcement): a cap below current usage wedges the storage unit
// PERMANENTLY — afterwards even `mkdir` and truncating an existing file to zero
// return ENOSPC, and the quota operation ITSELF returns ENOSPC, so the cap can
// neither be removed nor raised, not even back above current usage. The only exit
// is deleting the guest's bytes, and the external API gives the engine no way to do that on the
// caller's behalf. So accepting would manufacture a state the control plane cannot
// leave.
//
// The boundary case is included deliberately: `cap == usage` is ACCEPTED by the substrate and
// stored, but the very next write gets ENOSPC — so it means FULL, not "exactly enough", and a
// `>=` check would hand the caller a 200 and a volume that can never take another byte.
var ErrVolumeCapBelowUsage = errors.New("lifecycle: volume cap must be strictly greater than current usage")

// ErrVolumeCapUnsupported is the API error contract 400 for a cap the chosen backend cannot
// enforce. It wraps the backend's own typed refusal so the boundary does not have to
// re-derive the verdict, and it exists as a distinct sentinel because "this backend
// cannot do that" is a different thing from "that number is wrong".
var ErrVolumeCapUnsupported = errors.New("lifecycle: backend cannot enforce a capacity cap")

// CreateVolume allocates a volume: gate, then BYTES, then the row (the Volume subsystem
// bytes-first / health and reclamation existence-is-state).
//
// The ORDER is forced by existence-is-state: a `volumes` row has no not-ready value, so it may
// only be inserted once the storage exists, and a crash before the insert leaves orphan BYTES
// for the sweep instead of a row naming storage that is not there. The in-flight keep-set
// covers that window, and it matters MORE here than for an Overlay unit, where the row is
// inserted first — here a healthy in-flight create presents as "bytes, no row" even to a
// completely fresh keep-set.
//
// CAPABILITIES ARE ASKED BEFORE AN ID IS MINTED OR A BYTE MOVES (the volume backend seam) — see
// deriveVolume for the residue a late check leaves.
//
// admitMu spans {gate → allocate → insert}, which is the same
// {gate → make the promise visible} discipline create uses. Unlike Promote's byte
// copy the middle step here is bounded metadata work (allocate the unit, write the
// cap), so holding the node-global gate across it does not serialize the node behind
// a long operation.
//
// THAT PREMISE IS SPECIFIC TO THE LOCAL ARM: "bounded metadata work" is true of a btrfs
// ioctl and false of a network round trip. A remote backend falsifies it here and at
// SetCapacity / Snapshot / Clone / UncappedBytes, where the same node-global gate is held
// across a backend call. A backend whose operation is not bounded metadata work must use a
// reservation shape like Promote instead of holding the node-global gate across the call.
func (m *Mediator) CreateVolume(ctx context.Context, capBytes int64) (model.VolumeID, error) {
	return m.CreateVolumeWithKey(ctx, capBytes, "")
}

// CreateVolumeWithKey is CreateVolume with a durable caller-supplied
// idempotency key. Repeating the same semantic request returns the original
// Volume; reusing the key with a different cap conflicts deterministically.
func (m *Mediator) CreateVolumeWithKey(ctx context.Context, capBytes int64, key model.IdempotencyKey) (resultID model.VolumeID, resultErr error) {
	if capBytes < 0 {
		return "", fmt.Errorf("create volume: cap must be non-negative (0 = uncapped), got %d", capBytes)
	}
	claim, err := m.claimOperation(ctx, "volume.create", key, struct {
		CapByte int64 `json:"cap_byte"`
	}{capBytes})
	if err != nil {
		return "", err
	}
	if replay, err := operationReplayID(claim, model.ObjectVolume); err != nil {
		return "", err
	} else if replay != "" {
		return model.VolumeID(replay), nil
	}
	storeCtx := ctx
	opctx, ownedClaim, stopLease := m.startOperationLease(ctx, claim)
	claim = ownedClaim
	ctx = opctx
	defer stopLease()
	claim, err = m.prepareOperation(ctx, claim)
	if err != nil {
		return "", err
	}
	defer func() { m.failOperation(storeCtx, claim, resultErr) }()
	selected, err := m.defaultVolumeBackend()
	if err != nil {
		return "", fmt.Errorf("create volume: %w", err)
	}
	be := selected.backend
	// The idempotency-key verdict precedes capability validation so a reused key
	// always conflicts deterministically, independent of current provider state.
	if capBytes > 0 && !be.Capabilities().Quota {
		return "", fmt.Errorf("create volume: %w: backend %q reports no capacity-cap capability", ErrVolumeCapUnsupported, be.Name())
	}
	if err := m.advanceOperation(ctx, claim, model.OperationMaterializing); err != nil {
		return "", fmt.Errorf("create volume: advance operation: %w", err)
	}

	vid := model.VolumeID("vol_" + randToken())
	if claim.op != nil {
		vid = model.VolumeID(deterministicObjectID("vol_", claim.op.ID))
	}
	rec := model.Volume{ID: vid, Backend: selected.name, CapByte: capBytes, CreatedAt: time.Now()}

	// Protect the unit for the whole create window (atomic transition rollback). Registered BEFORE the
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
	err = m.deps.Store.CreateVolume(ctx, rec)
	m.admitMu.Unlock()
	if err != nil {
		// The bytes exist and the row does not. Reclaim now rather than leaving it to
		// the sweep — the sweep is the crash backstop, not the ordinary unwind path.
		if derr := be.DestroyVolume(ctx, string(vid)); derr != nil {
			m.deps.Logf("create volume: unwind %s left bytes behind (the orphan sweep will retry): %v", vid, derr)
		}
		return "", fmt.Errorf("create volume: insert record: %w", err)
	}
	if err := m.advanceOperation(ctx, claim, model.OperationMaterialized); err != nil {
		return "", fmt.Errorf("create volume: record materialized operation: %w", err)
	}
	if err := m.succeedOperation(ctx, claim, model.OperationResult{Kind: model.ObjectVolume, ID: string(vid)}); err != nil {
		return "", fmt.Errorf("create volume: commit operation result: %w", err)
	}
	return vid, nil
}

// VolumeInfo is the control-plane API inspect shape: the record plus the backend's own usage
// reading.
//
// Usage comes from the backend and the cap comes from the RECORD, and they are not
// interchangeable. The backend's limit read-back is diagnostic only, because the
// substrate's `none ↔ 0` mapping makes "never capped" and "deliberately uncapped"
// indistinguishable from storage (volume cap enforcement, and see RepairVolumeCaps). Reporting
// the record's value keeps inspect honest about what the engine PROMISED, which is also
// what the admission gate counts.
type VolumeInfo struct {
	Volume model.Volume
	// UsedByte is the backend's own statement of bytes consumed, or NIL when the
	// backend could not be read.
	//
	// A POINTER, because zero is a legitimate answer for an empty volume and the
	// the control-plane API contract says the key is absent IFF the number is unreadable. A value
	// type cannot carry both facts: it would make "empty" and "unreadable" the same
	// answer, and the caller's use for this number — choosing a new cap STRICTLY
	// greater than it — needs them apart (an unreadable usage must not look like a
	// volume with all its capacity free). Reachable on the plain-directory arm,
	// where a fresh volume's walk really is 0.
	UsedByte *int64
}

// GetVolume reads one volume's record and asks its backend for current usage
// (the control-plane API inspect).
func (m *Mediator) GetVolume(ctx context.Context, id model.VolumeID) (VolumeInfo, error) {
	rec, be, err := m.backendForVolume(ctx, id)
	if err != nil {
		return VolumeInfo{}, fmt.Errorf("inspect volume: %w", err)
	}
	info := VolumeInfo{Volume: rec}
	used, _, usageErr := be.Usage(ctx, string(id))
	if usageErr != nil {
		// Usage is REPORTING, not a decision here, so an unreadable backend degrades
		// the response rather than failing the read. The strict-inequality check in
		// SetVolumeCapacity treats the same failure as fatal, because there it IS the
		// decision.
		m.deps.Logf("inspect volume %s: usage unavailable: %v", id, usageErr)
	} else {
		info.UsedByte = &used
	}
	return info, nil
}

// ListVolumes returns every volume record (the control-plane API list).
func (m *Mediator) ListVolumes(ctx context.Context) ([]model.Volume, error) {
	volumes, err := m.deps.Store.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, rec := range volumes {
		if err := m.validateVolumeRecord(rec); err != nil {
			return nil, fmt.Errorf("list volumes: %w", err)
		}
	}
	return volumes, nil
}

// SetVolumeCapacity is the control-plane API PATCH write point — the cap's only write point
// besides create.
//
// Three disciplines, all from the Volume subsystem, and none of them optional:
//
//  1. IT WORKS WHETHER OR NOT ANYTHING HAS THE VOLUME MOUNTED, which is the exact
//     opposite of a sandbox cap (editable only while `paused`). The reason is
//     mechanical: a sandbox cap re-materializes at the next placement, while a
//     volume HAS no placement of its own — so "defer to the next placement" would
//     mean NEVER for an unmounted volume. There is nothing to defer, so there is no
//     "pause it first" refusal to make.
//  2. A RAISE RE-CLEARS ADMISSION (resource admission: any write to a cap changes Σ at THIS
//     commit, not at some later placement), and may be refused with 507. A lowering
//     does not need the gate — Σ only shrinks.
//  3. LOWERING BELOW CURRENT USAGE IS REFUSED, with a STRICT inequality. See
//     ErrVolumeCapBelowUsage: the substrate accepts such a cap and is then wedged
//     permanently, and `cap == usage` means FULL rather than "exactly enough".
//
// It is deliberately NOT subject to the write-regime invariant that governs direct
// content changes: this moves an allocation ceiling, not bytes, so there is nothing
// for a concurrent writer to tear (the Volume subsystem).
func (m *Mediator) SetVolumeCapacity(ctx context.Context, id model.VolumeID, capBytes int64) error {
	if capBytes < 0 {
		return fmt.Errorf("patch volume: cap must be non-negative (0 = uncapped), got %d", capBytes)
	}
	rec, be, err := m.backendForVolume(ctx, id)
	if err != nil {
		return fmt.Errorf("patch volume: %w", err)
	}
	if capBytes > 0 && !be.Capabilities().Quota {
		return fmt.Errorf("patch volume: %w: backend %q reports no capacity-cap capability", ErrVolumeCapUnsupported, be.Name())
	}

	// REMOVING the cap is refused while the disk axis is METERED, and this is resource admission's
	// rule rather than a new one (admitVolume's ErrCapRequired): PATCH is the same
	// promise written at a different time, so accepting 0 here manufactures exactly the
	// volume create that would have been refused — and it is the only route by which the
	// btrfs arm's resource admission second term can become non-zero on a metered node.
	//
	// "Removing a cap is always safe" was true of the WEDGE (0 only widens what the unit
	// may hold, so it can never strand bytes above the ceiling) and false of the BUDGET.
	// Both halves matter, so the wedge check below still skips 0 while this one does not.
	//
	// Gated on the axis being metered, not applied unconditionally: on an unmetered node
	// an uncapped volume is legal by resource admission's own terms, so refusing there would reject
	// a valid unmetered configuration.
	if capBytes == 0 && m.deps.NodeCapacity.DiskByte > 0 {
		return fmt.Errorf("patch volume: %w: this node meters the disk axis, so a volume may not be left uncapped (resource admission, the same reason create refuses one)", ErrCapRequired)
	}

	// The strict-inequality check, only when a cap is actually being imposed. Removing
	// the cap (0) can only widen what the unit may hold, so it cannot wedge anything.
	if capBytes > 0 {
		used, _, err := be.Usage(ctx, string(id))
		if err != nil {
			// FAIL CLOSED. Unlike inspect, usage here IS the decision: admitting a cap
			// without knowing usage risks the irreversible wedge described on
			// ErrVolumeCapBelowUsage, and that state has no exit through the API at all.
			return fmt.Errorf("patch volume: cannot read current usage of %s, refusing to set a cap blind (an under-usage cap wedges the volume irreversibly, the Volume subsystem): %w", id, err)
		}
		if capBytes <= used {
			return fmt.Errorf("patch volume: %w: requested %d, current usage %d — a cap at or below usage wedges the volume irreversibly (the Volume subsystem / volume cap enforcement)",
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
	// (resource admission). The reverse order would leave a record promising a cap the storage does
	// not carry — and that divergence is UNDETECTABLE afterwards (volume cap enforcement), so
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

// DeleteVolume removes the record, then reclaims the bytes (the Volume subsystem / the transition contract).
//
// ROW FIRST, and that order is load-bearing. The row delete is where every
// reference-protection refusal surfaces (the API error contract's Volume 409s), so destroying bytes first
// would mean a delete the store then REFUSES has already destroyed the data it was
// refusing to let go — the guard would be worse than absent.
//
// A crash between the two leaves orphan bytes, which is exactly the deferral the transition contract
// licenses and the orphan sweep retries. Byte reclamation failing does NOT fail the
// verb, for the same reason: the record is gone, so the sweep is the retry.
//
// NOT idempotent-by-swallowing: an absent volume reports store.ErrVolumeNotFound
// (the API error contract). Folding it into success is how an external implementation made
// "the guard refused this delete" indistinguishable from "the delete succeeded".
func (m *Mediator) DeleteVolume(ctx context.Context, id model.VolumeID) error {
	_, be, err := m.backendForVolume(ctx, id)
	if err != nil {
		return fmt.Errorf("delete volume: %w", err)
	}
	if err := m.deps.Store.DeleteVolume(ctx, id); err != nil {
		return fmt.Errorf("delete volume: %w", err)
	}
	if err := be.DestroyVolume(ctx, string(id)); err != nil {
		m.deps.Logf("delete volume %s: byte reclamation failed, deferring to the orphan sweep (the transition contract): %v", id, err)
	}
	return nil
}

// ImportVolume streams src into the volume at the root-relative path (the control-plane API
// POST /v1/volumes/{id}/import).
//
// NEEDS NO SANDBOX: this and ExportVolume are how the decoupling in the file header
// becomes true in SUBSTANCE instead of only in the record layer (the Volume subsystem) — seeding a volume
// before any sandbox exists, and reading one after the last is gone, have no other route.
//
// It STREAMS through a fixed buffer rather than materializing the payload, matching
// the control-plane API requirement that the bytes never sit whole at either end.
//
// Any existing file at that path is REMOVED first, so an import is a replacement
// rather than an overwrite-in-place: writing a shorter payload over a longer file
// would otherwise leave the old tail attached and produce a file that is neither
// the old nor the new content.
func (m *Mediator) ImportVolume(ctx context.Context, id model.VolumeID, path string, src io.Reader) (int64, error) {
	// The write-regime gate (the Volume subsystem). On IMPORT and not on EXPORT because the invariant is
	// about TEARING, and a read tears nothing (writeregime.go).
	//
	// The marker it publishes is held for the WHOLE stream, not just the decision —
	// the sweep must not reclaim these bytes at byte 4 of 40 MiB either.
	release, err := m.admitDirectWrite(ctx, id)
	if err != nil {
		return 0, err
	}
	defer release()

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
				// result (resource admission), and the partial content is what the caller asked to write
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

// ExportVolume streams the volume's file at the root-relative path into dst (the control-plane API
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

// StatVolume exposes root-confined LSTAT metadata without requiring a
// Sandbox Placement.
func (m *Mediator) StatVolume(ctx context.Context, id model.VolumeID, name string) (volume.FileInfo, error) {
	r, err := m.openVolumeRoot(ctx, id)
	if err != nil {
		return volume.FileInfo{}, err
	}
	defer r.Close()
	fi, err := r.Stat(name)
	if err != nil {
		return volume.FileInfo{}, fmt.Errorf("stat volume %s: %s: %w", id, name, err)
	}
	return fi, nil
}

// LstatVolume reports the final entry itself, including symlink metadata.
func (m *Mediator) LstatVolume(ctx context.Context, id model.VolumeID, name string) (volume.FileInfo, error) {
	r, err := m.openVolumeRoot(ctx, id)
	if err != nil {
		return volume.FileInfo{}, err
	}
	defer r.Close()
	fi, err := r.Lstat(name)
	if err != nil {
		return volume.FileInfo{}, fmt.Errorf("lstat volume %s: %s: %w", id, name, err)
	}
	return fi, nil
}

// ListVolume enumerates one root-relative directory.
func (m *Mediator) ListVolume(ctx context.Context, id model.VolumeID, name string) ([]volume.FileInfo, error) {
	r, err := m.openVolumeRoot(ctx, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	list, err := r.List(name)
	if err != nil {
		return nil, fmt.Errorf("list volume %s: %s: %w", id, name, err)
	}
	return list, nil
}

// ReadVolume reads a bounded range. A non-positive length reads to EOF, while
// the endpoint still bounds the allocation by the file's reported size.
func (m *Mediator) ReadVolume(ctx context.Context, id model.VolumeID, name string, offset, length int64) ([]byte, error) {
	if offset < 0 {
		return nil, fmt.Errorf("read volume: offset must be non-negative")
	}
	r, err := m.openVolumeRoot(ctx, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	fi, err := r.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("read volume %s: %s: %w", id, name, err)
	}
	if fi.IsDir {
		return nil, fmt.Errorf("read volume %s: %s is a directory", id, name)
	}
	if offset >= fi.Size {
		return []byte{}, nil
	}
	if length <= 0 || length > fi.Size-offset {
		length = fi.Size - offset
	}
	if length > 64<<20 {
		return nil, fmt.Errorf("read volume %s: requested range is too large", id)
	}
	buf := make([]byte, length)
	n, err := r.ReadAt(name, buf, offset)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read volume %s: %s: %w", id, name, err)
	}
	return buf[:n], nil
}

// WriteVolume writes one range under the same direct-write admission regime as
// streamed import.
func (m *Mediator) WriteVolume(ctx context.Context, id model.VolumeID, name string, offset int64, data []byte) (int64, error) {
	if offset < 0 {
		return 0, fmt.Errorf("write volume: offset must be non-negative")
	}
	release, err := m.admitDirectWrite(ctx, id)
	if err != nil {
		return 0, err
	}
	defer release()
	r, err := m.openVolumeRoot(ctx, id)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	if err := mkdirChain(r, name); err != nil {
		return 0, fmt.Errorf("write volume %s: prepare %s: %w", id, name, err)
	}
	n, err := r.WriteAt(name, data, offset)
	if err != nil {
		return int64(n), fmt.Errorf("write volume %s: %s: %w", id, name, err)
	}
	return int64(n), nil
}

func (m *Mediator) MkdirVolume(ctx context.Context, id model.VolumeID, name string, perm fs.FileMode) (volume.FileInfo, error) {
	release, err := m.admitDirectWrite(ctx, id)
	if err != nil {
		return volume.FileInfo{}, err
	}
	defer release()
	r, err := m.openVolumeRoot(ctx, id)
	if err != nil {
		return volume.FileInfo{}, err
	}
	defer r.Close()
	if err := r.Mkdir(name, perm); err != nil {
		return volume.FileInfo{}, fmt.Errorf("mkdir volume %s: %s: %w", id, name, err)
	}
	return r.Stat(name)
}

func (m *Mediator) MoveVolume(ctx context.Context, id model.VolumeID, oldName, newName string) (volume.FileInfo, error) {
	release, err := m.admitDirectWrite(ctx, id)
	if err != nil {
		return volume.FileInfo{}, err
	}
	defer release()
	r, err := m.openVolumeRoot(ctx, id)
	if err != nil {
		return volume.FileInfo{}, err
	}
	defer r.Close()
	if err := mkdirChain(r, newName); err != nil {
		return volume.FileInfo{}, fmt.Errorf("move volume %s: prepare %s: %w", id, newName, err)
	}
	if err := r.Rename(oldName, newName); err != nil {
		return volume.FileInfo{}, fmt.Errorf("move volume %s: %s to %s: %w", id, oldName, newName, err)
	}
	return r.Stat(newName)
}

func (m *Mediator) RemoveVolumePath(ctx context.Context, id model.VolumeID, name string) error {
	release, err := m.admitDirectWrite(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	r, err := m.openVolumeRoot(ctx, id)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.Remove(name); err != nil {
		return fmt.Errorf("remove volume %s: %s: %w", id, name, err)
	}
	return nil
}

// mkdirChain creates every parent directory of a root-relative file path, tolerating
// ones that already exist.
//
// It walks the segments rather than asking for a recursive create because Root has no
// MkdirAll, and it should not: the accessor's value is that each operation is one
// root-confined resolution, and a recursive helper on the seam would be a second place
// where path semantics could disagree with the first. Confinement is unaffected — every
// segment goes through the same resolution as any other path.
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
// create-if-absent by contract (the Volume subsystem), and although OpenRoot is not, keeping the
// authority check in the mediator is what makes an unknown ID a clean 404 instead
// of whatever the storage layer happens to say (the lifecycle: the store is authoritative, the
// backend answers about bytes).
func (m *Mediator) openVolumeRoot(ctx context.Context, id model.VolumeID) (volume.Root, error) {
	_, be, err := m.backendForVolume(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("volume content: %w", err)
	}
	r, err := be.OpenRoot(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("volume content: open %s: %w", id, err)
	}
	return r, nil
}

// SweepOrphanVolumes reclaims volume storage with no live record (atomic transition rollback) — the
// Volume subsystem mirror of SweepOrphanOverlays. The mediator reads the authoritative store
// for the live set, snapshots the in-flight keep-set as LATE as possible, and hands
// both to the backend, which enumerates only its own storage (the lifecycle: subsystems never
// read the store).
//
// Skip-and-log per atomic transition rollback: a sweep failure is not fatal and retries next pass.
//
// ITS POSITION IN THE BOOT SEQUENCE IS LOAD-BEARING, not housekeeping — it must run
// BEFORE any storage unit is created, because sweeping only on the periodic tick leaves a
// window in which a new Volume inherits a leaked row's ID and cap. ReclaimQuotaRows
// states the measured consequence.
func (m *Mediator) SweepOrphanVolumes(ctx context.Context) error {
	backends, err := m.configuredVolumeBackends()
	if err != nil {
		return fmt.Errorf("sweep volumes: %w", err)
	}
	if len(backends) == 0 {
		if err := m.validateDurableVolumeProviders(ctx); err != nil {
			return fmt.Errorf("sweep volumes: %w", err)
		}
		return nil
	}
	// ENUMERATE FIRST, then decide. The order is what closes a MEASURED window: with the
	// diff done inside the backend from two pre-read sets, a volume whose whole creation
	// fit between the `live` read and the `building` read was in neither, and the backend
	// deleted the bytes of a FULLY COMMITTED volume (measured: 3 of 6 snapshots destroyed
	// under 458 overlapping sweeps).
	//
	// Enumerating first means every candidate's bytes already existed before any of the
	// checks below ran, which is what makes those checks conclusive.
	type enumeration struct {
		provider string
		backend  volume.Backend
		units    []string
	}
	enumerated := make([]enumeration, 0, len(backends))
	for _, selected := range backends {
		units, enumErr := selected.backend.ListUnitIDs(ctx)
		if enumErr != nil {
			// Reclaim NOTHING: an unreadable enumeration is not evidence that anything is an
			// orphan (the Volume subsystem — never conclude "no owner" from a failed read).
			return fmt.Errorf("sweep volumes: enumerate provider %q: %w", selected.name, enumErr)
		}
		enumerated = append(enumerated, enumeration{provider: selected.name, backend: selected.backend, units: units})
	}

	vols, err := m.deps.Store.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("sweep volumes: list volumes: %w", err)
	}
	keep := make(map[string]model.Volume, len(vols))
	for _, v := range vols {
		// Validate every durable reference before any bytes are reclaimed. An unknown
		// name is not evidence that its unit is orphaned; failing closed protects it.
		if err := m.validateVolumeRecord(v); err != nil {
			return fmt.Errorf("sweep volumes: %w", err)
		}
		keep[string(v.ID)] = v
	}

	reclaimed := 0
	var failed []string
	for _, batch := range enumerated {
		for _, id := range batch.units {
			if _, ok := keep[id]; ok {
				continue
			}
			// A candidate with no row. Before destroying it, THREE checks, the first two of
			// which have a load-bearing ORDER:
			//
			//  1. the in-flight sets — an in-flight create or a direct write in progress is
			//     "bytes, no row" by construction, not an orphan;
			//  2. a FRESH per-candidate store read; and
			//  3. the FK-independent mount-table read (see below), which needs 2 to have run
			//     first to be worth anything.
			//
			// Check 1 before check 2 is what makes that pair conclusive. The in-flight mark is
			// dropped only AFTER the row is inserted, so a candidate that is NOT marked here
			// has necessarily already committed its row — and check 2 then finds it. A
			// candidate that IS marked is skipped outright. There is no third case, so no
			// committed volume can be reclaimed, and no lock is held across a store call.
			//
			// EVERY read here fails CLOSED: an error skips the candidate, and is never allowed
			// to read as "no owner" (the Volume subsystem).
			if m.isVolumeInFlight(model.VolumeID(id)) {
				continue
			}
			if _, err := m.deps.Store.GetVolume(ctx, model.VolumeID(id)); err == nil {
				// Its row landed after the snapshot above: not an orphan. A row naming
				// another provider also protects this unit; the owner backend gets to
				// decide how its own bytes are interpreted.
				continue
			} else if !errors.Is(err, store.ErrVolumeNotFound) {
				// Unreadable → skip. An error is not evidence of absence.
				m.deps.Logf("sweep volumes: %s (provider %s): record unreadable, skipping: %v", id, batch.provider, err)
				continue
			}
			// 3. A LIVE-MOUNT CHECK INDEPENDENT OF ANY FK (the Volume subsystem safety requirement): skip any
			// unit whose id appears in ANY mount row, live or not.
			//
			// It is asked of the CANDIDATE UNIT ID, and that placement is load-bearing.
			// Asking it of the volumes that HAVE records instead would be inert — every such
			// id is already kept by the loop above — and would answer a question no FK leaves
			// open: `mounts.volume REFERENCES volumes(id) ON DELETE RESTRICT` already makes
			// "record present" and "mount row present" co-extensive AS LONG AS THE FK HOLDS.
			// This check does NOT trust that: the candidates reaching this line have a
			// CONFIRMED ABSENT record, so a mount row found here is exactly the FK-violating
			// state the requirement names — a row still pointing at bytes nothing else
			// vouches for.
			//
			// FK RESTRICT protects the ROW; this protects the BYTES, and the bytes are what a
			// mis-sweep destroys — silently, with the guest's writes still returning success
			// into an unlinked inode (volume sweep safety: five ways of pulling a live volume, none
			// killed the sandbox, three produced no errno at all).
			if rows, err := m.deps.Store.ListMountsByVolume(ctx, model.VolumeID(id)); err != nil {
				// SKIP, never "no owner": measured, a bulk read fails wholesale and returns zero
				// rows under exactly the concurrent churn a sweep runs in (the Volume subsystem). Reclaiming on a
				// failed read would be indistinguishable from reclaiming on an empty table.
				m.deps.Logf("sweep volumes: %s (provider %s): mount table unreadable, skipping: %v", id, batch.provider, err)
				continue
			} else if len(rows) > 0 {
				m.deps.Logf("sweep volumes: %s (provider %s): no record but %d mount row(s) still name it, skipping", id, batch.provider, len(rows))
				continue
			}
			// Genuinely an orphan: bytes exist, no row, no mount row, nothing in flight.
			if err := batch.backend.DestroyVolume(ctx, id); err != nil {
				// CONTINUE, do not abort: this is a best-effort reclamation path and one stuck
				// unit must not leave every other leak in place. The sweep is idempotent, so
				// whatever failed is retried next pass.
				failed = append(failed, fmt.Sprintf("%s (%s): %v", id, batch.provider, err))
				continue
			}
			reclaimed++
		}
	}
	if reclaimed > 0 {
		m.deps.Logf("sweep volumes: reclaimed %d orphan volume unit(s)", reclaimed)
	}
	if len(failed) > 0 {
		return fmt.Errorf("sweep volumes: reclaim unit(s): %s", strings.Join(failed, "; "))
	}
	return nil
}

// isVolumeInFlight reports whether a volume has an in-flight create/derive or an
// in-flight direct write. Both are "bytes exist, no row (yet)" by construction.
//
// One accessor over both maps under ONE acquisition of mu, rather than two calls: a
// candidate must not be able to slip between them by finishing one and starting the
// other.
func (m *Mediator) isVolumeInFlight(id model.VolumeID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buildingVolumes[id] || m.inFlightWrites[id] > 0
}

// deleteMountsForCleanup drops a sandbox's mount rows on a cleanup path where the
// caller cannot fail (reconcile's skip-and-log discipline, atomic transition rollback).
//
// It logs rather than returning, because the caller's next step is a record delete
// whose own error is already logged-and-skipped — but the log is not decoration: if
// this fails, RESTRICT blocks that delete on every subsequent boot and the log is the
// only place that will ever say why the record is undeletable.
func (m *Mediator) deleteMountsForCleanup(ctx context.Context, id model.SandboxID) {
	if err := m.deps.Store.DeleteMounts(ctx, id); err != nil {
		m.deps.Logf("reconcile: %s: delete mount rows FAILED (%v) — the record delete will be blocked by mounts.sandbox RESTRICT on this and every future boot", id, err)
	}
}

// materializeMounts prepares every volume this sandbox's CURRENT mount table names,
// returning the backend handles (for later Release) and the provider specs (for the
// launch).
//
// The two halves are returned separately because they have different lifetimes: the
// specs are consumed once, by this launch, while the handles live in liveState until
// pause or destroy releases them.
//
// PARTIAL FAILURE RELEASES WHAT IT ALREADY TOOK. Without that, a cold start failing on
// the third of four volumes leaves two materializations outstanding with no handle
// anywhere — on the local arm that is harmless (Release is a no-op), but on the FUSE
// arm it strands a process, which is exactly the class of leak the handle exists to
// prevent.
func (m *Mediator) materializeMounts(ctx context.Context, id model.SandboxID) ([]*materializedVolume, []runtime.MountSource, error) {
	rows, err := m.deps.Store.ListMounts(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("read mount table: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}
	plan, err := CompileMountPlan(rows)
	if err != nil {
		return nil, nil, err
	}
	rows = plan.Rows
	if backends, err := m.configuredVolumeBackends(); err != nil {
		return nil, nil, fmt.Errorf("resolve volume providers for sandbox %s: %w", id, err)
	} else if len(backends) == 0 {
		return nil, nil, fmt.Errorf("sandbox %s has %d mount row(s) but no volume backend is registered", id, len(rows))
	}
	mounts := make([]*materializedVolume, 0, len(rows))
	specs := make([]runtime.MountSource, 0, len(rows))
	for _, mt := range rows {
		_, be, err := m.backendForVolume(ctx, mt.Volume)
		if err != nil {
			m.releaseMounts(ctx, mounts)
			return nil, nil, fmt.Errorf("materialize volume %s for %s: %w", mt.Volume, id, err)
		}
		mz, err := be.Materialize(ctx, string(mt.Volume))
		if err != nil {
			m.releaseMounts(ctx, mounts)
			return nil, nil, fmt.Errorf("materialize volume %s for %s: %w", mt.Volume, id, err)
		}
		mounts = append(mounts, &materializedVolume{backend: be, materialized: mz})
		specs = append(specs, runtime.MountSource{
			SourcePath: mz.SourcePath,
			Dest:       mt.Path,
			// The ONE bit of the access mode the provider needs. Exclusivity was already
			// decided in the control plane (the row's existence), so it does not cross —
			// and the direction of this bit is load-bearing (runtime.MountSource.ReadOnly).
			ReadOnly: mt.Access.ReadOnly(),
		})
	}
	return mounts, specs, nil
}

// adoptMounts re-recognizes the volume materializations of a sandbox being ADOPTED
// after a control-plane crash (atomic transition rollback) — rootfs.AdoptMerged's the Volume subsystem counterpart.
//
// It uses AdoptMaterialized, NOT Materialize, and the difference is load-bearing.
// Materialize is create-if-absent, so it would invent an empty unit for a volume whose
// storage is gone — and then report the sandbox as cleanly adopted while its guest holds a
// gofer onto the OLD, now-unlinked directory, indistinguishable from a healthy adopt while
// the guest's writes keep succeeding into bytes no host path can reach (the measured
// mis-sweep blast radius, volume sweep safety). Adopt REFUSES an absent unit instead, which
// turns that case into "uncertain → protect".
//
// A missing volume is therefore an ERROR here, deliberately, even though a fresh cold
// start would recreate it: the guest that is ALREADY RUNNING cannot be re-pointed at a
// new directory, so "recreate it empty" is not a recovery for a live sandbox. Failing
// the adopt demotes nothing and tears nothing down — the caller keeps the sandbox in the
// keep-set and retries — whereas a resume later will legitimately create-if-absent and
// restart the guest clean (the Volume subsystem: data lost but recoverable).
func (m *Mediator) adoptMounts(ctx context.Context, id model.SandboxID) ([]*materializedVolume, error) {
	rows, err := m.deps.Store.ListMounts(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	plan, err := CompileMountPlan(rows)
	if err != nil {
		return nil, err
	}
	rows = plan.Rows
	if backends, err := m.configuredVolumeBackends(); err != nil {
		return nil, fmt.Errorf("resolve volume providers for sandbox %s: %w", id, err)
	} else if len(backends) == 0 {
		return nil, fmt.Errorf("sandbox %s has %d mount row(s) but no volume backend is registered", id, len(rows))
	}
	mounts := make([]*materializedVolume, 0, len(rows))
	for _, mt := range rows {
		_, be, err := m.backendForVolume(ctx, mt.Volume)
		if err != nil {
			m.releaseMounts(ctx, mounts)
			return nil, fmt.Errorf("volume %s of sandbox %s: %w", mt.Volume, id, err)
		}
		mz, err := be.AdoptMaterialized(ctx, string(mt.Volume))
		if err != nil {
			// Release what was already re-recognized: a partially rebuilt set is worse than
			// none, because it is the shape that makes a later pause skip Releases it owes.
			m.releaseMounts(ctx, mounts)
			return nil, fmt.Errorf("volume %s of sandbox %s: %w", mt.Volume, id, err)
		}
		mounts = append(mounts, &materializedVolume{backend: be, materialized: mz})
	}
	return mounts, nil
}

// releaseMounts ends every materialization in the slice, best-effort.
//
// Best-effort is correct rather than lazy: Release PROMISES idempotency and tolerance
// of an already-gone target (the Volume subsystem), so a failure here cannot mean "the bytes are still
// live" — the bytes survive Release by definition. It is logged so a FUSE arm's stuck
// unmount is visible without failing a pause that has already committed.

func (m *Mediator) releaseMounts(ctx context.Context, mounts []*materializedVolume) {
	for _, mounted := range mounts {
		if mounted == nil || mounted.materialized == nil || mounted.backend == nil {
			continue
		}
		if err := mounted.backend.Release(ctx, mounted.materialized); err != nil {
			m.deps.Logf("release volume materialization %s: %v", mounted.materialized.Volume, err)
		}
	}
}

// mountDests reads one sandbox's volume mount destinations from the AUTHORITATIVE
// mount table.
//
// It exists so the Rootfs subsystem can be HANDED the set rather than asking for it: the mediator
// already holds the mount table and already calls Promote, so threading the
// destinations through keeps the Rootfs subsystem from ever needing to consult the Volume subsystem and no subsystem
// edge appears (the lifecycle / rootfs mount constraints).
func (m *Mediator) mountDests(ctx context.Context, id model.SandboxID) ([]string, error) {
	rows, err := m.deps.Store.ListMounts(ctx, id)
	if err != nil {
		return nil, err
	}
	dests := make([]string, 0, len(rows))
	for _, mt := range rows {
		dests = append(dests, mt.Path)
	}
	return dests, nil
}

// markBuildingVolume / unmarkBuildingVolume manage the in-flight-create keep-set.
//
// More necessary than the Overlay equivalent, whose row is inserted BEFORE its unit is
// allocated so `building` there only guards a stale keep-set snapshot — see CreateVolume
// for why bytes-first makes this one cover a healthy create too.
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

// SnapshotVolume / CloneVolume produce a NEW volume from an existing one (the control-plane API
// POST /v1/volumes/{id}/snapshot and .../clone).
//
// They are two entry points over one backend path on the local arm, because that is
// what the substrate is: the same whole-tree operation, differing in the semantic label
// (snapshot lineage). Keeping them separate is what lets a backend supporting only one
// say so through Capabilities.
//
// The product is AN ORDINARY WRITABLE VOLUME with a `parent`, not a distinct read-only
// type (snapshot lineage). Same cardinality argument as AccessMode: writability is a
// per-MOUNT verdict, so a per-volume read-only flag could not be honest — and the measured
// "the writer pays, in both directions" property is only meaningful while a snapshot is
// writable. Immutability is a shared-ro mount row.
//
// `size` IS REQUIRED WHEN DISK IS METERED and is NEVER inherited. Two independent
// reasons: the substrate has nothing to inherit (a snapshot gets a brand-new UNCAPPED
// quota row — measured in four shapes and verified by writing 3× the source's cap into
// one), and on the policy side inheriting would silently DOUBLE the node's declared disk
// claim at every snapshot, which the caller never asked for. resource admission counts the DECLARED
// cap precisely because the real bytes are metadata-level (~16 KiB).
func (m *Mediator) SnapshotVolume(ctx context.Context, srcID model.VolumeID, capBytes int64) (model.VolumeID, error) {
	return m.deriveVolume(ctx, srcID, capBytes, false, "")
}

func (m *Mediator) SnapshotVolumeWithKey(ctx context.Context, srcID model.VolumeID, capBytes int64, key model.IdempotencyKey) (model.VolumeID, error) {
	return m.deriveVolume(ctx, srcID, capBytes, false, key)
}

func (m *Mediator) CloneVolume(ctx context.Context, srcID model.VolumeID, capBytes int64) (model.VolumeID, error) {
	return m.deriveVolume(ctx, srcID, capBytes, true, "")
}

func (m *Mediator) CloneVolumeWithKey(ctx context.Context, srcID model.VolumeID, capBytes int64, key model.IdempotencyKey) (model.VolumeID, error) {
	return m.deriveVolume(ctx, srcID, capBytes, true, key)
}

// deriveVolume is the shared body. The ORDER of its steps is the whole content:
//
//	capability → id → keep-set → gate → BYTES+CAP (one backend call) → row
//
// CAPABILITY FIRST, before an ID is minted and before a byte moves (the volume backend seam). An
// external implementation maps "unsupported" at its HTTP boundary while the error is
// born deep inside, so its 400 arrives only after the source has been fetched, flushed
// and materialized — and combined with mediator-minted IDs, a late check leaves a window
// whose residue no sweep can classify.
//
// THE CAP IS WRITTEN INSIDE THE BACKEND CALL, never by a follow-up PATCH. It is
// necessarily the second substrate operation (no parameter makes the copy carry a cap),
// so the window "bytes exist, cap not set" is REAL — and a missed cap is SILENT AND
// UNDETECTABLE AFTERWARDS, because the limit read maps "never capped" and 0 onto each
// other. The window is covered twice: the in-flight keep-set spans it, and
// RepairVolumeCaps treats "record exists, quota row uncapped" as a REPAIR state.
func (m *Mediator) deriveVolume(ctx context.Context, srcID model.VolumeID, capBytes int64, isClone bool, key model.IdempotencyKey) (resultID model.VolumeID, resultErr error) {
	kind := "snapshot"
	if isClone {
		kind = "clone"
	}
	if capBytes < 0 {
		return "", fmt.Errorf("%s volume: size must be non-negative (0 = uncapped), got %d", kind, capBytes)
	}
	scope := "volume.snapshot"
	if isClone {
		scope = "volume.clone"
	}
	claim, err := m.claimOperation(ctx, scope, key, struct {
		Source  string `json:"source"`
		CapByte int64  `json:"cap_byte"`
	}{string(srcID), capBytes})
	if err != nil {
		return "", err
	}
	if replay, err := operationReplayID(claim, model.ObjectVolume); err != nil {
		return "", err
	} else if replay != "" {
		return model.VolumeID(replay), nil
	}
	storeCtx := ctx
	opctx, ownedClaim, stopLease := m.startOperationLease(ctx, claim)
	claim = ownedClaim
	ctx = opctx
	defer stopLease()
	claim, err = m.prepareOperation(ctx, claim)
	if err != nil {
		return "", err
	}
	defer func() { m.failOperation(storeCtx, claim, resultErr) }()
	// Resolve the source and capability only after claiming the idempotency
	// record. This makes retries deterministic: a reused key is compared with
	// its canonical request before current provider/source state can change the
	// result, and a committed object can always be replayed.
	source, be, err := m.backendForVolume(ctx, srcID)
	if err != nil {
		return "", fmt.Errorf("%s volume: source %s: %w", kind, srcID, err)
	}
	caps := be.Capabilities()
	supported := caps.Snapshot
	if isClone {
		supported = caps.Clone
	}
	if !supported {
		return "", fmt.Errorf("%s volume: %w: backend %q does not support %s", kind, ErrVolumeCapUnsupported, be.Name(), kind)
	}
	if capBytes > 0 && !caps.Quota {
		return "", fmt.Errorf("%s volume: %w: backend %q cannot enforce a capacity cap", kind, ErrVolumeCapUnsupported, be.Name())
	}
	if err := m.advanceOperation(ctx, claim, model.OperationMaterializing); err != nil {
		return "", err
	}

	// The source must exist as a RECORD, checked before an ID is minted and before any
	// bytes move. The record is the authority (resource admission), so a backend that happens to hold
	// bytes for a volume with no row is not a valid source — those bytes are an orphan the
	// sweep is about to reclaim, and deriving from them would produce a child of something
	// that is being deleted.
	newID := model.VolumeID("vol_" + randToken())
	if claim.op != nil {
		newID = model.VolumeID(deterministicObjectID("vol_", claim.op.ID))
	}
	rec := model.Volume{
		ID: newID, Backend: source.Backend, Parent: srcID, CapByte: capBytes, CreatedAt: time.Now(),
	}

	// Registered BEFORE anything exists, so there is no instant in which bytes could sit
	// unprotected — and this keep-set is what spans the bytes-then-cap window.
	m.markBuildingVolume(newID)
	defer m.unmarkBuildingVolume(newID)

	m.admitMu.Lock()
	if err := m.admitVolume(ctx, capBytes); err != nil {
		m.admitMu.Unlock()
		return "", err
	}
	// ONE backend call that both copies the bytes and writes the cap.
	if isClone {
		err = be.Clone(ctx, string(srcID), string(newID), capBytes)
	} else {
		err = be.Snapshot(ctx, string(srcID), string(newID), capBytes)
	}
	if err != nil {
		m.admitMu.Unlock()
		if errors.Is(err, volume.ErrUnsupported) {
			return "", fmt.Errorf("%s volume: %w: %v", kind, ErrVolumeCapUnsupported, err)
		}
		return "", fmt.Errorf("%s volume: %w", kind, err)
	}
	// FAULT INJECTION POINT (test-only): simulate a crash between the backend operation
	// and the row insert — the window that leaves a live, registered, UNCAPPED volume if
	// the cap were not written inside the call above.
	if m.deps.FaultAfterVolumeBytes != nil {
		m.deps.FaultAfterVolumeBytes(newID)
	}
	err = m.deps.Store.CreateVolume(ctx, rec)
	m.admitMu.Unlock()
	if err != nil {
		if derr := be.DestroyVolume(ctx, string(newID)); derr != nil {
			m.deps.Logf("%s volume: unwind %s left bytes behind (the orphan sweep will retry): %v", kind, newID, derr)
		}
		return "", fmt.Errorf("%s volume: insert record: %w", kind, err)
	}
	if err := m.advanceOperation(ctx, claim, model.OperationMaterialized); err != nil {
		return "", err
	}
	if err := m.succeedOperation(ctx, claim, model.OperationResult{Kind: model.ObjectVolume, ID: string(newID)}); err != nil {
		return "", err
	}
	return newID, nil
}

// repairUncappedVolumes is the Volume subsystem / volume cap enforcement REPAIR-STATE pass: a volume whose
// RECORD declares a cap while its storage carries none.
//
// IT IS A REPAIR STATE, NOT A LEGAL ONE: the crash window deriveVolume describes leaves a
// volume that is registered, live, and UNCAPPED, and nothing detects that afterwards on its
// own (the `none ↔ 0` mapping). So the engine must re-assert the record's cap rather than
// trust what it finds.
//
// The direction is always RECORD → STORAGE. The record is the only authority (resource admission), so
// a disagreement is always resolved by rewriting the storage, never by adopting the
// storage's value into the record — the latter would silently ratify exactly the crash
// residue this exists to fix.
//
// BUT THE DIRECTION IS NOT A LICENCE TO WRITE ANY CAP: the same predicate the control-plane API write
// point refuses (cap ≤ current usage, ErrVolumeCapBelowUsage) is refused here too — see
// the branch below.
//
// It is also the reason this pass BACKSTOPS LocalBtrfs.Materialize's fallback: that
// fallback recreates a mis-swept unit as a PLAIN DIRECTORY, which carries no cap, and
// nothing else re-asserts one. The dependency is worth naming because the guard below
// preserves it exactly where it matters — a freshly recreated unit is EMPTY, so its usage
// is 0 and any positive recorded cap is strictly above it.
//
// Runs in the same boot position as the orphan sweep, BEFORE anything creates a unit.
func (m *Mediator) RepairVolumeCaps(ctx context.Context) error {
	vols, err := m.deps.Store.ListVolumes(ctx)
	if err != nil {
		return fmt.Errorf("repair volume caps: list volumes: %w", err)
	}
	repaired := 0
	for _, v := range vols {
		be, err := m.backendForRecord(v)
		if err != nil {
			return fmt.Errorf("repair volume caps: %w", err)
		}
		if !be.Capabilities().Quota {
			continue // this provider cannot carry a cap; its records must be uncapped
		}
		if v.CapByte <= 0 {
			continue // the record says uncapped, so "no cap on storage" is correct
		}
		used, limit, err := be.Usage(ctx, string(v.ID))
		if err != nil {
			// Unreadable: draw no conclusion and change nothing (the same discipline the
			// sweep follows — an unreadable state is not evidence of anything).
			m.deps.Logf("repair volume caps: %s: usage unreadable, skipping: %v", v.ID, err)
			continue
		}
		if limit == v.CapByte {
			continue
		}
		// A DISAGREEMENT IS NOT ENOUGH TO ACT ON: the record's cap must also still be
		// applicable. At or below what the unit already holds, re-asserting it does not
		// repair the volume, it WEDGES it irreversibly — the measured mechanism is on
		// ErrVolumeCapBelowUsage (volume cap enforcement).
		//
		// It is the SAME PREDICATE SetVolumeCapacity refuses at the control-plane API write point, and it
		// has to be checked here too: this path reaches the identical backend call without
		// passing that guard, so a recovery pass would otherwise be the way around a refusal
		// that exists precisely because the resulting state has no exit. The window that
		// produces the input is real — Volume storage's load-bearing window leaves "record says cap=N,
		// quota row says none", and `none` means UNBOUNDED writes meanwhile, so usage past N
		// is reachable without anything misbehaving.
		//
		// SKIPPING IS THE SAFE DIRECTION AND IMPOSING IS THE IRREVERSIBLE ONE: the skipped
		// volume is temporarily UNCAPPED, so its bytes leave the resource admission first term (declared
		// caps) and are picked up by the SECOND one — the backend measures them through
		// UncappedBytes — which keeps the node's inequality a true statement about this disk
		// and errs conservatively. Imposing trades that for a unit no API call can unstick.
		//
		// Neither is it a dead end. Usage falling back under the recorded cap (the guest
		// deletes bytes) makes the NEXT pass repair it with no operator action at all, and
		// the control-plane API PATCH /v1/volumes/{id} with a size STRICTLY above current usage is a legal
		// raise that re-asserts a cap immediately (it re-clears admission, so it can be
		// refused with 507, but it is never refused as a wedge).
		if v.CapByte <= used {
			m.deps.Logf("repair volume caps: %s: NOT re-asserting the recorded cap %d — the unit holds %d byte(s) and a cap at or below usage wedges it irreversibly (the Volume subsystem / volume cap enforcement). It stays UNCAPPED, counted by the resource admission uncapped term; exits are the guest freeing bytes or PATCH /v1/volumes/%s above %d",
				v.ID, v.CapByte, used, v.ID, used)
			continue
		}
		// Disagreement, and the record's cap is still applicable. Re-assert it.
		if err := be.SetCapacity(ctx, string(v.ID), v.CapByte); err != nil {
			m.deps.Logf("repair volume caps: %s: could not re-assert the recorded cap %d (storage reports %d): %v", v.ID, v.CapByte, limit, err)
			continue
		}
		repaired++
		m.deps.Logf("repair volume caps: %s carried %d on storage but %d in the record — re-asserted the record's cap (this is the crash window between a snapshot and its cap write, volume cap enforcement)", v.ID, limit, v.CapByte)
	}
	if repaired > 0 {
		m.deps.Logf("repair volume caps: re-asserted %d cap(s)", repaired)
	}
	return nil
}

// ReclaimQuotaRows runs the Volume subsystem / Volume storage leaked-quota-row reclamation round.
//
// A leaked row is a CORRECTNESS problem, not untidiness: it does not reserve its own
// unit id, so a remount can hand that id — along with its CAP — to a brand-new volume,
// which then hits ENOSPC on a limit nobody set for it (measured: a never-capped unit hit
// ENOSPC at 3.3 MB) or is silently over-provisioned, with the engine unable to tell from
// the inside.
//
// The `parent` FK demotes this from the MAIN path to a BACKSTOP: leaking requires
// deleting a source while a live sharer remains, and RESTRICT makes that unreachable
// through the API (→ 409). What is left are paths that do not pass through user intent —
// a crash between deleting a sharer and deleting the source, and the sweep's own
// per-unit ordering. That is exactly why its error handling is "leave it for the next
// round" rather than "block".
func (m *Mediator) ReclaimQuotaRows(ctx context.Context) error {
	backends, err := m.configuredVolumeBackends()
	if err != nil {
		return fmt.Errorf("reclaim quota rows: %w", err)
	}
	if len(backends) == 0 {
		if err := m.validateDurableVolumeProviders(ctx); err != nil {
			return fmt.Errorf("reclaim quota rows: %w", err)
		}
		return nil
	}
	if err := m.validateDurableVolumeProviders(ctx); err != nil {
		return fmt.Errorf("reclaim quota rows: %w", err)
	}
	for _, selected := range backends {
		ids := selected.backend.DeletedRowIDs(ctx)
		if len(ids) == 0 {
			continue
		}
		n, reclaimErr := selected.backend.ReclaimQuotaRows(ctx, ids)
		if n > 0 {
			m.deps.Logf("reclaim quota rows: provider %s reclaimed %d leaked row(s)", selected.name, n)
		}
		if reclaimErr != nil {
			// Informational, not fatal: every row named in it stays recorded and is retried.
			// A `<squota space holder>` can legitimately resist reclamation for a live clone's
			// entire lifetime, so this is normal operation rather than an error path.
			m.deps.Logf("reclaim quota rows: provider %s: %v", selected.name, reclaimErr)
		}
	}
	return nil
}
