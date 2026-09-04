package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/TSHOGX/khora/internal/model"
)

// Memory is the in-memory Store: the second implementation of the extension boundary Behavior
// seam, serving fast, disk-free L1 unit tests.
//
// The single mutex is how Memory DELIVERS the transactional outbox atomic-commit contract:
// "change the map + append the outbox slice" happen under one Lock, so a
// transition and its event commit together or not at all — where SQLite uses
// BEGIN/COMMIT. Every "X equivalent" comment below is one clause of that.
type Memory struct {
	mu            sync.Mutex
	rec           map[model.SandboxID]model.Sandbox
	placements    map[model.SandboxID]model.Placement
	processes     map[model.ProcessID]model.Process
	operations    map[model.OperationID]model.Operation
	operationKeys map[string]model.OperationID
	telemetry     map[model.SandboxID][]model.TelemetrySample
	telemetrySeq  map[model.SandboxID]uint64
	// tmpl holds Template rows (health and reclamation first-class object). The health and reclamation
	// reference-protection contract arrives here as a Go scan under the one mutex,
	// where SQLite gets it from FK RESTRICT.
	tmpl map[model.TemplateID]model.Template
	// vol holds Volume rows (the Volume subsystem first-class object, health and reclamation).
	vol map[model.VolumeID]model.Volume
	// mounts is the mount table (the Volume subsystem). A SLICE rather than a map because no single
	// column is its key — a row is identified by (sandbox, path) for topology and by
	// (volume, access) for the lease, and both are scanned rather than looked up.
	//
	// Reference protection on this backend is three explicit Go scans — sandbox side,
	// volume side, and the lease — where SQLite gets them from constraints.
	mounts []model.Mount
	// outbox is ordered and the publisher reads oldest-first. Published rows remain
	// replayable until TrimOutbox removes them. With event retention disabled it grows
	// for the lifetime of this non-durable store, matching SQLite's configured behavior.
	outbox []outboxRow
	// nextSeq is the monotonic sequence counter (Store's sequence contract). It
	// is assigned under m.mu, in the SAME critical section that makes the row
	// visible — which is precisely how Memory delivers the no-visibility-gap
	// promise: there is no window in which a number is taken but the row is not
	// yet readable, so a lower sequence can never appear after a higher one has
	// been served. It counts appends, NOT len(outbox), so it stays monotonic even
	// if rows are ever removed.
	nextSeq int64
}

// outboxRow is one persisted Event plus its published flag (the transactional outbox).
type outboxRow struct {
	ev        Event
	published bool
	// publishedAt is WHEN delivery was marked. It exists because the retention window ages
	// rows by DELIVERY rather than occurrence (Store.TrimOutbox) — a bool alone cannot
	// answer "delivered before this cutoff".
	publishedAt time.Time
}

func NewMemory() *Memory {
	return &Memory{
		rec:           make(map[model.SandboxID]model.Sandbox),
		tmpl:          make(map[model.TemplateID]model.Template),
		vol:           make(map[model.VolumeID]model.Volume),
		placements:    make(map[model.SandboxID]model.Placement),
		processes:     make(map[model.ProcessID]model.Process),
		operations:    make(map[model.OperationID]model.Operation),
		operationKeys: make(map[string]model.OperationID),
		telemetry:     make(map[model.SandboxID][]model.TelemetrySample),
		telemetrySeq:  make(map[model.SandboxID]uint64),
	}
}

// checkEventKind refuses a Kind outside the lifecycle state machine's closed set, which is what the SQLite
// arm's CHECK constraint does. Both backends must agree, or a typo'd kind is rejected in
// production and accepted in the tests that are supposed to prove the behaviour.
func checkEventKind(ev *Event) error {
	if ev == nil {
		return nil
	}
	if !ev.Kind.Valid() {
		// A plain error, matching the SQLite arm: a CHECK violation is not one of the
		// contract's named sentinels, and inventing one here would make the two backends
		// differ on the error VALUE while agreeing on the refusal.
		return fmt.Errorf("store: event kind %q is not one of the lifecycle state machine's six (%v)",
			ev.Kind, model.EventKinds)
	}
	// Cause carries the same obligation (its column has a CHECK too). Empty is legitimate
	// — most kinds have one route and carry no cause — so only a non-empty value is checked.
	if ev.Cause != model.CauseNotApplicable && !ev.Cause.Valid() {
		return fmt.Errorf("store: event cause %q is not one of the known causes (%v)",
			ev.Cause, model.EventCauses)
	}
	return nil
}

// appendEvent records ev in the outbox. MUST be called with m.mu held so it
// commits atomically with the state change beside it (the transactional outbox).
func (m *Memory) appendEvent(ev *Event) {
	if ev == nil {
		return
	}
	row := outboxRow{ev: *ev}
	m.nextSeq++
	row.ev.Seq = m.nextSeq // the store assigns it; any caller-set Seq is overwritten
	m.outbox = append(m.outbox, row)
}

func (m *Memory) ensureEventSequence(ev *Event) error {
	if ev != nil && m.nextSeq == 1<<63-1 {
		return ErrEventSequenceExhausted
	}
	return nil
}

func (m *Memory) CreateSandbox(_ context.Context, s model.Sandbox, ev *Event) error {
	if err := checkEventKind(ev); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rec[s.ID]; ok {
		return ErrConflict
	}
	// FK RESTRICT equivalent (health and reclamation reference-side): the referenced Template must exist.
	// Under the one mutex a concurrent DeleteTemplate cannot interleave — the same
	// single-writer serialization SQLite gets (health and reclamation).
	if _, ok := m.tmpl[s.Template]; !ok {
		return ErrTemplateNotFound
	}
	if err := m.ensureEventSequence(ev); err != nil {
		return err
	}
	// Normalize the network declaration on the way in, so this backend holds what SQLite
	// holds (it gets the same from marshalNetwork): the control-plane API requires the three arrays to read
	// back as `[]` rather than null. Without it the two backends would disagree on the
	// READ SHAPE of every declaration, and storetest would be pinning one's accident.
	s.Network = s.Network.Normalize()
	m.rec[s.ID] = s
	m.appendEvent(ev) // same commit as the insert (the transactional outbox)
	return nil
}

func (m *Memory) CreateTemplate(_ context.Context, t model.Template) error {
	if err := t.Validate(); err != nil {
		return err
	}
	t.Manifest = t.Manifest.Normalize()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tmpl[t.ID]; ok {
		return ErrConflict
	}
	// non-empty parent must exist (lineage FK, health and reclamation: references point upward).
	if t.Parent != "" {
		if _, ok := m.tmpl[t.Parent]; !ok {
			return ErrTemplateNotFound
		}
	}
	m.tmpl[t.ID] = t
	return nil
}

func (m *Memory) GetTemplate(_ context.Context, id model.TemplateID) (model.Template, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tmpl[id]
	if !ok {
		return model.Template{}, ErrTemplateNotFound
	}
	t.Manifest = t.Manifest.Normalize()
	if err := t.Validate(); err != nil {
		return model.Template{}, err
	}
	return t, nil
}

func (m *Memory) ListTemplates(_ context.Context) ([]model.Template, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Template, 0, len(m.tmpl))
	for _, t := range m.tmpl {
		t.Manifest = t.Manifest.Normalize()
		if err := t.Validate(); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (m *Memory) DeleteTemplate(_ context.Context, id model.TemplateID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tmpl[id]; !ok {
		return ErrTemplateNotFound
	}
	// RESTRICT equivalent: scan for any live referencing row — a sandbox pinning it, or a
	// child Template naming it as parent. Either → ErrInUse (health and reclamation
	// referenced-means-undeletable). The Go-check twin of SQLite's FK RESTRICT.
	for _, s := range m.rec {
		if s.Template == id {
			return ErrInUse
		}
	}
	for _, t := range m.tmpl {
		if t.Parent == id {
			return ErrInUse
		}
	}
	delete(m.tmpl, id)
	return nil
}

// --- Volume CRUD (the Volume subsystem) ------------------------------------------------------

func (m *Memory) CreateVolume(_ context.Context, v model.Volume) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vol[v.ID]; ok {
		return ErrConflict
	}
	// Lineage FK equivalent (health and reclamation, references point upward): a named parent must exist.
	if v.Parent != "" {
		if _, ok := m.vol[v.Parent]; !ok {
			return ErrVolumeNotFound
		}
	}
	m.vol[v.ID] = v
	return nil
}

func (m *Memory) GetVolume(_ context.Context, id model.VolumeID) (model.Volume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vol[id]
	if !ok {
		return model.Volume{}, ErrVolumeNotFound
	}
	return v, nil
}

func (m *Memory) ListVolumes(_ context.Context) ([]model.Volume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Volume, 0, len(m.vol))
	for _, v := range m.vol {
		out = append(out, v)
	}
	return out, nil
}

func (m *Memory) SetVolumeCapacity(_ context.Context, id model.VolumeID, capByte int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vol[id]
	if !ok {
		return ErrVolumeNotFound
	}
	v.CapByte = capByte
	m.vol[id] = v
	return nil
}

func (m *Memory) DeleteVolume(_ context.Context, id model.VolumeID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vol[id]; !ok {
		return ErrVolumeNotFound // reported, never folded into success (the API error contract)
	}
	// RESTRICT equivalent, VOLUME side: the Go-check twin of SQLite's mounts.volume FK.
	for _, mt := range m.mounts {
		if mt.Volume == id {
			return ErrVolumeInUse
		}
	}
	// LINEAGE side, a DIFFERENT verdict: a live snapshot/clone names it as parent. The
	// caller's remedy differs (delete the descendant vs detach a mount), so the two are
	// never collapsed (the control-plane API).
	for _, other := range m.vol {
		if other.Parent == id {
			return ErrVolumeHasDescendants
		}
	}
	delete(m.vol, id)
	return nil
}

// --- Mount table (the Volume subsystem) ------------------------------------------------------
//
// The atomic lease arrives under the ONE mutex Memory already holds — no new lock class
// (the Volume subsystem / the mount table).

func (m *Memory) CreateMounts(_ context.Context, mounts []model.Mount) error {
	if len(mounts) == 0 {
		return nil
	}
	if err := validateBinding(mounts); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	accepted, err := m.admitBinding(m.mounts, mounts)
	if err != nil {
		return err
	}
	// ALL-OR-NOTHING: nothing was appended to m.mounts until every row cleared, so a
	// binding that lost anywhere above added no rows at all.
	m.mounts = append(m.mounts, accepted...)
	return nil
}

// ReplaceMounts swaps the whole binding under the ONE mutex Memory already holds: filter
// and append in a single critical section, so no reader can be scheduled between the two
// halves and observe the sandbox with no rows. Why that is a contract promise rather than
// this backend's detail: Store.ReplaceMounts.
func (m *Memory) ReplaceMounts(_ context.Context, sandbox model.SandboxID, next []model.Mount) error {
	if err := validateReplacement(sandbox, next); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// The predicate is evaluated against the rows that SURVIVE the replacement — i.e. with
	// this sandbox's own rows already dropped. Judged against the un-filtered set, a
	// re-submitted IDENTICAL binding would contend with the very rows it is replacing.
	kept := make([]model.Mount, 0, len(m.mounts))
	for _, mt := range m.mounts {
		if mt.Sandbox != sandbox {
			kept = append(kept, mt)
		}
	}
	accepted, err := m.admitBinding(kept, next)
	if err != nil {
		// m.mounts is untouched, so a refused replacement leaves the OLD binding intact
		// (consequence ) — the same all-or-nothing SQLite gets from rolling back.
		return err
	}
	m.mounts = append(kept, accepted...)
	return nil
}

// admitBinding evaluates the endpoint, lease and topology rules for `mounts` against
// `base` (the rows that will coexist with them) and returns the rows to append. It
// mutates nothing, so a caller that gets an error has changed no state.
//
// Callers MUST hold m.mu. Shared by CreateMounts and ReplaceMounts so the lease predicate
// has ONE spelling on this backend, mirroring SQLite's shared insertBinding.
func (m *Memory) admitBinding(base, mounts []model.Mount) ([]model.Mount, error) {
	// Reference side of health and reclamation atomicity: both endpoints must exist. Under the one mutex a
	// concurrent delete cannot interleave.
	for _, mt := range mounts {
		if _, ok := m.rec[mt.Sandbox]; !ok {
			return nil, ErrNotFound
		}
		if _, ok := m.vol[mt.Volume]; !ok {
			return nil, ErrVolumeNotFound
		}
	}

	// The lease predicate, evaluated against BOTH the coexisting rows and the rows this
	// same binding is about to add — the second half matters because a binding could
	// otherwise contend with itself and slip past.
	pending := make([]model.Mount, 0, len(mounts))
	for _, mt := range mounts {
		for _, existing := range append(append([]model.Mount(nil), base...), pending...) {
			if existing.Volume != mt.Volume {
				continue
			}
			// The FULL predicate, not just "two exclusive rows": inserting exclusive when
			// ANY row exists, or inserting anything when an EXCLUSIVE row exists.
			if mt.Access == model.Exclusive || existing.Access == model.Exclusive {
				return nil, ErrMountLeaseConflict
			}
		}
		// Topology assertions, against coexisting rows as well as this binding.
		for _, existing := range append(append([]model.Mount(nil), base...), pending...) {
			if existing.Sandbox != mt.Sandbox {
				continue
			}
			if existing.Path == mt.Path || existing.Volume == mt.Volume {
				return nil, ErrMountBindingInvalid
			}
		}
		pending = append(pending, mt)
	}
	return pending, nil
}

func (m *Memory) ListMounts(_ context.Context, sandbox model.SandboxID) ([]model.Mount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Mount, 0)
	for _, mt := range m.mounts {
		if mt.Sandbox == sandbox {
			out = append(out, mt)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (m *Memory) ListMountsByVolume(_ context.Context, volume model.VolumeID) ([]model.Mount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Mount, 0)
	for _, mt := range m.mounts {
		if mt.Volume == volume {
			out = append(out, mt)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sandbox != out[j].Sandbox {
			return out[i].Sandbox < out[j].Sandbox
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

func (m *Memory) DeleteMounts(_ context.Context, sandbox model.SandboxID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.mounts[:0]
	for _, mt := range m.mounts {
		if mt.Sandbox != sandbox {
			kept = append(kept, mt)
		}
	}
	m.mounts = kept
	return nil // idempotent: no rows is success
}

func (m *Memory) GetSandbox(_ context.Context, id model.SandboxID) (model.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rec[id]
	if !ok {
		return model.Sandbox{}, ErrNotFound
	}
	// DEEP-COPY the declaration's slices. SQLite re-decodes JSON on every read and so
	// cannot alias; handing out the live backing array here would let a caller's in-place
	// mutation corrupt the store on ONE backend only (model.NetworkDecl.Clone).
	s.Network = s.Network.Clone()
	return s, nil
}

func (m *Memory) ListSandboxes(_ context.Context) ([]model.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Sandbox, 0, len(m.rec))
	for _, s := range m.rec {
		s.Network = s.Network.Clone() // see GetSandbox: the read shape must match SQLite's
		out = append(out, s)
	}
	return out, nil
}

func (m *Memory) CompareAndSetState(_ context.Context, id model.SandboxID, expected, next model.Lifecycle, ev *Event, cause *model.DeathCause) error {
	// BEFORE the lock and before any mutation: a refused event must leave the state
	// untouched, because the transition and its event are one commit (the transactional outbox).
	if err := checkEventKind(ev); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rec[id]
	if !ok {
		return ErrNotFound
	}
	if s.State != expected {
		return ErrConflict
	}
	if err := m.ensureEventSequence(ev); err != nil {
		return err
	}
	s.State = next
	// death cause under the SAME lock as the state change (health and reclamation / the transactional outbox, and
	// Store.CompareAndSetState for why no later window exists). nil = leave untouched.
	if cause != nil {
		s.DeathCause = *cause
	}
	s.UpdatedAt = time.Now()
	m.rec[id] = s
	m.appendEvent(ev) // state change + event append, one commit (the transactional outbox)
	return nil
}

func (m *Memory) UpdateBinding(_ context.Context, id model.SandboxID, expected model.Lifecycle, b Binding, cause *model.DeathCause) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rec[id]
	if !ok {
		return ErrNotFound
	}
	// Conditional on the state the caller decided against (per-sandbox transition serialization), so a resume or
	// evict that landed in between makes this fail rather than write against a
	// decision that no longer holds.
	if s.State != expected {
		return ErrConflict
	}
	s.Limits, s.IdleTimeout, s.Retention, s.Network = b.Apply(s)
	// Same lock as the field writes (the transactional outbox): a raise that lifts the health and reclamation refusal
	// must not be observable without the cap that justifies it.
	if cause != nil {
		s.DeathCause = *cause
	}
	// UpdatedAt deliberately NOT touched (see Store.UpdateBinding).
	m.rec[id] = s
	return nil
}

// SetUpdatedAtForTest backdates a record's UpdatedAt. TEST-ONLY seam: reaper
// retention math measures "how long paused" from UpdatedAt (the last state
// change), and tests need to simulate a sandbox paused long ago without
// sleeping. NOT part of the Store interface — it carries no production
// semantics. No-op for an unknown ID.
func (m *Memory) SetUpdatedAtForTest(id model.SandboxID, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.rec[id]; ok {
		s.UpdatedAt = t
		m.rec[id] = s
	}
}

// SetLimitsForTest overwrites a record's resource-limit Binding, bypassing the
// state precondition and admission gate UpdateBinding enforces. TEST-ONLY seam,
// like SetUpdatedAtForTest: it lets a test prove the caps are a BINDING
// (re-materialized from the record at every placement, resource admission) rather than frozen
// placement state — edit the record while paused, resume, and the new values must
// take effect. No-op for an unknown ID.
func (m *Memory) SetLimitsForTest(id model.SandboxID, l model.Limits) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.rec[id]; ok {
		s.Limits = l
		m.rec[id] = s
	}
}

func (m *Memory) DeleteSandbox(_ context.Context, id model.SandboxID, ev *Event) error {
	if err := checkEventKind(ev); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rec[id]; !ok {
		return ErrNotFound
	}
	// RESTRICT equivalent, SANDBOX side. It is what makes the deletion ORDER required
	// rather than stylistic on this backend too: the caller must DeleteMounts first,
	// exactly as against SQLite, so one discipline covers both.
	for _, mt := range m.mounts {
		if mt.Sandbox == id {
			return ErrSandboxHasMounts
		}
	}
	if err := m.ensureEventSequence(ev); err != nil {
		return err
	}
	delete(m.placements, id)
	for processID, p := range m.processes {
		if p.Sandbox == id {
			delete(m.processes, processID)
		}
	}
	delete(m.rec, id)
	m.appendEvent(ev) // the outbox row outlives the deleted record (the transactional outbox)
	return nil
}

func (m *Memory) UnpublishedEvents(_ context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		// Same refusal as the SQLite arm, and the contract suite pins both: a
		// zero-means-everything reading is how the unbounded scan comes back.
		return nil, fmt.Errorf("store: UnpublishedEvents limit must be positive, got %d", limit)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, 0, limit)
	// the slice is append-ordered, so this is already Seq order.
	for _, r := range m.outbox {
		if !r.published {
			out = append(out, r.ev)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// TrimOutbox implements Store.TrimOutbox (the transactional outbox's retention mechanism). Read that
// contract for why each clause matters.
func (m *Memory) TrimOutbox(_ context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("store: TrimOutbox limit must be positive, got %d", limit)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.outbox) == 0 {
		return 0, nil
	}
	// The newest row is never eligible, so the window's bounds cannot collapse to 0/0
	// and be mistaken for "nothing ever happened" (see the contract).
	newest := m.outbox[len(m.outbox)-1].ev.Seq
	cut := cutoff.UnixNano()
	kept := make([]outboxRow, 0, len(m.outbox))
	deleted := 0
	for _, r := range m.outbox {
		eligible := deleted < limit &&
			r.published &&
			!r.publishedAt.IsZero() && r.publishedAt.UnixNano() < cut &&
			r.ev.Seq < newest
		if eligible {
			deleted++
			continue
		}
		kept = append(kept, r)
	}
	m.outbox = kept
	return deleted, nil
}

// CountUnpublished implements Store.CountUnpublished (the transactional outbox: the gauge's source is
// independent of the drain read). This arm walks, which is honest at its scale — the
// in-memory outbox is bounded by the process's own lifetime.
func (m *Memory) CountUnpublished(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, r := range m.outbox {
		if !r.published {
			n++
		}
	}
	return n, nil
}

// Events implements the control-plane API replay read (Store.Events). Delivery state is not a
// predicate — replay is over the whole retained window.
func (m *Memory) Events(_ context.Context, after int64, limit int) ([]Event, int64, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.outbox) == 0 {
		return []Event{}, 0, 0, nil
	}
	earliest := m.outbox[0].ev.Seq
	latest := m.outbox[len(m.outbox)-1].ev.Seq
	out := make([]Event, 0)
	for _, r := range m.outbox {
		if r.ev.Seq <= after {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, r.ev)
	}
	return out, earliest, latest, nil
}

func (m *Memory) MarkPublished(_ context.Context, eventID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.outbox {
		if m.outbox[i].ev.ID == eventID {
			m.outbox[i].published = true // idempotent: re-marking is harmless
			// First mark wins: re-marking must not push the delivery time forward, or a
			// row's retention age resets on every idempotent re-mark and the window never
			// advances.
			if m.outbox[i].publishedAt.IsZero() {
				m.outbox[i].publishedAt = time.Now()
			}
			return nil
		}
	}
	return nil // unknown ID → no-op (idempotent)
}
