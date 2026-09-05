package model_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
)

func requireClass(t *testing.T, err error, want model.ErrorClass) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error; want class %q", want)
	}
	if got := model.ErrorClassOf(err); got != want {
		t.Fatalf("error class = %q, want %q (err=%v)", got, want, err)
	}
}

func TestPlacementEpochAdoptResumeAndStaleRequest(t *testing.T) {
	got, err := model.NextPlacementEpoch(0)
	if err != nil || got != model.FirstPlacementEpoch {
		t.Fatalf("first resume epoch = %d, %v; want %d", got, err, model.FirstPlacementEpoch)
	}
	got, err = model.NextPlacementEpoch(41)
	if err != nil || got != 42 {
		t.Fatalf("next resume epoch = %d, %v; want 42", got, err)
	}
	got, err = model.AdoptPlacementEpoch(41)
	if err != nil || got != 41 {
		t.Fatalf("adopt epoch = %d, %v; want 41 (adopt must preserve the epoch)", got, err)
	}
	_, err = model.AdoptPlacementEpoch(0)
	requireClass(t, err, model.ErrorInvalidArgument)
	if err := model.ValidateEpoch(7, 8); err == nil {
		t.Fatal("stale epoch was accepted")
	} else {
		requireClass(t, err, model.ErrorStaleEpoch)
	}
	if err := model.ValidateEpoch(7, 7); err != nil {
		t.Fatalf("matching epoch refused: %v", err)
	}
	wrapped := fmt.Errorf("request: %w", model.ErrStaleEpoch)
	if !errors.Is(wrapped, model.ErrStaleEpoch) {
		t.Fatal("errors.Is did not preserve the stale-epoch class through wrapping")
	}
	_, err = model.NextPlacementEpoch(^model.PlacementEpoch(0))
	requireClass(t, err, model.ErrorInternal)
}

func TestPlacementAndProcessValidatePortableShapes(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	p := model.Placement{
		Sandbox:   "sbx_a",
		Epoch:     3,
		Provider:  "local",
		State:     model.PlacementActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid placement refused: %v", err)
	}
	if !p.Matches("sbx_a", 3) || p.Matches("sbx_a", 2) {
		t.Fatal("placement matching did not bind Sandbox and epoch together")
	}
	exit := now.Add(time.Minute)
	code := 0
	proc := model.Process{
		ID:             "proc_a",
		Sandbox:        "sbx_a",
		PlacementEpoch: 3,
		PID:            91,
		State:          model.ProcessExited,
		ExitCode:       &code,
		StartedAt:      now,
		ExitedAt:       &exit,
		OutputCursor:   model.ProcessOutputCursor("proc_a", 3, 9),
	}
	if err := proc.Validate(); err != nil {
		t.Fatalf("valid process refused: %v", err)
	}
	if !proc.IsStale(4) || proc.IsStale(3) {
		t.Fatal("process stale check did not use the placement epoch")
	}
	proc.OutputCursor = model.ProcessOutputCursor("proc_a", 2, 9)
	requireClass(t, proc.Validate(), model.ErrorInvalidArgument)
}

func TestProcessAllowsHandleFingerprintWithoutSeparateKey(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	proc := model.Process{
		ID:               "proc_caller",
		Sandbox:          "sbx_a",
		PlacementEpoch:   1,
		PID:              0,
		State:            model.ProcessStarting,
		StartedAt:        now,
		OutputCursor:     model.ProcessOutputCursor("proc_caller", 1, 0),
		StartRequestHash: model.DigestBytes([]byte("start-request")),
	}
	if err := proc.Validate(); err != nil {
		t.Fatalf("handle-scoped fingerprint without key refused: %v", err)
	}
}

func TestProcessExitRetentionAndReclaimBoundary(t *testing.T) {
	if err := model.DefaultProcessRetention.Validate(); err != nil {
		t.Fatalf("default process retention invalid: %v", err)
	}
	if model.DefaultProcessExitRetention != 24*time.Hour {
		t.Fatalf("default process retention = %s, want 24h", model.DefaultProcessExitRetention)
	}
	started := time.Unix(100, 0).UTC()
	exited := started.Add(time.Hour)
	code := 17
	proc := model.Process{
		ID:             "proc_a",
		Sandbox:        "sbx_a",
		PlacementEpoch: 1,
		PID:            3,
		State:          model.ProcessExited,
		ExitCode:       &code,
		StartedAt:      started,
		ExitedAt:       &exited,
		OutputCursor:   model.ProcessOutputCursor("proc_a", 1, 0),
	}
	deadline := exited.Add(model.DefaultProcessExitRetention)
	if proc.Reclaimable(deadline.Add(-time.Nanosecond), model.DefaultProcessRetention) {
		t.Fatal("process reclaimed before the retention boundary")
	}
	if !proc.Reclaimable(deadline, model.DefaultProcessRetention) {
		t.Fatal("process was not reclaimable at the inclusive retention boundary")
	}
}

func TestReclaimedProcessClearsLocatorAndExitCode(t *testing.T) {
	now := time.Now()
	code := 1
	bad := model.Process{
		ID: "proc_reclaimed", Sandbox: "sbx_reclaimed", PlacementEpoch: 1,
		PID: 7, State: model.ProcessReclaimed, ExitCode: &code,
		StartedAt: now, ExitedAt: ptrTime(now),
		OutputCursor: model.ProcessOutputCursor("proc_reclaimed", 1, 0),
	}
	requireClass(t, bad.Validate(), model.ErrorInvalidArgument)
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestTemplateManifestNormalizationValidationAndDigest(t *testing.T) {
	content := []byte("immutable template bytes")
	digest := model.DigestBytes(content)
	m := model.TemplateManifest{
		SchemaVersion:   model.CurrentManifestSchema,
		DigestAlgorithm: model.DigestSHA256,
		ContentDigest:   digest,
		Platform:        "linux/amd64",
		Capabilities:    []string{"network", "fs", "network"},
		Provenance:      "oci://example/template",
		SBOMReference:   "https://example.invalid/sbom.json",
	}
	if err := m.Validate(); err == nil {
		t.Fatal("duplicate capabilities accepted before normalization")
	} else {
		requireClass(t, err, model.ErrorInvalidArgument)
	}
	m = m.Normalize()
	if err := m.Validate(); err != nil {
		t.Fatalf("normalized manifest refused: %v", err)
	}
	if got, want := m.Capabilities, []string{"fs", "network"}; !equalStrings(got, want) {
		t.Fatalf("normalized capabilities = %v, want %v", got, want)
	}
	if digest != "sha256:4beb0cebe1b869415d06b2c94e260ea7abb700654d2733215b24c5fd0445baff" {
		t.Fatalf("digest spelling changed: %q", digest)
	}
	bad := m
	bad.ContentDigest = "sha256:not-hex"
	requireClass(t, bad.Validate(), model.ErrorInvalidArgument)
	bad = m
	bad.DigestAlgorithm = "blake3"
	requireClass(t, bad.Validate(), model.ErrorUnsupported)
	bad = m
	bad.SchemaVersion = model.CurrentManifestSchema + 1
	requireClass(t, bad.Validate(), model.ErrorInvalidArgument)
	bad = m
	bad.Capabilities = nil
	requireClass(t, bad.Validate(), model.ErrorInvalidArgument)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOperationHashKeyReuseAndRestartTakeover(t *testing.T) {
	first, err := model.RequestHash(map[string]any{"template_id": "bare", "size": 0})
	if err != nil {
		t.Fatalf("hash request: %v", err)
	}
	second, err := model.RequestHash(map[string]any{"size": 0, "template_id": "bare"})
	if err != nil {
		t.Fatalf("hash reordered request: %v", err)
	}
	if first != second {
		t.Fatalf("semantic map reorder changed request hash: %q != %q", first, second)
	}
	now := time.Unix(200, 0).UTC()
	op, err := model.BeginOperation("op_a", "sandbox.create", "key_a", first, now, time.Minute)
	if err != nil {
		t.Fatalf("begin operation: %v", err)
	}
	if err := op.Validate(); err != nil {
		t.Fatalf("valid operation refused: %v", err)
	}
	if err := op.SameRequest(first); err != nil {
		t.Fatalf("same idempotency request did not converge: %v", err)
	}
	other, _ := model.RequestHash(map[string]any{"template_id": "other", "size": 0})
	requireClass(t, op.SameRequest(other), model.ErrorIdempotencyConflict)
	if err := op.Takeover(now.Add(30*time.Second), time.Minute); err == nil {
		t.Fatal("unexpired operation lease was taken over")
	} else {
		requireClass(t, err, model.ErrorInProgress)
	}
	// An expired row remains a valid recovery input; validation must not erase
	// the restart takeover window.
	if err := op.Validate(); err != nil {
		t.Fatalf("expired in-progress operation became invalid: %v", err)
	}
	if err := op.Takeover(now.Add(time.Minute), time.Minute); err != nil {
		t.Fatalf("expired operation was not taken over: %v", err)
	}
	if op.Attempt != 2 || op.Phase != model.OperationRecorded || !op.LeaseUntil.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("takeover = attempt %d phase %q lease %s", op.Attempt, op.Phase, op.LeaseUntil)
	}
	op.Phase = model.OperationMaterialized
	if got := op.RecoveryAction(); got != model.RecoveryCleanup {
		t.Fatalf("materialized recovery action = %q, want cleanup_then_retry", got)
	}
	op.Phase = model.OperationRecorded
	if got := op.RecoveryAction(); got != model.RecoveryRetry {
		t.Fatalf("recorded recovery action = %q, want retry", got)
	}
	if err := op.Advance(model.OperationMaterializing, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("advance operation: %v", err)
	}
	if err := op.Advance(model.OperationRecorded, now.Add(3*time.Minute)); err == nil {
		t.Fatal("operation phase moved backwards")
	} else {
		requireClass(t, err, model.ErrorConflict)
	}
	if err := op.Succeed(model.OperationResult{Kind: model.ObjectSandbox, ID: "sbx_a"}, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("commit operation: %v", err)
	}
	if err := op.Validate(); err != nil {
		t.Fatalf("committed operation invalid: %v", err)
	}
	if err := op.SameRequest(first); err != nil {
		t.Fatalf("committed operation did not retain request hash: %v", err)
	}
	if err := op.Advance(model.OperationCleaning, now.Add(5*time.Minute)); err == nil {
		t.Fatal("terminal operation advanced")
	} else {
		requireClass(t, err, model.ErrorConflict)
	}
}

func TestCursorEncodingStreamsAndResetRules(t *testing.T) {
	c := model.ProcessOutputCursor("proc_a", 7, 12)
	raw, err := c.Encode()
	if err != nil {
		t.Fatalf("encode process cursor: %v", err)
	}
	parsed, err := model.ParseCursor(raw)
	if err != nil {
		t.Fatalf("parse process cursor: %v", err)
	}
	if cmp, err := parsed.Compare(c); err != nil || cmp != 0 {
		t.Fatalf("cursor round trip compare = %d, %v", cmp, err)
	}
	if _, err := model.ParseCursor("v0." + raw[3:]); err == nil {
		t.Fatal("unsupported cursor version accepted")
	} else {
		requireClass(t, err, model.ErrorInvalidArgument)
	}
	window, err := model.NewCursorWindow(c, 5, 20)
	if err != nil {
		t.Fatalf("cursor window: %v", err)
	}
	if err := window.Accept(model.ProcessOutputCursor("proc_a", 7, 4)); err != nil {
		t.Fatalf("cursor at retained boundary refused: %v", err)
	}
	if empty, err := model.NewCursorWindow(c, 0, 0); err != nil {
		t.Fatalf("empty cursor window: %v", err)
	} else if err := empty.Accept(model.ProcessOutputCursor("proc_a", 7, 1)); err == nil {
		t.Fatal("cursor accepted against an empty retained window")
	} else {
		requireClass(t, err, model.ErrorCursorExpired)
	}
	if err := window.Accept(model.ProcessOutputCursor("proc_a", 7, 1)); err != nil {
		requireClass(t, err, model.ErrorCursorExpired)
	} else {
		t.Fatal("expired cursor accepted")
	}
	if err := window.Accept(model.ProcessOutputCursor("proc_a", 8, 12)); err == nil {
		t.Fatal("cursor from a different placement epoch accepted")
	} else {
		requireClass(t, err, model.ErrorStaleEpoch)
	}
	if err := window.Accept(model.WatchCursor("sbx_a", 7, 12)); err == nil {
		t.Fatal("cursor from a different stream accepted")
	} else {
		requireClass(t, err, model.ErrorInvalidArgument)
	}
	window.Overflow = true
	requireClass(t, window.Accept(model.ProcessOutputCursor("proc_a", 7, 12)), model.ErrorCursorGap)
	reset, err := model.PlacementReset(model.CursorStreamProcessOutput, "proc_a", 8)
	if err != nil {
		t.Fatalf("placement reset: %v", err)
	}
	if reset.Reason != model.CursorResetPlacement || !reset.Rescan || reset.Cursor.Epoch != 8 || reset.Cursor.Sequence != 0 {
		t.Fatalf("placement reset = %+v", reset)
	}
	if _, err := model.PlacementReset(model.CursorStreamLifecycle, "", 1); err == nil {
		t.Fatal("lifecycle cursor accepted a placement reset")
	} else {
		requireClass(t, err, model.ErrorInvalidArgument)
	}
	if _, err := model.PlacementReset(model.CursorStreamWatch, "", 8); err == nil {
		t.Fatal("watch reset accepted an empty object ID")
	} else {
		requireClass(t, err, model.ErrorInvalidArgument)
	}
}

func TestCrossEdgeJSONContract(t *testing.T) {
	cases := []struct {
		name string
		got  any
		want string
	}{
		{
			name: "lifecycle cursor",
			got:  model.LifecycleCursor(4),
			want: `{"stream":"lifecycle","sequence":4}`,
		},
		{
			name: "placement cursor",
			got:  model.WatchCursor("sbx_a", 2, 0),
			want: `{"stream":"watch","object_id":"sbx_a","epoch":2,"sequence":0}`,
		},
		{
			name: "telemetry cursor",
			got:  model.TelemetryCursor("sbx_a", 9),
			want: `{"stream":"telemetry","object_id":"sbx_a","sequence":9}`,
		},
		{
			name: "measurement",
			got:  model.Measured(0),
			want: `{"available":true,"value":0}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("JSON = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestMeasurementZeroAndUnavailableAreDistinct(t *testing.T) {
	zero := model.Measured(0)
	unknown := model.UnavailableMeasurement()
	if err := zero.Validate(); err != nil {
		t.Fatalf("measured zero refused: %v", err)
	}
	if err := unknown.Validate(); err != nil {
		t.Fatalf("unavailable measurement refused: %v", err)
	}
	zeroJSON, _ := json.Marshal(zero)
	unknownJSON, _ := json.Marshal(unknown)
	if bytes.Equal(zeroJSON, unknownJSON) {
		t.Fatalf("measured zero and unavailable serialized identically: %s", zeroJSON)
	}
	if string(zeroJSON) != `{"available":true,"value":0}` {
		t.Fatalf("measured zero JSON = %s", zeroJSON)
	}
	if string(unknownJSON) != `{"available":false,"value":0}` {
		t.Fatalf("unavailable JSON = %s", unknownJSON)
	}
	bad := model.Measurement{Available: false, Value: 1}
	requireClass(t, bad.Validate(), model.ErrorInvalidArgument)
}

func TestDecisionGatePolicies(t *testing.T) {
	if err := model.DefaultWatchPolicy.Validate(); err != nil {
		t.Fatalf("default watch policy invalid: %v", err)
	}
	if model.DefaultWatchPolicy.Semantics != model.WatchChangeDetection || model.DefaultWatchPolicy.BufferEvents != 4096 || !model.DefaultWatchPolicy.ResetOnEpoch {
		t.Fatalf("watch policy = %+v", model.DefaultWatchPolicy)
	}
	if err := model.DefaultTelemetryPolicy.Validate(); err != nil {
		t.Fatalf("default telemetry policy invalid: %v", err)
	}
	if model.DefaultTelemetryPolicy.Interval != 10*time.Second || model.DefaultTelemetryPolicy.Retention != 24*time.Hour {
		t.Fatalf("telemetry policy = %+v", model.DefaultTelemetryPolicy)
	}
	badWatch := model.DefaultWatchPolicy
	badWatch.Semantics = "exact"
	requireClass(t, badWatch.Validate(), model.ErrorUnsupported)
	badTelemetry := model.DefaultTelemetryPolicy
	badTelemetry.Retention = time.Second
	requireClass(t, badTelemetry.Validate(), model.ErrorInvalidArgument)
}

func TestNegativeFixtures(t *testing.T) {
	t.Run("stale-epoch", func(t *testing.T) {
		var fixture struct {
			Expected model.PlacementEpoch `json:"expected"`
			Actual   model.PlacementEpoch `json:"actual"`
		}
		loadFixture(t, "stale-epoch.json", &fixture)
		requireClass(t, model.ValidateEpoch(fixture.Expected, fixture.Actual), model.ErrorStaleEpoch)
	})
	t.Run("key-reuse", func(t *testing.T) {
		var fixture struct {
			Stored  string `json:"stored_hash"`
			Request string `json:"request_hash"`
		}
		loadFixture(t, "key-reuse.json", &fixture)
		op := model.Operation{RequestHash: fixture.Stored}
		requireClass(t, op.SameRequest(fixture.Request), model.ErrorIdempotencyConflict)
	})
	t.Run("unavailable-measurement", func(t *testing.T) {
		var fixture model.Measurement
		loadFixture(t, "unavailable-measurement.json", &fixture)
		requireClass(t, fixture.Validate(), model.ErrorInvalidArgument)
	})
	t.Run("generated-drift", func(t *testing.T) {
		raw := readFixture(t, "generated-drift.json")
		encoded := model.CursorEncodingVersion + "." + base64.RawURLEncoding.EncodeToString(raw)
		if _, err := model.ParseCursor(encoded); err == nil {
			t.Fatal("fixture with an unknown generated field decoded successfully")
		} else {
			requireClass(t, err, model.ErrorInvalidArgument)
		}
	})
}

func fixturePath(name string) string {
	return filepath.Join("testdata", name)
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func loadFixture(t *testing.T, name string, dst any) {
	t.Helper()
	if err := json.Unmarshal(readFixture(t, name), dst); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
}
