package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"engine/internal/model"

	_ "modernc.org/sqlite" // pure-Go driver, CGO_ENABLED=0 (ADR-0004)
)

// SQLite is the persistent Store backend (ADR-0004): pure-Go modernc.org/sqlite,
// CGO_ENABLED=0, single embedded file (§1.1 local store, no external service).
//
// It DELIVERS the §4.4-C atomic-commit contract the interface promises — "state
// change + outbox append" is one BEGIN/COMMIT — the same contract Memory
// delivers with a mutex. The publish algorithm above never learns which backend
// it runs on (§4.4-C 铁律); this file only implements the promise.
//
// Multi-key transactions are exactly why SQLite was chosen over bbolt (ADR-0004):
// the outbox is a first-class table and the state row + event row commit together
// without contortion.
type SQLite struct {
	db *sql.DB
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS sandboxes (
	id           TEXT PRIMARY KEY,
	template     TEXT NOT NULL,
	overlay      TEXT NOT NULL,
	state        TEXT NOT NULL,
	guest_user   TEXT NOT NULL,
	idle_timeout INTEGER NOT NULL DEFAULT 0, -- TTL Binding, ns (§3.4 / §4.3)
	retention    INTEGER NOT NULL DEFAULT 0, -- TTL Binding, ns (§3.4 / §4.3)
	created_at   INTEGER NOT NULL, -- unix nanos
	updated_at   INTEGER NOT NULL
);
-- KNOWN LIMITATION (open-decisions §2 §4.1/§4.4-C row): published rows are
-- marked (published_at set), NEVER deleted, so this table grows monotonically.
-- The partial index below keeps the publisher's unpublished-scan from
-- degrading, but at-rest size is unbounded. Pruning (retention/replay window)
-- is deferred to the event-stream replay-window decision (open-decisions §2),
-- by design — not an oversight.
CREATE TABLE IF NOT EXISTS outbox (
	id           TEXT PRIMARY KEY,       -- transition-unique event ID (§4.4-C)
	sandbox      TEXT NOT NULL,
	kind         TEXT NOT NULL,
	state        TEXT NOT NULL,
	occurred_at  INTEGER NOT NULL,       -- unix nanos, publisher orders by this
	published_at INTEGER                 -- NULL = unpublished (the query predicate)
);
CREATE INDEX IF NOT EXISTS outbox_unpublished ON outbox(occurred_at) WHERE published_at IS NULL;
`

// OpenSQLite opens (creating if absent) the persistent store at path and ensures
// the schema. path may be a file path or ":memory:" (tests).
//
// Concurrency model (this is the whole correctness argument, so it is explicit).
// database/sql hands out a POOL of connections, not one — and two facts make an
// unbounded pool wrong here:
//
//  1. Correctness: every §4.4-C transition is a write. With multiple write
//     connections, concurrent cross-sandbox transitions contend for SQLite's
//     single write lock; _txlock=immediate turns that into a fail-fast on the
//     busy-timeout rather than a mid-transaction deadlock, but it still surfaces
//     as spurious errors the mediator would have to retry. The §4 mediator
//     already serializes verbs PER sandbox (§4.4-B transition lock); pinning the
//     store to ONE connection makes writes globally serialize at the store too,
//     so a transition either runs or waits — never fails spuriously. For a
//     single-node control plane (§1.1) whose writes are infrequent lifecycle
//     events, total serialization is the right trade: determinism over
//     write-concurrency the workload does not need.
//  2. :memory: coherence: each modernc.org/sqlite connection to ":memory:" gets
//     its OWN private database. A pool would therefore split state across
//     connections — reconcile/publisher reads could miss a just-committed write.
//     One connection means one in-memory database, shared across every call.
//
// So we cap the pool at a single connection. WAL + busy_timeout remain for the
// file case (crash-safe commits, and a short wait rather than an instant error
// if an external process ever holds the file lock).
func OpenSQLite(path string) (*SQLite, error) {
	// _txlock=immediate: every BEGIN takes its write lock up front, so a
	// read-modify-write CAS transaction can't deadlock against another writer
	// mid-transaction. Correct for the §4.4-C commit path where every transition
	// is a write.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: open: %w", err)
	}
	// Single connection: deterministic write serialization (§4.4-B/C) + a coherent
	// single in-memory database for ":memory:". Safe from self-deadlock because no
	// code path holds an open *sql.Tx while trying to acquire a second connection —
	// each Store method takes the connection once and returns it.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0) // never expire the pinned connection (keeps :memory: alive)
	if _, err := db.ExecContext(context.Background(), sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store/sqlite: schema: %w", err)
	}
	// No schema migration: there are no live instances, so the schema is whatever
	// CREATE TABLE declares today (CLAUDE.md "No compatibility burden"). A column
	// growth edits sqliteSchema in place; the store.db is discarded and rebuilt.
	return &SQLite{db: db}, nil
}

// Close releases the database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// tx runs fn inside a single transaction, committing on nil error and rolling
// back otherwise. This is the primitive that delivers §4.4-C: a transition and
// its outbox append run inside ONE fn, so a crash between them is impossible —
// the commit is all-or-nothing.
func (s *SQLite) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Commit()
}

// insertEvent appends one outbox row inside the given tx (§4.4-C: same commit as
// the state change). nil = publish nothing.
func insertEvent(t *sql.Tx, ev *Event) error {
	if ev == nil {
		return nil
	}
	_, err := t.Exec(
		`INSERT INTO outbox(id, sandbox, kind, state, occurred_at, published_at) VALUES(?,?,?,?,?,NULL)`,
		ev.ID, string(ev.Sandbox), ev.Kind, string(ev.State), ev.OccurredAt.UnixNano(),
	)
	return err
}

func (s *SQLite) CreateSandbox(ctx context.Context, sb model.Sandbox, ev *Event) error {
	return s.tx(ctx, func(t *sql.Tx) error {
		// detect duplicate → ErrConflict (matches Memory + CAS semantics).
		var exists int
		if err := t.QueryRow(`SELECT 1 FROM sandboxes WHERE id=?`, string(sb.ID)).Scan(&exists); err == nil {
			return ErrConflict
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := t.Exec(
			`INSERT INTO sandboxes(id, template, overlay, state, guest_user, idle_timeout, retention, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			string(sb.ID), string(sb.Template), string(sb.Overlay), string(sb.State),
			sb.GuestUser, int64(sb.IdleTimeout), int64(sb.Retention),
			sb.CreatedAt.UnixNano(), sb.UpdatedAt.UnixNano(),
		); err != nil {
			return err
		}
		return insertEvent(t, ev)
	})
}

func (s *SQLite) GetSandbox(ctx context.Context, id model.SandboxID) (model.Sandbox, error) {
	return scanSandbox(s.db.QueryRowContext(ctx,
		`SELECT id, template, overlay, state, guest_user, idle_timeout, retention, created_at, updated_at FROM sandboxes WHERE id=?`,
		string(id)))
}

func (s *SQLite) ListSandboxes(ctx context.Context) ([]model.Sandbox, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, template, overlay, state, guest_user, idle_timeout, retention, created_at, updated_at FROM sandboxes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Sandbox, 0)
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func (s *SQLite) CompareAndSetState(ctx context.Context, id model.SandboxID, expected, next model.Lifecycle, ev *Event) error {
	return s.tx(ctx, func(t *sql.Tx) error {
		var cur string
		err := t.QueryRow(`SELECT state FROM sandboxes WHERE id=?`, string(id)).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if model.Lifecycle(cur) != expected {
			return ErrConflict // failed CAS → rollback drops any event (§4.4-C atomicity)
		}
		if _, err := t.Exec(`UPDATE sandboxes SET state=?, updated_at=? WHERE id=?`,
			string(next), time.Now().UnixNano(), string(id)); err != nil {
			return err
		}
		return insertEvent(t, ev)
	})
}

func (s *SQLite) DeleteSandbox(ctx context.Context, id model.SandboxID, ev *Event) error {
	return s.tx(ctx, func(t *sql.Tx) error {
		res, err := t.Exec(`DELETE FROM sandboxes WHERE id=?`, string(id))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		// the outbox row is a separate table — it OUTLIVES the deleted record.
		return insertEvent(t, ev)
	})
}

func (s *SQLite) UnpublishedEvents(ctx context.Context) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, sandbox, kind, state, occurred_at FROM outbox WHERE published_at IS NULL ORDER BY occurred_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Event, 0)
	for rows.Next() {
		var e Event
		var sandbox, state string
		var occ int64
		if err := rows.Scan(&e.ID, &sandbox, &e.Kind, &state, &occ); err != nil {
			return nil, err
		}
		e.Sandbox = model.SandboxID(sandbox)
		e.State = model.Lifecycle(state)
		e.OccurredAt = time.Unix(0, occ)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLite) MarkPublished(ctx context.Context, eventID string) error {
	// idempotent: UPDATE of an unknown or already-published row affects 0/1 rows,
	// both fine. Only set published_at once (WHERE published_at IS NULL) so the
	// timestamp reflects first delivery.
	_, err := s.db.ExecContext(ctx,
		`UPDATE outbox SET published_at=? WHERE id=? AND published_at IS NULL`,
		time.Now().UnixNano(), eventID)
	return err
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSandbox(r rowScanner) (model.Sandbox, error) {
	var sb model.Sandbox
	var id, tmpl, ov, state string
	var idleTimeout, retention, created, updated int64
	err := r.Scan(&id, &tmpl, &ov, &state, &sb.GuestUser, &idleTimeout, &retention, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Sandbox{}, ErrNotFound
	}
	if err != nil {
		return model.Sandbox{}, err
	}
	sb.ID = model.SandboxID(id)
	sb.Template = model.TemplateID(tmpl)
	sb.Overlay = model.OverlayID(ov)
	sb.State = model.Lifecycle(state)
	sb.IdleTimeout = time.Duration(idleTimeout)
	sb.Retention = time.Duration(retention)
	sb.CreatedAt = time.Unix(0, created)
	sb.UpdatedAt = time.Unix(0, updated)
	return sb, nil
}

// compile-time: SQLite satisfies Store.
var _ Store = (*SQLite)(nil)
