package store

import (
	"context"
	"sync"
	"time"

	"engine/internal/model"
)

// Memory is the M1 in-memory Store. It is scaffolding, not a design decision:
// M2 replaces it with an embedded persistent backend behind the same interface
// (§4.4-C). The mutex here stands in for what M2 makes a real transactional
// commit — the CAS shape is already correct so the transition lock lands right.
type Memory struct {
	mu  sync.Mutex
	rec map[model.SandboxID]model.Sandbox
}

func NewMemory() *Memory {
	return &Memory{rec: make(map[model.SandboxID]model.Sandbox)}
}

func (m *Memory) CreateSandbox(_ context.Context, s model.Sandbox) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rec[s.ID]; ok {
		return ErrConflict
	}
	m.rec[s.ID] = s
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

func (m *Memory) CompareAndSetState(_ context.Context, id model.SandboxID, expected, next model.Lifecycle) error {
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
	s.UpdatedAt = time.Now()
	m.rec[id] = s
	return nil
}

func (m *Memory) DeleteSandbox(_ context.Context, id model.SandboxID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rec[id]; !ok {
		return ErrNotFound
	}
	delete(m.rec, id)
	return nil
}
