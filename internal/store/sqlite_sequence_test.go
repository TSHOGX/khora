package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/TSHOGX/khora/internal/model"
)

func TestSQLiteEventSequenceExhaustionLeavesTransitionAtomic(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTemplate(ctx, model.Template{ID: "bare", Builtin: true, Manifest: model.DefaultTemplateManifest(), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSandbox(ctx, model.Sandbox{ID: "sbx", Template: "bare", Overlay: "ov_sbx", State: model.Creating, CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO sqlite_sequence(name, seq) VALUES('outbox', ?)`, maxEventSequence); err != nil {
		t.Fatal(err)
	}
	err = s.CompareAndSetState(ctx, "sbx", model.Creating, model.Running, &Event{ID: "evt_max", Sandbox: "sbx", Kind: model.EventCreated, State: model.Running, OccurredAt: time.Now()}, nil)
	if !errors.Is(err, ErrEventSequenceExhausted) {
		t.Fatalf("CompareAndSetState at max SQLite sequence = %v; want ErrEventSequenceExhausted", err)
	}
	rec, err := s.GetSandbox(ctx, "sbx")
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != model.Creating {
		t.Fatalf("state after exhausted SQLite transition = %s; want creating", rec.State)
	}
	if _, earliest, latest, err := s.Events(ctx, 0, 10); err != nil || earliest != 0 || latest != 0 {
		t.Fatalf("outbox after exhausted SQLite transition = bounds %d/%d err %v; want empty", earliest, latest, err)
	}
}
