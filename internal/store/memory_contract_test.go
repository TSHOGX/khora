package store_test

import (
	"testing"

	"github.com/TSHOGX/warren/internal/store"
	"github.com/TSHOGX/warren/internal/store/storetest"
)

// Memory runs the shared §4.4-C contract suite. It is not durable, so reopen is
// nil (the Durability subtest skips). SQLite runs the SAME suite with a real
// reopen (memory_contract vs sqlite_contract, one set of assertions).
func TestMemoryContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) (store.Store, func(t *testing.T) store.Store) {
		return store.NewMemory(), nil
	})
}
