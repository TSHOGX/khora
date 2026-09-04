package lifecycle

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
	"github.com/TSHOGX/khora/internal/store"
)

// A maximum cursor is a valid tail position.  The expiry predicate must not
// compute after+1, because that wraps uint64 and turns a normal empty page into
// a false cursor-expired refusal.
func TestTelemetryMaximumCursorIsNotExpired(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func(t *testing.T) store.Store
	}{
		{name: "memory", mk: func(t *testing.T) store.Store { return store.NewMemory() }},
		{name: "sqlite", mk: func(t *testing.T) store.Store {
			st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "telemetry.db"))
			if err != nil {
				t.Fatalf("OpenSQLite: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			return st
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.mk(t)
			if err := st.CreateTemplate(ctx, model.Template{ID: "bare", Builtin: true, Manifest: model.DefaultTemplateManifest(), CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateSandbox(ctx, model.Sandbox{ID: "sbx_cursor", Template: "bare", Overlay: "ov_cursor", State: model.Paused, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil); err != nil {
				t.Fatal(err)
			}
			m := New(Deps{Store: st})
			for i := 0; i < 3; i++ {
				if err := m.SampleTelemetry(ctx, "sbx_cursor"); err != nil {
					t.Fatalf("sample %d: %v", i, err)
				}
			}
			if n, err := st.TrimTelemetry(ctx, time.Now().Add(time.Hour), 100); err != nil || n != 2 {
				t.Fatalf("trim = %d, %v; want two old samples removed", n, err)
			}

			for _, tc := range []struct {
				name      string
				after     uint64
				wantError error
				wantCount int
			}{
				{name: "expired", after: 1, wantError: model.ErrCursorExpired, wantCount: 0},
				{name: "exclusive boundary", after: 2, wantCount: 1},
				{name: "maximum", after: ^uint64(0), wantCount: 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					page, earliest, latest, err := m.Telemetry(ctx, "sbx_cursor", tc.after, 10)
					if tc.wantError != nil {
						if !errors.Is(err, tc.wantError) {
							t.Fatalf("after=%d err=%v; want %v", tc.after, err, tc.wantError)
						}
					} else if err != nil {
						t.Fatalf("after=%d err=%v; want success", tc.after, err)
					}
					if len(page) != tc.wantCount || earliest != 3 || latest != 3 {
						t.Fatalf("after=%d page=%d bounds=%d/%d; want %d and 3/3", tc.after, len(page), earliest, latest, tc.wantCount)
					}
				})
			}
		})
	}
}
