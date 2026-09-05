package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
)

func telemetrySequenceSandbox(t *testing.T, s Store, id model.SandboxID) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateTemplate(ctx, model.Template{
		ID: "bare", Builtin: true, Manifest: model.DefaultTemplateManifest(), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSandbox(ctx, model.Sandbox{
		ID: id, Template: "bare", Overlay: model.OverlayID("ov_" + string(id)), State: model.Paused,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil); err != nil {
		t.Fatal(err)
	}
}

func validTelemetrySample(id model.SandboxID) model.TelemetrySample {
	now := time.Now().UTC()
	return model.TelemetrySample{
		Sandbox: id,
		Snapshot: model.ResourceSnapshot{
			CPU: model.Measured(1), Memory: model.Measured(2), Disk: model.Measured(3),
			State: "paused", At: now,
		},
	}
}

func TestMemoryTelemetrySequenceExhaustionDoesNotWrapOrMutate(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	const id model.SandboxID = "sbx_telemetry_max"
	telemetrySequenceSandbox(t, s, id)

	s.mu.Lock()
	s.telemetrySeq[id] = maxTelemetrySequence
	s.mu.Unlock()
	if err := s.AppendTelemetry(ctx, validTelemetrySample(id)); !errors.Is(err, ErrTelemetrySequenceExhausted) {
		t.Fatalf("append at max telemetry sequence = %v; want ErrTelemetrySequenceExhausted", err)
	}
	s.mu.Lock()
	seq, rows := s.telemetrySeq[id], len(s.telemetry[id])
	s.mu.Unlock()
	if seq != maxTelemetrySequence || rows != 0 {
		t.Fatalf("memory telemetry after exhaustion: seq=%d rows=%d; want unchanged max/0", seq, rows)
	}
}

func TestMemoryTelemetrySequenceReachesCeilingThenRefuses(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	const id model.SandboxID = "sbx_telemetry_ceiling"
	telemetrySequenceSandbox(t, s, id)
	s.mu.Lock()
	s.telemetrySeq[id] = maxTelemetrySequence - 1
	s.mu.Unlock()
	if err := s.AppendTelemetry(ctx, validTelemetrySample(id)); err != nil {
		t.Fatalf("append at max-1 = %v; want sequence %d to be representable", err, maxTelemetrySequence)
	}
	page, _, latest, err := s.Telemetry(ctx, id, 0, 2)
	if err != nil || len(page) != 1 || latest != maxTelemetrySequence {
		t.Fatalf("telemetry at ceiling = page=%+v latest=%d err=%v; want one sample at max", page, latest, err)
	}
	if err := s.AppendTelemetry(ctx, validTelemetrySample(id)); !errors.Is(err, ErrTelemetrySequenceExhausted) {
		t.Fatalf("append after ceiling = %v; want ErrTelemetrySequenceExhausted", err)
	}
}

func TestSQLiteTelemetrySequenceExhaustionDoesNotWrapOrMutate(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const id model.SandboxID = "sbx_sqlite_telemetry_max"
	telemetrySequenceSandbox(t, s, id)
	if _, err := s.db.Exec(`INSERT INTO telemetry(sandbox,sequence,event_id,placement_epoch,process_id,state,cpu_available,cpu_value,memory_available,memory_value,disk_available,disk_value,at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(id), int64(maxTelemetrySequence), "telemetry_max", 0, nil, "paused", 1, 1, 1, 2, 1, 3, time.Now().UnixNano()); err != nil {
		t.Fatalf("seed maximum telemetry row: %v", err)
	}
	if err := s.AppendTelemetry(ctx, validTelemetrySample(id)); !errors.Is(err, ErrTelemetrySequenceExhausted) {
		t.Fatalf("sqlite append at max telemetry sequence = %v; want ErrTelemetrySequenceExhausted", err)
	}
	page, earliest, latest, err := s.Telemetry(ctx, id, 0, 10)
	if err != nil || len(page) != 1 || earliest != maxTelemetrySequence || latest != maxTelemetrySequence {
		t.Fatalf("sqlite telemetry after exhaustion = page=%+v bounds=%d/%d err=%v; want original maximum row", page, earliest, latest, err)
	}
}
