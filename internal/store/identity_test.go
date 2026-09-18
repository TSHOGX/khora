package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/TSHOGX/khora/internal/model"
)

func TestSQLiteIdentityConcurrentInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	const count = 8
	ids := make(chan model.StoreID, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := OpenSQLite(path)
			if err != nil {
				errs <- err
				return
			}
			defer s.Close()
			id, err := s.Identity(context.Background())
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var first model.StoreID
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("concurrent open produced different identities")
		}
	}
}

func TestSQLiteIdentityNeverRepairsExistingState(t *testing.T) {
	for _, mutation := range []string{"DROP TABLE store_identity", "DELETE FROM store_identity", "UPDATE store_identity SET id='broken'"} {
		t.Run(mutation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			s, err := OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Identity(context.Background()); err == nil {
				t.Fatal("damaged identity readable")
			}
			s.Close()
			if reopened, err := OpenSQLite(path); err == nil {
				reopened.Close()
				t.Fatal("damaged identity replaced")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE old_state(id TEXT)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if s, err := OpenSQLite(path); err == nil {
		s.Close()
		t.Fatal("existing state without identity accepted")
	}
}
