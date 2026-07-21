package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"engine/internal/model"
	"engine/internal/store"

	_ "modernc.org/sqlite"
)

// A store.db created before the M4 TTL Binding columns must migrate cleanly:
// OpenSQLite adds idle_timeout/retention via idempotent ALTER, old rows backfill
// to 0 (the reaper reads 0 as "use the engine default"), and a re-open of an
// already-migrated file is a no-op (the duplicate-column ALTER is swallowed).
func TestSQLiteMigratesPreM4Schema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")

	// Hand-build a PRE-M4 sandboxes table: no idle_timeout/retention columns.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	_, err = raw.ExecContext(ctx, `
		CREATE TABLE sandboxes (
			id TEXT PRIMARY KEY, template TEXT NOT NULL, overlay TEXT NOT NULL,
			state TEXT NOT NULL, guest_user TEXT NOT NULL,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		);
		INSERT INTO sandboxes VALUES('old','bare','ov_old','paused','', 1, 2);`)
	if err != nil {
		t.Fatalf("seed pre-M4 row: %v", err)
	}
	_ = raw.Close()

	// OpenSQLite must migrate (ALTER ADD COLUMN), not fail.
	s, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite (migrate): %v", err)
	}
	rec, err := s.GetSandbox(ctx, "old")
	if err != nil {
		t.Fatalf("get migrated row: %v", err)
	}
	if rec.State != model.Paused {
		t.Fatalf("migrated row state=%s; want paused (data preserved)", rec.State)
	}
	// old row backfilled to 0 TTL (reaper reads as "engine default").
	if rec.IdleTimeout != 0 || rec.Retention != 0 {
		t.Fatalf("pre-M4 row TTL = %s/%s; want 0/0 (backfill)", rec.IdleTimeout, rec.Retention)
	}
	// a NEW insert on the migrated DB round-trips a concrete TTL.
	now := time.Now()
	if err := s.CreateSandbox(ctx, model.Sandbox{
		ID: "new", Template: "bare", Overlay: "ov_new", State: model.Creating,
		IdleTimeout: 5 * time.Minute, Retention: time.Hour, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("create on migrated DB: %v", err)
	}
	got, _ := s.GetSandbox(ctx, "new")
	if got.IdleTimeout != 5*time.Minute || got.Retention != time.Hour {
		t.Fatalf("new row TTL = %s/%s; want 5m/1h", got.IdleTimeout, got.Retention)
	}
	_ = s.Close()

	// re-open an already-migrated file: the ALTER is a no-op (duplicate column
	// swallowed), not an error.
	s2, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("re-open migrated DB: %v", err)
	}
	_ = s2.Close()
}
