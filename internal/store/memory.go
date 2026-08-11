package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"warren/internal/model"
)

// Memory is the in-memory Store: the second implementation of the §1.3 Behavior
// seam, serving fast, disk-free L1 unit tests. The SQLite backend (ADR-0004)
// coexists behind the SAME interface; which one the orchestrator assembles is
// register-an-implementation (§1.3).
//
// The single mutex is how Memory DELIVERS the §4.4-C atomic-commit contract:
// "change the map + append the outbox slice" happen under one Lock, so a
// transition and its event commit together or not at all — the SAME contract
// SQLite delivers with BEGIN/COMMIT, and the caller cannot tell which it got.
type Memory struct {
	mu  sync.Mutex
	rec map[model.SandboxID]model.Sandbox
	// tmpl holds Template rows (§4.3 first-class object). Memory delivers the
	// SAME §4.3 reference-protection contract SQLite gives via FK RESTRICT, but
	// with a Go scan under the one mutex (§1.3 seam: one set of assertions, two
	// backends). Existence-is-state: presence in this map = the Template's ready
	// state (§4.3), no separate state field.
	tmpl map[model.TemplateID]model.Template
	// vol holds Volume rows (§5.C first-class object, §4.3). Existence-is-state:
	// presence in this map IS the volume's ready state, with no separate field.
	vol map[model.VolumeID]model.Volume
	// mounts is the mount table (§5.C). A SLICE rather than a map because no single
	// column is its key — a row is identified by (sandbox, path) for topology and by
	// (volume, access) for the lease, and both are scanned rather than looked up.
	//
	// Reference protection on this backend is three explicit Go scans — sandbox side,
	// volume side, and the lease — where SQLite gets them from constraints. Same
	// contract, different mechanism (§1.3 seam).
	mounts []model.Mount
	// outbox is append-only, ordered — the publisher reads oldest-first. Like the
	// SQLite backend, published rows are marked, never removed (same unbounded-
	// growth known limitation; open-decisions §2, "The size of the replayable
	// event-stream window (§4.4-C)").
	// Bounded in practice only by process lifetime since Memory is non-durable.
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

// outboxRow is one persisted Event plus its published flag (§4.4-C outbox).
type outboxRow struct {
	ev        Event
	published bool
}

func NewMemory() *Memory {
	return &Memory{
		rec:  make(map[model.SandboxID]model.Sandbox),
		tmpl: make(map[model.TemplateID]model.Template),
		vol:  make(map[model.VolumeID]model.Volume),
	}
}

// checkEventKind refuses a Kind outside §4.1's closed set, which is what the SQLite
// arm's CHECK constraint does. Both backends must agree: a closed set enforced on one
// of them only means a typo'd kind is rejected in production and accepted in the tests
// that are supposed to prove the behaviour (§1.3 — one set of assertions, two
// backends).
func checkEventKind(ev *Event) error {
	if ev == nil || ev.Kind.Valid() {
		return nil
	}
	// A plain error, matching the SQLite arm: a CHECK violation is not one of the
	// contract's named sentinels, and inventing one here would make the two backends
	// differ on the error VALUE while agreeing on the refusal.
	return fmt.Errorf("store: event kind %q is not one of §4.1's six (%v)",
		ev.Kind, model.EventKinds)
}

// appendEvent records ev in the outbox. MUST be called with m.mu held so it
// commits atomically with the state change beside it (§4.4-C).
func (m *Memory) appendEvent(ev *Event) {
	if ev == nil {
		return
	}
	row := outboxRow{ev: *ev}
	m.nextSeq++
	row.ev.Seq = m.nextSeq // the store assigns it; any caller-set Seq is overwritten
	m.outbox = append(m.outbox, row)
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
	// FK RESTRICT equivalent (§4.3 reference-side): the referenced Template must
	// exist. Under the one mutex, a concurrent DeleteTemplate cannot interleave —
	// the same single-writer serialization SQLite gets, so the atomicity is
	// identical (§4.3 single-writer serialization).
	if _, ok := m.tmpl[s.Template]; !ok {
		return ErrTemplateNotFound
	}
	// Normalize the §5.D declaration on the way in, so this backend holds what SQLite
	// holds: §6.1 requires the three arrays to always read back as `[]` rather than
	// null, and SQLite gets that from marshalNetwork. Without it the two backends
	// would disagree on the READ SHAPE of every declaration, and storetest's contract
	// assertions would be pinning one backend's accident.
	s.Network = s.Network.Normalize()
	m.rec[s.ID] = s
	m.appendEvent(ev) // same commit as the insert (§4.4-C)
	return nil
}

func (m *Memory) CreateTemplate(_ context.Context, t model.Template) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tmpl[t.ID]; ok {
		return ErrConflict
	}
	// non-empty parent must exist (lineage FK, §4.3: references point upward).
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
	return t, nil
}

func (m *Memory) ListTemplates(_ context.Context) ([]model.Template, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Template, 0, len(m.tmpl))
	for _, t := range m.tmpl {
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
	// RESTRICT equivalent: scan for any live referencing row — a sandbox pinning
	// it, or a child Template naming it as parent. Either → ErrInUse (§4.3
	// referenced-means-undeletable). This is the Go-check twin of SQLite's FK
	// RESTRICT; the two run the SAME contract assertions (§1.3 seam).
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

// --- Volume CRUD (§5.C) ------------------------------------------------------
//
// Memory delivers the SAME contract SQLite does, under the one mutex that already
// serializes everything else here (§1.3 seam: one set of assertions, two backends).

func (m *Memory) CreateVolume(_ context.Context, v model.Volume) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vol[v.ID]; ok {
		return ErrConflict
	}
	// Lineage FK equivalent (§4.3, references point upward): a named parent must exist.
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
		return ErrVolumeNotFound // reported, never folded into success (§6.2)
	}
	// RESTRICT equivalent, VOLUME side: the Go-check twin of SQLite's mounts.volume
	// FK. Both backends run the same contract assertions (§1.3 seam).
	for _, mt := range m.mounts {
		if mt.Volume == id {
			return ErrVolumeInUse
		}
	}
	// LINEAGE side, a DIFFERENT verdict: a live snapshot/clone names it as parent. The
	// caller's remedy differs (delete the descendant vs detach a mount), so the two are
	// never collapsed (§6.1).
	for _, other := range m.vol {
		if other.Parent == id {
			return ErrVolumeHasDescendants
		}
	}
	delete(m.vol, id)
	return nil
}

// --- Mount table (§5.C) ------------------------------------------------------
//
// Memory delivers the atomic lease under the ONE mutex it already holds — no new lock
// class, which is the same "not a new mechanism" argument SQLite's single statement
// makes (§5.C / volume.md D3).

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

// ReplaceMounts swaps the whole binding under the ONE mutex Memory already holds. See
// Store.ReplaceMounts for why the atomicity is a contract promise; here it is delivered by
// filtering and appending in a single critical section, so no reader can be scheduled
// between the two halves and observe the sandbox with no rows.
func (m *Memory) ReplaceMounts(_ context.Context, sandbox model.SandboxID, next []model.Mount) error {
	if err := validateReplacement(sandbox, next); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// The predicate is evaluated against the rows that SURVIVE the replacement — i.e. with
	// this sandbox's own rows already dropped. That is the whole reason a rebind cannot be
	// a delete plus a CreateMounts: judged against the un-filtered set, a re-submitted
	// IDENTICAL binding contends with the very rows it is replacing.
	kept := make([]model.Mount, 0, len(m.mounts))
	for _, mt := range m.mounts {
		if mt.Sandbox != sandbox {
			kept = append(kept, mt)
		}
	}
	accepted, err := m.admitBinding(kept, next)
	if err != nil {
		// m.mounts is untouched, so a refused replacement leaves the OLD binding intact
		// (consequence ①) — the same all-or-nothing SQLite gets from rolling back.
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
	// Reference side of §4.3 atomicity: both endpoints must exist. Under the one mutex
	// a concurrent delete cannot interleave, so this is the same atomicity SQLite's FK
	// gives inside its transaction.
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
	// cannot alias; handing out the live backing array here would let a caller's
	// in-place mutation corrupt the store on ONE backend only — the read-shape
	// divergence CreateSandbox's normalization exists to prevent.
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
	// untouched, because the transition and its event are one commit (§4.4-C).
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
	s.State = next
	// death cause under the SAME lock as the state change — the Go-mutex twin of
	// SQLite's single transaction, delivering the atomicity
	// Store.CompareAndSetState promises (§4.3 / §4.4-C). nil = leave untouched.
	if cause != nil {
		s.DeathCause = *cause
	}
	s.UpdatedAt = time.Now()
	m.rec[id] = s
	m.appendEvent(ev) // state change + event append, one commit (§4.4-C)
	return nil
}

func (m *Memory) UpdateBinding(_ context.Context, id model.SandboxID, expected model.Lifecycle, b Binding, cause *model.DeathCause) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rec[id]
	if !ok {
		return ErrNotFound
	}
	// Conditional on the state the caller decided against (§4.4-B), so a resume or
	// evict that landed in between makes this fail rather than write against a
	// decision that no longer holds.
	if s.State != expected {
		return ErrConflict
	}
	s.Limits, s.IdleTimeout, s.Retention, s.Network = b.Apply(s)
	// Same lock as the field writes (§4.4-C): a raise that lifts the §4.3 refusal
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
// (re-materialized from the record at every placement, §3.4) rather than frozen
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
	delete(m.rec, id)
	m.appendEvent(ev) // the outbox row outlives the deleted record (§4.4-C)
	return nil
}

func (m *Memory) UnpublishedEvents(_ context.Context) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, 0)
	// the slice is append-ordered, so this is already Seq order.
	for _, r := range m.outbox {
		if !r.published {
			out = append(out, r.ev)
		}
	}
	return out, nil
}

// Events implements the §6.1 replay read (Store.Events). Delivery state is not a
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
			return nil
		}
	}
	return nil // unknown ID → no-op (idempotent)
}
