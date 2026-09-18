package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/TSHOGX/khora/internal/model"
)

func newStoreID() model.StoreID {
	var id [16]byte
	// Failure of the operating system random source cannot yield a usable identity.
	if _, err := rand.Read(id[:]); err != nil {
		panic(fmt.Sprintf("store identity entropy: %v", err))
	}
	return model.StoreID("store_" + hex.EncodeToString(id[:]))
}

func validStoreID(id model.StoreID) bool {
	s := string(id)
	if len(s) != 38 || !strings.HasPrefix(s, "store_") {
		return false
	}
	_, err := hex.DecodeString(s[6:])
	return err == nil && strings.ToLower(s) == s
}

func initializeStore(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tables int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		if _, err := tx.ExecContext(ctx, sqliteSchema); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO store_identity(singleton,id) VALUES(1,?)", newStoreID()); err != nil {
			return err
		}
	} else {
		var id model.StoreID
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM store_identity").Scan(&count); err != nil {
			return fmt.Errorf("identity missing; rebuild state: %w", err)
		}
		if count != 1 {
			return fmt.Errorf("invalid store identity row count")
		}
		if err := tx.QueryRowContext(ctx, "SELECT id FROM store_identity WHERE singleton=1").Scan(&id); err != nil {
			return err
		}
		if !validStoreID(id) {
			return fmt.Errorf("invalid store identity")
		}
	}
	return tx.Commit()
}

func (s *SQLite) Identity(ctx context.Context) (model.StoreID, error) {
	var id model.StoreID
	if err := s.db.QueryRowContext(ctx, "SELECT id FROM store_identity WHERE singleton=1").Scan(&id); err != nil {
		return "", err
	}
	if !validStoreID(id) {
		return "", fmt.Errorf("store: invalid identity")
	}
	return id, nil
}
