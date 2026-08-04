package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"engine/internal/model"

	_ "modernc.org/sqlite" // pure-Go driver, CGO_ENABLED=0 (ADR-0004)
)

// SQLite is the persistent Store backend (ADR-0004): pure-Go modernc.org/sqlite,
// CGO_ENABLED=0, single embedded file (§1.1 local store, no external service).
//
// It DELIVERS the §4.4-C atomic-commit contract the interface promises — "state
// change + outbox append" is one BEGIN/COMMIT. This file only implements that
// promise; the reasoning behind it lives on the Store interface.
//
// Multi-key transactions are exactly why SQLite was chosen over bbolt (ADR-0004):
// the outbox is a first-class table and the state row + event row commit together
// without contortion.
type SQLite struct {
	db *sql.DB
}

// sqliteSchema is the ONE authoritative schema. There is no migration path: a
// breaking change edits this constant and the store file is discarded and
// rebuilt (CLAUDE.md "No compatibility burden"). IF NOT EXISTS makes OpenSQLite
// idempotent across reopens of a file this same constant created — it is NOT a
// version-tolerance mechanism, and a file whose columns disagree is a discard,
// not something to detect or adapt to.
const sqliteSchema = `
-- templates: first-class immutable object (§3.1 / §4.3). Existence-is-state
-- (no state column), derived refcount (no count column). parent is a
-- self-reference expressing lineage; ON DELETE RESTRICT declares "a referenced
-- parent layer cannot be deleted" (§4.3: references point upward). MUST be
-- created before the sandboxes table so the sandboxes.template FK below resolves.
CREATE TABLE IF NOT EXISTS templates (
	id             TEXT PRIMARY KEY,          -- opaque "tmpl_<token>" / builtin = static dir name
	parent         TEXT REFERENCES templates(id) ON DELETE RESTRICT, -- lineage; NULL = base layer
	builtin        INTEGER NOT NULL DEFAULT 0, -- 1 = platform-preseeded (boot seed), 0 = Promote-built
	created_at     INTEGER NOT NULL            -- unix nanos
);
CREATE TABLE IF NOT EXISTS sandboxes (
	id           TEXT PRIMARY KEY,
	-- template FK RESTRICT: a Template referenced by any sandbox cannot be
	-- deleted (§4.3). The reference is a persistent row seen by RESTRICT from
	-- creating through destroyed, even for a paused sandbox.
	template     TEXT NOT NULL REFERENCES templates(id) ON DELETE RESTRICT,
	overlay      TEXT NOT NULL,
	state        TEXT NOT NULL,
	-- TTL Binding, ns (§3.4 / §4.3). 0 = no such deadline. Always written
	-- explicitly by CreateSandbox; the DEFAULT states the resting value.
	idle_timeout INTEGER NOT NULL DEFAULT 0,
	retention    INTEGER NOT NULL DEFAULT 0,
	-- resource-limit Binding (§3.4): re-materialized at each placement FROM these
	-- columns, never read back from a frozen placement. 0 = unlimited. Three
	-- separate columns, never one fungible "size" — the axes and why they are not
	-- interchangeable are documented on model.Limits.
	cpu_millis   INTEGER NOT NULL DEFAULT 0, -- §5.A cgroup cpu
	memory_byte  INTEGER NOT NULL DEFAULT 0, -- §5.A host memcg (SAFETY limit)
	disk_byte    INTEGER NOT NULL DEFAULT 0, -- §5.B FS quota on Overlay upper+work
	-- why the unit was last found dead (§4.3). Committed in the SAME transaction
	-- as the crash-paused CAS (see Store.CompareAndSetState for why that is
	-- required). '' = never observed dead.
	death_cause  TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL, -- unix nanos
	updated_at   INTEGER NOT NULL
);
-- KNOWN LIMITATION (open-decisions §2, the §4.1/§4.4-C event-stream row):
-- published rows are marked (published_at set), NEVER deleted, so this table
-- grows monotonically. The partial index below keeps the publisher's
-- unpublished-scan from degrading, but at-rest size is unbounded. Pruning
-- (retention/replay window) is deferred to the event-stream replay-window
-- decision, by design — not an oversight.
CREATE TABLE IF NOT EXISTS outbox (
	-- seq is the §4.4-C cursor: the monotonic total order both the publisher and
	-- the §6.1 GET /v1/events replay page on. It is the PRIMARY KEY (the rowid)
	-- and the event ID is a UNIQUE column beside it — the two swapped roles
	-- deliberately, because SQLite gives an INTEGER PRIMARY KEY the persistent
	-- monotonic counter that a TEXT key cannot have.
	--
	-- Why occurred_at cannot be the cursor: unix nanos are not a total order —
	-- two transitions in the same nanosecond have no tie-break, and the wall
	-- clock can be stepped backwards by NTP, which would make an already-served
	-- cursor position ambiguous.
	--
	-- Why AUTOINCREMENT and not a bare rowid: a bare rowid is max(rowid)+1, so
	-- emptying the table RESTARTS it at 1 (measured) and a subscriber's cursor
	-- would silently point past freshly-issued sequences — the same
	-- sequence-rollback class of defect audited in the reference implementation.
	-- Today we never delete rows, so max(rowid) is monotonic anyway and this is
	-- DEFENCE IN DEPTH rather than strictly required: it makes the invariant a
	-- property of the schema instead of a "we promise never to delete rows"
	-- policy living in a comment. AUTOINCREMENT keeps the high-water mark in
	-- sqlite_sequence, so it survives the table being emptied.
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	id           TEXT NOT NULL UNIQUE,   -- transition-unique event ID (§4.4-C)
	sandbox      TEXT NOT NULL,
	kind         TEXT NOT NULL,
	state        TEXT NOT NULL,
	occurred_at  INTEGER NOT NULL,       -- unix nanos; DATA, not the ordering key
	published_at INTEGER                 -- NULL = unpublished (the query predicate)
);
CREATE INDEX IF NOT EXISTS outbox_unpublished ON outbox(seq) WHERE published_at IS NULL;
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
	// seq is omitted: the backend assigns it (Store's sequence contract). Any Seq
	// the caller happened to set is ignored rather than trusted — the store owns
	// this number.
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
			`INSERT INTO sandboxes(id, template, overlay, state, idle_timeout, retention,
			   cpu_millis, memory_byte, disk_byte, death_cause, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			string(sb.ID), string(sb.Template), string(sb.Overlay), string(sb.State),
			int64(sb.IdleTimeout), int64(sb.Retention),
			sb.Limits.CPUMillis, sb.Limits.MemoryByte, sb.Limits.DiskByte,
			string(sb.DeathCause),
			sb.CreatedAt.UnixNano(), sb.UpdatedAt.UnixNano(),
		); err != nil {
			// FK RESTRICT on template: the referenced Template does not exist —
			// the reference-side of §4.3 atomicity (a concurrent DELETE won, or
			// boot seed hasn't run). create loses cleanly (§6 create-from-Template).
			if isFKViolation(err) {
				return ErrTemplateNotFound
			}
			return err
		}
		return insertEvent(t, ev)
	})
}

func (s *SQLite) GetSandbox(ctx context.Context, id model.SandboxID) (model.Sandbox, error) {
	return scanSandbox(s.db.QueryRowContext(ctx,
		`SELECT id, template, overlay, state, idle_timeout, retention,
		   cpu_millis, memory_byte, disk_byte, death_cause, created_at, updated_at
		 FROM sandboxes WHERE id=?`,
		string(id)))
}

func (s *SQLite) ListSandboxes(ctx context.Context) ([]model.Sandbox, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, template, overlay, state, idle_timeout, retention,
		   cpu_millis, memory_byte, disk_byte, death_cause, created_at, updated_at
		 FROM sandboxes ORDER BY created_at`)
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

func (s *SQLite) CompareAndSetState(ctx context.Context, id model.SandboxID, expected, next model.Lifecycle, ev *Event, cause *model.DeathCause) error {
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
		// state + death_cause + outbox row in ONE transaction — the atomicity
		// Store.CompareAndSetState promises (§4.4-C / §4.3).
		if cause == nil {
			if _, err := t.Exec(`UPDATE sandboxes SET state=?, updated_at=? WHERE id=?`,
				string(next), time.Now().UnixNano(), string(id)); err != nil {
				return err
			}
		} else {
			if _, err := t.Exec(`UPDATE sandboxes SET state=?, death_cause=?, updated_at=? WHERE id=?`,
				string(next), string(*cause), time.Now().UnixNano(), string(id)); err != nil {
				return err
			}
		}
		return insertEvent(t, ev)
	})
}

func (s *SQLite) UpdateBinding(ctx context.Context, id model.SandboxID, expected model.Lifecycle, b Binding, cause *model.DeathCause) error {
	return s.tx(ctx, func(t *sql.Tx) error {
		// Read the current record inside the transaction so the state check and the
		// write cannot straddle a concurrent transition (§4.4-B).
		var cur string
		var lim model.Limits
		var idleNanos, retNanos int64
		err := t.QueryRow(
			`SELECT state, cpu_millis, memory_byte, disk_byte, idle_timeout, retention
			   FROM sandboxes WHERE id=?`, string(id),
		).Scan(&cur, &lim.CPUMillis, &lim.MemoryByte, &lim.DiskByte, &idleNanos, &retNanos)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if model.Lifecycle(cur) != expected {
			return ErrConflict
		}
		newLim, newIdle, newRet := b.Apply(model.Sandbox{
			Limits:      lim,
			IdleTimeout: time.Duration(idleNanos),
			Retention:   time.Duration(retNanos),
		})
		// One statement set, one transaction (§4.4-C): the caps and the death-cause
		// clear that lifts §4.3's refusal land together or not at all. updated_at is
		// deliberately absent (see Store.UpdateBinding).
		if cause == nil {
			_, err = t.Exec(
				`UPDATE sandboxes SET cpu_millis=?, memory_byte=?, disk_byte=?, idle_timeout=?, retention=?
				   WHERE id=?`,
				newLim.CPUMillis, newLim.MemoryByte, newLim.DiskByte,
				int64(newIdle), int64(newRet), string(id))
		} else {
			_, err = t.Exec(
				`UPDATE sandboxes SET cpu_millis=?, memory_byte=?, disk_byte=?, idle_timeout=?, retention=?, death_cause=?
				   WHERE id=?`,
				newLim.CPUMillis, newLim.MemoryByte, newLim.DiskByte,
				int64(newIdle), int64(newRet), string(*cause), string(id))
		}
		return err
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
	// ORDER BY seq, not occurred_at: the sequence is the total order the Store
	// contract promises (equal-nanosecond timestamps have no tie-break).
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, id, sandbox, kind, state, occurred_at FROM outbox WHERE published_at IS NULL ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// Events implements the §6.1 replay read (Store.Events). Delivery state is not a
// predicate here — replay is over the whole retained window.
func (s *SQLite) Events(ctx context.Context, after int64, limit int) ([]Event, int64, int64, error) {
	// Window bounds first, from the SAME connection (the pool is pinned to one),
	// so the bounds and the page cannot straddle an unrelated writer.
	var earliest, latest sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(seq), MAX(seq) FROM outbox`).Scan(&earliest, &latest); err != nil {
		return nil, 0, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, id, sandbox, kind, state, occurred_at FROM outbox
		   WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	evs, err := scanEvents(rows)
	if err != nil {
		return nil, 0, 0, err
	}
	return evs, earliest.Int64, latest.Int64, nil
}

// scanEvents drains an outbox row set selecting (seq, id, sandbox, kind, state,
// occurred_at) in that order. Shared by both read paths so they cannot drift
// apart in how they materialize an Event.
func scanEvents(rows *sql.Rows) ([]Event, error) {
	out := make([]Event, 0)
	for rows.Next() {
		var e Event
		var sandbox, state string
		var occ int64
		if err := rows.Scan(&e.Seq, &e.ID, &sandbox, &e.Kind, &state, &occ); err != nil {
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

// isFKViolation reports whether err is a SQLite foreign-key constraint failure.
// modernc.org/sqlite surfaces it in the error text (no typed sentinel exposed);
// this is the seam's translation point from a raw driver error to the §4.3
// contract verdict (ErrInUse on RESTRICT-delete, ErrTemplateNotFound on a
// missing-parent insert). Keeping the string match HERE, not in callers, is what
// stops the backend detail leaking into correctness (§4.4-C).
func isFKViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

func (s *SQLite) CreateTemplate(ctx context.Context, tm model.Template) error {
	return s.tx(ctx, func(t *sql.Tx) error {
		var exists int
		if err := t.QueryRow(`SELECT 1 FROM templates WHERE id=?`, string(tm.ID)).Scan(&exists); err == nil {
			return ErrConflict
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// parent is nullable: empty TemplateID → SQL NULL (a base layer). A
		// non-NULL parent is FK-checked here — a missing parent → the insert hits
		// the FK and we translate to ErrTemplateNotFound (§4.3 reference-side).
		var parent any
		if tm.Parent != "" {
			parent = string(tm.Parent)
		}
		builtin := 0
		if tm.Builtin {
			builtin = 1
		}
		if _, err := t.Exec(
			`INSERT INTO templates(id, parent, builtin, created_at) VALUES(?,?,?,?)`,
			string(tm.ID), parent, builtin, tm.CreatedAt.UnixNano(),
		); err != nil {
			if isFKViolation(err) {
				return ErrTemplateNotFound // missing parent layer
			}
			return err
		}
		return nil
	})
}

func (s *SQLite) GetTemplate(ctx context.Context, id model.TemplateID) (model.Template, error) {
	return scanTemplate(s.db.QueryRowContext(ctx,
		`SELECT id, parent, builtin, created_at FROM templates WHERE id=?`,
		string(id)))
}

func (s *SQLite) ListTemplates(ctx context.Context) ([]model.Template, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, parent, builtin, created_at FROM templates ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Template, 0)
	for rows.Next() {
		tm, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tm)
	}
	return out, rows.Err()
}

func (s *SQLite) DeleteTemplate(ctx context.Context, id model.TemplateID) error {
	return s.tx(ctx, func(t *sql.Tx) error {
		res, err := t.Exec(`DELETE FROM templates WHERE id=?`, string(id))
		if err != nil {
			// FK RESTRICT: a sandbox or child Template still references it →
			// ErrInUse (§4.3 referenced-means-undeletable / §6 DELETE → 409).
			if isFKViolation(err) {
				return ErrInUse
			}
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrTemplateNotFound
		}
		return nil
	})
}

func scanTemplate(r rowScanner) (model.Template, error) {
	var tm model.Template
	var id string
	var parent sql.NullString
	var builtin int
	var created int64
	err := r.Scan(&id, &parent, &builtin, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Template{}, ErrTemplateNotFound
	}
	if err != nil {
		return model.Template{}, err
	}
	tm.ID = model.TemplateID(id)
	tm.Parent = model.TemplateID(parent.String) // "" when NULL
	tm.Builtin = builtin != 0
	tm.CreatedAt = time.Unix(0, created)
	return tm, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSandbox(r rowScanner) (model.Sandbox, error) {
	var sb model.Sandbox
	var id, tmpl, ov, state, deathCause string
	var idleTimeout, retention, cpuMillis, memoryByte, diskByte, created, updated int64
	err := r.Scan(&id, &tmpl, &ov, &state, &idleTimeout, &retention,
		&cpuMillis, &memoryByte, &diskByte, &deathCause, &created, &updated)
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
	sb.Limits = model.Limits{CPUMillis: cpuMillis, MemoryByte: memoryByte, DiskByte: diskByte}
	sb.DeathCause = model.DeathCause(deathCause)
	sb.CreatedAt = time.Unix(0, created)
	sb.UpdatedAt = time.Unix(0, updated)
	return sb, nil
}

// compile-time: SQLite satisfies Store.
var _ Store = (*SQLite)(nil)
