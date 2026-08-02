package store

import (
	"context"
	"sync"
	"time"

	"engine/internal/model"
)

// Memory is the in-memory Store. It is NOT scaffolding to be deleted: it is the
// second implementation of the §1.3 Behavior seam, kept for fast, disk-free L1
// unit tests. The SQLite backend (ADR-0004) coexists behind the SAME interface;
// which one the orchestrator assembles is register-an-implementation (§1.3).
//
// The single mutex is how Memory DELIVERS the §4.4-C atomic-commit contract:
// "change the map + append the outbox slice" happen under one Lock, so a
// transition and its event commit together or not at all — the SAME contract
// SQLite delivers with BEGIN/COMMIT, expressed with no branch in the caller
// (§4.4-C 铁律).
type Memory struct {
	mu  sync.Mutex
	rec map[model.SandboxID]model.Sandbox
	// tmpl holds Template rows (§4.3 first-class object). Memory delivers the
	// SAME §4.3 reference-protection contract SQLite gives via FK RESTRICT, but
	// with a Go scan under the one mutex (§1.3 seam: one set of assertions, two
	// backends). Existence-is-state: presence in this map = the Template's ready
	// state (§4.3), no separate state field.
	tmpl map[model.TemplateID]model.Template
	// outbox is append-only, ordered — the publisher reads oldest-first. Like the
	// SQLite backend, published rows are marked, never removed (same unbounded-
	// growth known limitation; open-decisions §2 §4.1/§4.4-C row). Bounded in
	// practice only by process lifetime since Memory is non-durable.
	outbox []outboxRow
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
	}
}

// appendEvent records ev in the outbox. MUST be called with m.mu held so it
// commits atomically with the state change beside it (§4.4-C).
func (m *Memory) appendEvent(ev *Event) {
	if ev == nil {
		return
	}
	m.outbox = append(m.outbox, outboxRow{ev: *ev})
}

func (m *Memory) CreateSandbox(_ context.Context, s model.Sandbox, ev *Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rec[s.ID]; ok {
		return ErrConflict
	}
	// FK RESTRICT equivalent (§4.3 reference-side): the referenced Template must
	// exist. Under the one mutex, a concurrent DeleteTemplate cannot interleave —
	// the same single-writer serialization SQLite gets, so the atomicity is
	// identical (§4.3 单写者串行 / §4.4-C 铁律: no backend-difference in correctness).
	if _, ok := m.tmpl[s.Template]; !ok {
		return ErrTemplateNotFound
	}
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
	// non-empty parent must exist (lineage FK, §4.3 INV-2 引用向上).
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
	// it, or a child Template naming it as parent. Either → ErrInUse (§4.3 INV-1
	// 被引用即拒删). This is the Go-check twin of SQLite's FK RESTRICT; the two run
	// the SAME contract assertions (§1.3 seam).
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

func (m *Memory) GetSandbox(_ context.Context, id model.SandboxID) (model.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rec[id]
	if !ok {
		return model.Sandbox{}, ErrNotFound
	}
	return s, nil
}

func (m *Memory) ListSandboxes(_ context.Context) ([]model.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Sandbox, 0, len(m.rec))
	for _, s := range m.rec {
		out = append(out, s)
	}
	return out, nil
}

func (m *Memory) CompareAndSetState(_ context.Context, id model.SandboxID, expected, next model.Lifecycle, ev *Event, cause *model.DeathCause) error {
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
	// death cause under the SAME lock as the state change (§4.3 / §4.4-C): the
	// Go-mutex twin of SQLite's single transaction — a committed crash-paused
	// demotion can never lack its cause. nil = leave untouched.
	if cause != nil {
		s.DeathCause = *cause
	}
	s.UpdatedAt = time.Now()
	m.rec[id] = s
	m.appendEvent(ev) // state change + event append, one commit (§4.4-C)
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

// SetLimitsForTest overwrites a record's resource-limit Binding. TEST-ONLY seam,
// like SetUpdatedAtForTest: it exists so a test can prove the caps are a BINDING
// (re-materialized from the record at every placement, §3.4) rather than frozen
// placement state — edit the record while paused, resume, and the new values must
// take effect. There is no production "edit limits" verb yet, so this carries no
// production semantics. No-op for an unknown ID.
func (m *Memory) SetLimitsForTest(id model.SandboxID, l model.Limits) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.rec[id]; ok {
		s.Limits = l
		m.rec[id] = s
	}
}

func (m *Memory) DeleteSandbox(_ context.Context, id model.SandboxID, ev *Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rec[id]; !ok {
		return ErrNotFound
	}
	delete(m.rec, id)
	m.appendEvent(ev) // the outbox row outlives the deleted record (§4.4-C)
	return nil
}

func (m *Memory) UnpublishedEvents(_ context.Context) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, 0)
	for _, r := range m.outbox {
		if !r.published {
			out = append(out, r.ev)
		}
	}
	return out, nil
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
