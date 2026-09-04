package lifecycle

import (
	"context"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

// A maximum cursor is a valid tail position.  The expiry predicate must not
// compute after+1, because that wraps uint64 and turns a normal empty page into
// a false cursor-expired refusal.
func TestTelemetryMaximumCursorIsNotExpired(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.CreateTemplate(ctx, model.Template{ID: "bare", Builtin: true, Manifest: model.DefaultTemplateManifest(), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSandbox(ctx, model.Sandbox{ID: "sbx_cursor", Template: "bare", Overlay: "ov_cursor", State: model.Paused, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	m := New(Deps{Store: st})
	for i := 0; i < 2; i++ {
		if err := m.SampleTelemetry(ctx, "sbx_cursor"); err != nil {
			t.Fatal(err)
		}
	}
	page, earliest, latest, err := m.Telemetry(ctx, "sbx_cursor", ^uint64(0), 10)
	if err != nil {
		t.Fatalf("maximum telemetry cursor = %v; want an empty tail page", err)
	}
	if len(page) != 0 || earliest == 0 || latest == 0 {
		t.Fatalf("maximum telemetry cursor page=%d bounds=%d/%d; want empty page with retained bounds", len(page), earliest, latest)
	}
}
