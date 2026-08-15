package store_test

import (
	"path/filepath"
	"testing"

	"github.com/TSHOGX/khora/internal/store"
	"github.com/TSHOGX/khora/internal/store/storetest"
)

// SQLite runs the SAME the transactional outbox contract suite as Memory (the extension boundary seam: the two
// backends run one set of assertions). It uses a real on-disk file so the
// Durability subtest can reopen it — proving records AND unpublished outbox rows
// survive a process restart, the substrate for atomic transition rollback reconciliation and
// redelivery.
func TestSQLiteContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) (store.Store, func(t *testing.T) store.Store) {
		dir := t.TempDir()
		path := filepath.Join(dir, "store.db")
		s, err := store.OpenSQLite(path)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		reopen := func(t *testing.T) store.Store {
			// close the current handle first (WAL checkpoint) then reopen the
			// SAME file — a clean process-restart simulation.
			_ = s.Close()
			s2, err := store.OpenSQLite(path)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			s = s2
			t.Cleanup(func() { _ = s2.Close() })
			return s2
		}
		return s, reopen
	})
}
