package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/TSHOGX/khora/internal/model"
)

// SampleTelemetry records one non-blocking best-effort object sample. Provider
// values that cannot be measured remain explicitly unavailable.
func (m *Mediator) SampleTelemetry(ctx context.Context, id model.SandboxID) error {
	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now()
	sample := model.TelemetrySample{
		Sandbox:  id,
		Snapshot: model.ResourceSnapshot{State: string(rec.State), At: now, CPU: model.UnavailableMeasurement(), Memory: model.UnavailableMeasurement(), Disk: model.UnavailableMeasurement()},
		Envelope: model.EventEnvelope{ID: "", Stream: model.CursorStreamTelemetry, ObjectKind: model.ObjectSandbox, ObjectID: string(id), OccurredAt: now},
	}
	if m.deps.Rootfs != nil && rec.Overlay != "" {
		if used, _, uerr := m.deps.Rootfs.Usage(ctx, string(rec.Overlay)); uerr == nil {
			sample.Snapshot.Disk = model.Measured(used)
		}
	}
	if p, perr := m.deps.Store.GetPlacement(ctx, id); perr == nil {
		sample.PlacementEpoch = p.Epoch
	}
	if err := m.deps.Store.AppendTelemetry(ctx, sample); err != nil {
		return fmt.Errorf("append telemetry: %w", err)
	}
	return nil
}

func (m *Mediator) Telemetry(ctx context.Context, id model.SandboxID, after uint64, limit int) ([]model.TelemetrySample, uint64, uint64, error) {
	if _, err := m.deps.Store.GetSandbox(ctx, id); err != nil {
		return nil, 0, 0, err
	}
	samples, earliest, latest, err := m.deps.Store.Telemetry(ctx, id, after, limit)
	if err != nil {
		return nil, 0, 0, err
	}
	if after > 0 && earliest > 0 && after+1 < earliest {
		return nil, earliest, latest, model.ErrCursorExpired
	}
	return samples, earliest, latest, nil
}

func (m *Mediator) TrimTelemetry(ctx context.Context) error {
	policy := m.deps.TelemetryPolicy
	if policy.Retention <= 0 {
		return nil
	}
	_, err := m.deps.Store.TrimTelemetry(ctx, time.Now().Add(-policy.Retention), 1000)
	return err
}

func (m *Mediator) SandboxSnapshot(ctx context.Context, id model.SandboxID) (model.SandboxSnapshot, error) {
	rec, err := m.deps.Store.GetSandbox(ctx, id)
	if err != nil {
		return model.SandboxSnapshot{}, err
	}
	now := time.Now()
	s := model.SandboxSnapshot{Sandbox: id, State: string(rec.State), At: now, Resources: model.ResourceSnapshot{State: string(rec.State), At: now, CPU: model.UnavailableMeasurement(), Memory: model.UnavailableMeasurement(), Disk: model.UnavailableMeasurement()}}
	if p, err := m.deps.Store.GetPlacement(ctx, id); err == nil {
		s.PlacementEpoch = p.Epoch
	}
	if m.deps.Rootfs != nil {
		if used, _, err := m.deps.Rootfs.Usage(ctx, string(rec.Overlay)); err == nil {
			s.Resources.Disk = model.Measured(used)
		}
	}
	return s, nil
}

func (m *Mediator) PlacementSnapshot(ctx context.Context, id model.SandboxID) (model.PlacementSnapshot, error) {
	p, err := m.deps.Store.GetPlacement(ctx, id)
	if err != nil {
		return model.PlacementSnapshot{}, err
	}
	now := time.Now()
	return model.PlacementSnapshot{Sandbox: id, Epoch: p.Epoch, State: p.State, Resources: model.ResourceSnapshot{State: string(p.State), At: now, CPU: model.UnavailableMeasurement(), Memory: model.UnavailableMeasurement(), Disk: model.UnavailableMeasurement()}, At: now}, nil
}

func (m *Mediator) ProcessSnapshot(ctx context.Context, id model.ProcessID) (model.ProcessSnapshot, error) {
	p, err := m.deps.Store.GetProcess(ctx, id)
	if err != nil {
		return model.ProcessSnapshot{}, err
	}
	return model.ProcessSnapshot{Process: p.ID, Sandbox: p.Sandbox, PlacementEpoch: p.PlacementEpoch, State: p.State, PID: p.PID, ExitCode: p.ExitCode, At: time.Now()}, nil
}
