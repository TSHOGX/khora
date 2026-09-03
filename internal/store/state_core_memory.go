package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/TSHOGX/khora/internal/model"
)

// The state-core records are kept under Memory's existing mutex.  There is no
// second lock class: placement, process, and operation updates participate in
// the same atomic critical section as sandbox and outbox state.

func (m *Memory) CreatePlacement(_ context.Context, p model.Placement) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rec[p.Sandbox]; !ok {
		return ErrNotFound
	}
	if _, ok := m.placements[p.Sandbox]; ok {
		return ErrConflict
	}
	m.placements[p.Sandbox] = p
	return nil
}

func (m *Memory) GetPlacement(_ context.Context, sandbox model.SandboxID) (model.Placement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.placements[sandbox]
	if !ok {
		return model.Placement{}, ErrPlacementNotFound
	}
	return p, nil
}

func (m *Memory) ListPlacements(_ context.Context) ([]model.Placement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Placement, 0, len(m.placements))
	for _, p := range m.placements {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sandbox < out[j].Sandbox })
	return out, nil
}

func (m *Memory) UpdatePlacement(_ context.Context, p model.Placement) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.placements[p.Sandbox]
	if !ok {
		return ErrPlacementNotFound
	}
	if p.Epoch < old.Epoch {
		return model.ErrStaleEpoch
	}
	m.placements[p.Sandbox] = p
	return nil
}

func (m *Memory) DeletePlacement(_ context.Context, sandbox model.SandboxID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.placements[sandbox]; !ok {
		return ErrPlacementNotFound
	}
	delete(m.placements, sandbox)
	return nil
}

func (m *Memory) CreateProcess(_ context.Context, p model.Process) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rec[p.Sandbox]; !ok {
		return ErrNotFound
	}
	if _, ok := m.processes[p.ID]; ok {
		return ErrProcessConflict
	}
	m.processes[p.ID] = p
	return nil
}

func (m *Memory) GetProcess(_ context.Context, id model.ProcessID) (model.Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.processes[id]
	if !ok {
		return model.Process{}, ErrProcessNotFound
	}
	return p, nil
}

func (m *Memory) ListProcesses(_ context.Context, sandbox model.SandboxID) ([]model.Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Process, 0)
	for _, p := range m.processes {
		if p.Sandbox == sandbox {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out, nil
}

func (m *Memory) UpdateProcess(_ context.Context, p model.Process) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.processes[p.ID]
	if !ok {
		return ErrProcessNotFound
	}
	if old.Sandbox != p.Sandbox || old.PlacementEpoch != p.PlacementEpoch || !old.StartedAt.Equal(p.StartedAt) {
		return ErrProcessConflict
	}
	m.processes[p.ID] = p
	return nil
}

func (m *Memory) DeleteProcess(_ context.Context, id model.ProcessID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.processes[id]; !ok {
		return ErrProcessNotFound
	}
	delete(m.processes, id)
	return nil
}

func (m *Memory) ReclaimProcesses(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, p := range m.processes {
		if p.State == model.ProcessExited && p.ExitedAt != nil && !p.ExitedAt.After(before) {
			p.State = model.ProcessReclaimed
			p.PID = 0
			p.ExitCode = nil
			m.processes[id] = p
			n++
		}
	}
	return n, nil
}

func operationKey(scope string, key model.IdempotencyKey) string { return scope + "\x00" + string(key) }

func (m *Memory) CreateOperation(_ context.Context, op model.Operation) error {
	if err := op.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.operations[op.ID]; ok {
		return ErrConflict
	}
	k := operationKey(op.Scope, op.IdempotencyKey)
	if _, ok := m.operationKeys[k]; ok {
		return ErrOperationKeyConflict
	}
	m.operations[op.ID] = op
	m.operationKeys[k] = op.ID
	return nil
}

func (m *Memory) GetOperation(_ context.Context, id model.OperationID) (model.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[id]
	if !ok {
		return model.Operation{}, ErrOperationNotFound
	}
	return op, nil
}

func (m *Memory) GetOperationByKey(_ context.Context, scope string, key model.IdempotencyKey) (model.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.operationKeys[operationKey(scope, key)]
	if !ok {
		return model.Operation{}, ErrOperationNotFound
	}
	op, ok := m.operations[id]
	if !ok {
		return model.Operation{}, ErrOperationNotFound
	}
	return op, nil
}

func (m *Memory) ListOperations(_ context.Context, state model.OperationState) ([]model.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Operation, 0)
	for _, op := range m.operations {
		if state == "" || op.State == state {
			out = append(out, op)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	return out, nil
}

// TakeoverOperation transfers an expired operation lease under the same mutex
// that protects the operation row. The attempt is the ownership token: once a
// contender increments it, every other contender with the old token loses.
func (m *Memory) TakeoverOperation(_ context.Context, id model.OperationID, expectedAttempt uint32, now time.Time, lease time.Duration) (model.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[id]
	if !ok {
		return model.Operation{}, ErrOperationNotFound
	}
	if op.Attempt != expectedAttempt || !op.TakeoverAllowed(now) {
		return model.Operation{}, ErrOperationOwnershipConflict
	}
	if err := op.Takeover(now, lease); err != nil {
		return model.Operation{}, err
	}
	m.operations[id] = op
	return op, nil
}

// RenewOperation extends an operation lease only while the caller still owns
// the current attempt. An expired owner cannot resurrect a lease after a
// competing takeover.
func (m *Memory) RenewOperation(_ context.Context, id model.OperationID, expectedAttempt uint32, now time.Time, lease time.Duration) error {
	if lease <= 0 || now.IsZero() {
		return model.ErrInvalidArgument
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[id]
	if !ok {
		return ErrOperationNotFound
	}
	if op.State != model.OperationInProgress || op.Attempt != expectedAttempt || op.LeaseUntil.IsZero() || !now.Before(op.LeaseUntil) {
		return ErrOperationOwnershipConflict
	}
	if now.Before(op.UpdatedAt) {
		return model.ErrInvalidArgument
	}
	op.UpdatedAt = now
	op.LeaseUntil = now.Add(lease)
	m.operations[id] = op
	return nil
}

func (m *Memory) UpdateOperation(_ context.Context, op model.Operation) error {
	if err := op.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.operations[op.ID]
	if !ok {
		return ErrOperationNotFound
	}
	if old.Scope != op.Scope || old.IdempotencyKey != op.IdempotencyKey || old.RequestHash != op.RequestHash {
		return ErrOperationKeyConflict
	}
	if old.Attempt != op.Attempt || (old.State != model.OperationInProgress && op.State != old.State) {
		return ErrOperationOwnershipConflict
	}
	if op.UpdatedAt.Before(old.UpdatedAt) {
		return ErrOperationOwnershipConflict
	}
	if op.State == model.OperationInProgress && op.LeaseUntil.Before(old.LeaseUntil) {
		return ErrOperationOwnershipConflict
	}
	m.operations[op.ID] = op
	return nil
}

func (m *Memory) DeleteOperation(_ context.Context, id model.OperationID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[id]
	if !ok {
		return ErrOperationNotFound
	}
	delete(m.operations, id)
	delete(m.operationKeys, operationKey(op.Scope, op.IdempotencyKey))
	return nil
}

func (m *Memory) AppendTelemetry(_ context.Context, sample model.TelemetrySample) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rec[sample.Sandbox]; !ok {
		return ErrNotFound
	}
	rows := m.telemetry[sample.Sandbox]
	if sample.Snapshot.At.IsZero() {
		sample.Snapshot.At = time.Now()
	}
	sample.Envelope.Stream = model.CursorStreamTelemetry
	sample.Envelope.ObjectKind = model.ObjectSandbox
	sample.Envelope.ObjectID = string(sample.Sandbox)
	if sample.Envelope.OccurredAt.IsZero() {
		sample.Envelope.OccurredAt = sample.Snapshot.At
	}
	sequence := m.telemetrySeq[sample.Sandbox] + 1
	sample.Envelope.Sequence = sequence
	if sample.Envelope.ID == "" {
		sample.Envelope.ID = fmt.Sprintf("telemetry_%s_%d", sample.Sandbox, sample.Envelope.Sequence)
	}
	if err := sample.Validate(); err != nil {
		return err
	}
	m.telemetrySeq[sample.Sandbox] = sequence
	m.telemetry[sample.Sandbox] = append(rows, sample)
	return nil
}

func (m *Memory) Telemetry(_ context.Context, sandbox model.SandboxID, after uint64, limit int) ([]model.TelemetrySample, uint64, uint64, error) {
	if limit <= 0 {
		return nil, 0, 0, fmt.Errorf("store: Telemetry limit must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.telemetry[sandbox]
	if len(rows) == 0 {
		return []model.TelemetrySample{}, 0, 0, nil
	}
	out := make([]model.TelemetrySample, 0, limit)
	for _, sample := range rows {
		if sample.Envelope.Sequence > after {
			out = append(out, sample)
			if len(out) == limit {
				break
			}
		}
	}
	return out, rows[0].Envelope.Sequence, rows[len(rows)-1].Envelope.Sequence, nil
}

func (m *Memory) TrimTelemetry(_ context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("store: TrimTelemetry limit must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	deleted := 0
	for sandbox, rows := range m.telemetry {
		if len(rows) < 2 {
			continue
		}
		keep := make([]model.TelemetrySample, 0, len(rows))
		for i, sample := range rows {
			if deleted < limit && i < len(rows)-1 && sample.Snapshot.At.Before(before) {
				deleted++
				continue
			}
			keep = append(keep, sample)
		}
		m.telemetry[sandbox] = keep
	}
	return deleted, nil
}
