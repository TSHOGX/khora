package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/TSHOGX/khora/internal/model"

	_ "modernc.org/sqlite" // pure-Go driver, CGO_ENABLED=0 (the SQLite store choice)
)

// SQLite is the persistent Store backend (the SQLite store choice): pure-Go modernc.org/sqlite,
// CGO_ENABLED=0, single embedded file (the engine vocabulary boundary local store, no external service).
//
// It DELIVERS the transactional outbox atomic-commit contract the interface promises — "state
// change + outbox append" is one BEGIN/COMMIT. This file only implements that
// promise; the reasoning behind it lives on the Store interface.
//
// Multi-key transactions are exactly why SQLite was chosen over bbolt (the SQLite store choice):
// the outbox is a first-class table and the state row + event row commit together
// without contortion.
type SQLite struct {
	db *sql.DB
}

// sqliteSchema is the ONE authoritative schema. There is no migration path: a
// breaking change edits this constant and the store file is discarded and
// rebuilt (the documentation "No compatibility burden"). IF NOT EXISTS makes OpenSQLite
// idempotent across reopens of a file this same constant created — it is NOT a
// version-tolerance mechanism, and a file whose columns disagree is a discard,
// not something to detect or adapt to.
const sqliteSchema = `
-- templates: first-class immutable object (the Template and Overlay model / health and reclamation). Existence-is-state
-- (no state column), derived refcount (no count column). parent is a
-- self-reference expressing lineage; ON DELETE RESTRICT declares "a referenced
-- parent layer cannot be deleted" (health and reclamation: references point upward). MUST be
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
	-- deleted (health and reclamation). The reference is a persistent row seen by RESTRICT from
	-- creating through destroyed, even for a paused sandbox.
	template     TEXT NOT NULL REFERENCES templates(id) ON DELETE RESTRICT,
	overlay      TEXT NOT NULL,
	state        TEXT NOT NULL,
	-- TTL Binding, ns (resource admission / health and reclamation). 0 = no such deadline. Always written
	-- explicitly by CreateSandbox; the DEFAULT states the resting value.
	idle_timeout INTEGER NOT NULL DEFAULT 0,
	retention    INTEGER NOT NULL DEFAULT 0,
	-- resource-limit Binding (resource admission): re-materialized at each placement FROM these
	-- columns, never read back from a frozen placement. 0 = unlimited. Three
	-- separate columns, never one fungible "size" — the axes and why they are not
	-- interchangeable are documented on model.Limits.
	cpu_millis   INTEGER NOT NULL DEFAULT 0, -- the runtime provider contract cgroup cpu
	memory_byte  INTEGER NOT NULL DEFAULT 0, -- the runtime provider contract host memcg (SAFETY limit)
	disk_byte    INTEGER NOT NULL DEFAULT 0, -- the Rootfs subsystem FS quota on Overlay upper+work
	-- why the unit was last found dead (health and reclamation). Committed in the SAME transaction
	-- as the crash-paused CAS (see Store.CompareAndSetState for why that is
	-- required). '' = never observed dead.
	death_cause  TEXT NOT NULL DEFAULT '',
	-- the network declaration reachability declaration (resource admission Binding), as an OPAQUE JSON document
	-- rather than normalized tables — see model.Sandbox.Network for why one column.
	--
	-- NOT NULL with no DEFAULT: an omitted network object is normalized to an explicit
	-- {deny, [], [], []} at the WRITE POINT (the network declaration), so a row with no declaration
	-- cannot be inserted. A schema default would reintroduce exactly the
	-- "a null cannot be told from a choice" ambiguity that normalization removes.
	network      TEXT NOT NULL,
	created_at   INTEGER NOT NULL, -- unix nanos
	updated_at   INTEGER NOT NULL
);
-- RETENTION (the transactional outbox, and a GitHub Issue the security model for the one part still open):
-- TrimOutbox deletes DELIVERED rows older than a configured window, so this table
-- is bounded when a deployment sets one. The DEFAULT is off, and with it off rows
-- are marked (published_at set) and never deleted, so at-rest size is unbounded —
-- a KNOWN LIMITATION of that default. Only the VALUE is deferred, not the
-- mechanism (the transactional outbox).
CREATE TABLE IF NOT EXISTS outbox (
	-- seq is the transactional outbox cursor: the monotonic total order both the publisher and
	-- the control-plane API GET /v1/events replay page on. It is the PRIMARY KEY (the rowid)
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
	-- AUTOINCREMENT keeps the high-water mark in sqlite_sequence, so it survives
	-- the table being emptied.
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	id           TEXT NOT NULL UNIQUE,   -- transition-unique event ID (the transactional outbox)
	sandbox      TEXT NOT NULL,
	-- The CHECK asserts the lifecycle state machine's CLOSED set of six kinds (model.EventKind states why it is
	-- enforced rather than documented). Kept in step with model.EventKinds, which the Go
	-- side enumerates.
	kind         TEXT NOT NULL CHECK (kind IN
	                 ('created','paused','auto-paused','crash-paused','resumed','destroyed')),
	state        TEXT NOT NULL,
	-- cause: WHY, for the kinds with more than one route to them (the transactional outbox, model.EventCause).
	-- NULL = this kind has one route, the common case.
	--
	-- NULL rather than '' for absent, so the CHECK can assert the closed set without
	-- carrying a member that means "no member". Kept in step with model.EventCauses.
	--
	-- It cannot be inferred later from sandboxes.death_cause — a different fact (why a
	-- UNIT was found gone) on a row that a destroy has already deleted.
	cause        TEXT CHECK (cause IS NULL OR cause IN ('requested','retention-expired')),
	occurred_at  INTEGER NOT NULL,       -- unix nanos; DATA, not the ordering key
	published_at INTEGER                 -- NULL = unpublished (the query predicate)
);
CREATE INDEX IF NOT EXISTS outbox_unpublished ON outbox(seq) WHERE published_at IS NULL;
-- volumes: first-class persistent object with a lifecycle DECOUPLED from every
-- sandbox (the Volume subsystem / health and reclamation). Existence-is-state, derived refcount — the same shape as
-- templates. What is deliberately NOT a column here (access mode, lease/owner, state),
-- and why each absence is a decision: model.Volume.
CREATE TABLE IF NOT EXISTS volumes (
	id         TEXT PRIMARY KEY,           -- opaque "vol_<token>"
	backend    TEXT NOT NULL,              -- which registered volume.Backend holds the bytes
	-- lineage: the volume this one was snapshotted/cloned from. NULL = not derived.
	-- ON DELETE RESTRICT is a CORRECTNESS lever, not tidiness — the measured quota-leak
	-- argument is on model.Volume.Parent.
	parent     TEXT REFERENCES volumes(id) ON DELETE RESTRICT,
	-- per-volume capacity cap, 0 = uncapped (the Volume subsystem / resource admission). THIS COLUMN IS THE AUTHORITY;
	-- a read-back cannot disambiguate it (model.Volume.CapByte / volume cap enforcement).
	cap_byte   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL             -- unix nanos
);
-- mounts: the mount table (the Volume subsystem), a first-class persistent object and structurally
-- the DOUBLING of sandboxes.template — two columns, each ON DELETE RESTRICT, which
-- together give "a volume with a live mount row cannot be deleted" AND "a sandbox
-- with a live mount row cannot be deleted" (model.Mount).
CREATE TABLE IF NOT EXISTS mounts (
	sandbox TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE RESTRICT,
	volume  TEXT NOT NULL REFERENCES volumes(id)   ON DELETE RESTRICT,
	-- guest destination, NORMALIZED by the caller (absolute, cleaned, not '/'). The
	-- UNIQUE below is a literal string comparison, so '/data', '/data/' and
	-- '/data/../data' would be three different mount points to it — an un-normalized
	-- path makes the assertion SILENTLY not apply.
	path    TEXT NOT NULL,
	-- how THIS MOUNT views the volume; per-row, never per-volume (model.AccessMode).
	--
	-- The CHECK is REQUIRED, not defensive: the lease index below has a LITERAL STRING
	-- predicate, so a mis-cased 'Exclusive' would make that index simply NOT APPLY and
	-- let two exclusive rows both insert.
	access  TEXT NOT NULL CHECK (access IN ('exclusive','shared-ro','shared-rw')),
	-- Two topology assertions (the Volume subsystem), both judged 400 rather than 409 by the API:
	--   (sandbox, path):   two volumes at one destination means the second BIND SHADOWS
	--                      the first, so one volume is invisible in the guest while
	--                      still counting in resource admission and still holding its lease. "An
	--                      exclusive lease on a volume the guest cannot see" is exactly
	--                      the silent failure this catches.
	--   (sandbox, volume): the same volume twice in one sandbox. It has no known
	--                      consumer, and forbidding it keeps the rule single rather
	--                      than forking by access mode (the lease predicate already
	--                      refuses a second row when either is exclusive). Relaxing a
	--                      validation later breaks no caller; tightening one does.
	UNIQUE (sandbox, path),
	UNIQUE (sandbox, volume)
);
-- SCHEMA-LEVEL ASSERTION, NOT THE INVARIANT'S ENFORCEMENT (the Volume subsystem). It forbids only
-- "two exclusive rows" and does NOT forbid an exclusive row coexisting with a shared
-- one — it covers half the invariant. The other half is delivered by the atomic
-- INSERT…SELECT…WHERE NOT EXISTS in CreateMounts, which expresses the full predicate.
--
-- The predicate keys on 'access', which is IMMUTABLE FOR THE ROW'S LIFETIME — do not
-- move it onto some 'detached_at'. An external implementation keyed its live-owner
-- partial index on a soft-delete TIMESTAMP instead: paths that set the column existed
-- while NOTHING in the entire codebase ever cleared it back to NULL — so one mistaken
-- write both condemned the volume permanently AND freed the uniqueness slot for a
-- second holder to take.
CREATE UNIQUE INDEX IF NOT EXISTS mounts_exclusive_lease
	ON mounts(volume) WHERE access = 'exclusive';
`

// OpenSQLite opens (creating if absent) the persistent store at path and ensures
// the schema. path may be a file path or ":memory:" (tests).
//
// Concurrency model (this is the whole correctness argument, so it is explicit).
// database/sql hands out a POOL of connections, not one — and two facts make an
// unbounded pool wrong here:
//
//  1. Correctness: every the transactional outbox transition is a write. With multiple write
//     connections, concurrent cross-sandbox transitions contend for SQLite's
//     single write lock; _txlock=immediate turns that into a fail-fast on the
//     busy-timeout rather than a mid-transaction deadlock, but it still surfaces
//     as spurious errors the mediator would have to retry. The lifecycle mediator
//     already serializes verbs PER sandbox (per-sandbox transition serialization transition lock); pinning the
//     store to ONE connection makes writes globally serialize at the store too,
//     so a transition either runs or waits — never fails spuriously. For a
//     single-node control plane (the engine vocabulary boundary) whose writes are infrequent lifecycle
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
	// _txlock=immediate: every BEGIN takes its write lock up front, preventing a
	// deferred transaction from failing during a later read-to-write upgrade.
	//
	// auto_vacuum=INCREMENTAL is what lets TrimOutbox return space to the FILESYSTEM
	// rather than only inside the file. IT MUST BE SET BEFORE THE FIRST TABLE EXISTS —
	// measured: on a database that already has tables the pragma is a silent no-op (it
	// reports back 0/none) and converting afterwards needs a full VACUUM. Hence the DSN,
	// applied on every open. Free to add only because there are no live instances to
	// migrate (the documentation "No compatibility burden").
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=auto_vacuum(incremental)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: open: %w", err)
	}
	// Single connection: deterministic write serialization (per-sandbox transition serialization/C) + a coherent
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
// back otherwise. This is the primitive that delivers the transactional outbox: a transition and
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

// insertEvent appends one outbox row inside the given tx (the transactional outbox: same commit as
// the state change). nil = publish nothing.
func insertEvent(t *sql.Tx, ev *Event) error {
	if ev == nil {
		return nil
	}
	// seq is omitted: the backend assigns it (Store's sequence contract). Any Seq
	// the caller happened to set is ignored rather than trusted — the store owns
	// this number.
	_, err := t.Exec(
		`INSERT INTO outbox(id, sandbox, kind, state, cause, occurred_at, published_at) VALUES(?,?,?,?,?,?,NULL)`,
		ev.ID, string(ev.Sandbox), ev.Kind, string(ev.State), causeArg(ev.Cause), ev.OccurredAt.UnixNano(),
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
		netJSON, jerr := marshalNetwork(sb.Network)
		if jerr != nil {
			return jerr
		}
		if _, err := t.Exec(
			`INSERT INTO sandboxes(id, template, overlay, state, idle_timeout, retention,
			   cpu_millis, memory_byte, disk_byte, death_cause, network, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			string(sb.ID), string(sb.Template), string(sb.Overlay), string(sb.State),
			int64(sb.IdleTimeout), int64(sb.Retention),
			sb.Limits.CPUMillis, sb.Limits.MemoryByte, sb.Limits.DiskByte,
			string(sb.DeathCause), netJSON,
			sb.CreatedAt.UnixNano(), sb.UpdatedAt.UnixNano(),
		); err != nil {
			// FK RESTRICT on template: the referenced Template does not exist —
			// the reference-side of health and reclamation atomicity (a concurrent DELETE won, or
			// boot seed hasn't run). create loses cleanly (the external API create-from-Template).
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
		   cpu_millis, memory_byte, disk_byte, death_cause, network, created_at, updated_at
		 FROM sandboxes WHERE id=?`,
		string(id)))
}

func (s *SQLite) ListSandboxes(ctx context.Context) ([]model.Sandbox, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, template, overlay, state, idle_timeout, retention,
		   cpu_millis, memory_byte, disk_byte, death_cause, network, created_at, updated_at
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
			return ErrConflict // failed CAS → rollback drops any event (the transactional outbox atomicity)
		}
		// state + death_cause + outbox row in ONE transaction — the atomicity
		// Store.CompareAndSetState promises (the transactional outbox / health and reclamation).
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
		// write cannot straddle a concurrent transition (per-sandbox transition serialization).
		var cur, netJSON string
		var lim model.Limits
		var idleNanos, retNanos int64
		err := t.QueryRow(
			`SELECT state, cpu_millis, memory_byte, disk_byte, idle_timeout, retention, network
			   FROM sandboxes WHERE id=?`, string(id),
		).Scan(&cur, &lim.CPUMillis, &lim.MemoryByte, &lim.DiskByte, &idleNanos, &retNanos, &netJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if model.Lifecycle(cur) != expected {
			return ErrConflict
		}
		curNet, nerr := unmarshalNetwork(netJSON)
		if nerr != nil {
			return nerr
		}
		newLim, newIdle, newRet, newNet := b.Apply(model.Sandbox{
			Limits:      lim,
			IdleTimeout: time.Duration(idleNanos),
			Retention:   time.Duration(retNanos),
			Network:     curNet,
		})
		nextNetJSON, jerr := marshalNetwork(newNet)
		if jerr != nil {
			return jerr
		}
		// One statement set, one transaction (the transactional outbox): the caps and the death-cause
		// clear that lifts health and reclamation's refusal land together or not at all. updated_at is
		// deliberately absent (see Store.UpdateBinding).
		if cause == nil {
			_, err = t.Exec(
				`UPDATE sandboxes SET cpu_millis=?, memory_byte=?, disk_byte=?, idle_timeout=?, retention=?, network=?
				   WHERE id=?`,
				newLim.CPUMillis, newLim.MemoryByte, newLim.DiskByte,
				int64(newIdle), int64(newRet), nextNetJSON, string(id))
		} else {
			_, err = t.Exec(
				`UPDATE sandboxes SET cpu_millis=?, memory_byte=?, disk_byte=?, idle_timeout=?, retention=?, network=?, death_cause=?
				   WHERE id=?`,
				newLim.CPUMillis, newLim.MemoryByte, newLim.DiskByte,
				int64(newIdle), int64(newRet), nextNetJSON, string(*cause), string(id))
		}
		return err
	})
}

func (s *SQLite) DeleteSandbox(ctx context.Context, id model.SandboxID, ev *Event) error {
	return s.tx(ctx, func(t *sql.Tx) error {
		res, err := t.Exec(`DELETE FROM sandboxes WHERE id=?`, string(id))
		if err != nil {
			// The translation point (see ErrSandboxHasMounts): without it a mounts.sandbox
			// RESTRICT violation reaches the API's default arm as a 500 instead of a 409.
			if isFKViolation(err) {
				return ErrSandboxHasMounts
			}
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

func (s *SQLite) UnpublishedEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		// Refused rather than treated as unbounded: "0 means everything" is how the
		// unbounded scan this parameter exists to prevent would come back, and it would
		// come back silently on a caller that simply forgot the argument.
		return nil, fmt.Errorf("store: UnpublishedEvents limit must be positive, got %d", limit)
	}
	// ORDER BY seq, not occurred_at: the sequence is the total order the Store
	// contract promises (equal-nanosecond timestamps have no tie-break).
	//
	// LIMIT is load-bearing rather than an optimization — on this backend specifically,
	// because the pool is pinned to ONE connection, so an unbounded scan holds it for
	// its whole length and every state transition waits behind it (the transactional outbox).
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, id, sandbox, kind, state, cause, occurred_at FROM outbox WHERE published_at IS NULL ORDER BY seq LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// TrimOutbox implements Store.TrimOutbox (the transactional outbox's retention mechanism). Read that
// contract for why each clause of this predicate is load-bearing.
func (s *SQLite) TrimOutbox(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("store: TrimOutbox limit must be positive, got %d", limit)
	}
	// The subquery is what bounds the delete: SQLite's DELETE … LIMIT is a compile-time
	// option that cannot be relied on, so the rows are selected explicitly.
	//
	// `seq < (SELECT MAX(seq) …)` is the never-trim-the-newest clause (see the contract):
	// without it a fully-caught-up outbox trims EMPTY and MIN/MAX go NULL.
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM outbox WHERE seq IN (
			SELECT seq FROM outbox
			 WHERE published_at IS NOT NULL
			   AND published_at < ?
			   AND seq < (SELECT MAX(seq) FROM outbox)
			 ORDER BY seq
			 LIMIT ?
		 )`, cutoff.UnixNano(), limit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		// Reclaim the freed pages into the filesystem (see auto_vacuum in the DSN).
		//
		// DELIBERATELY NOT PROPAGATED: the rows ARE deleted and the window HAS advanced by
		// the time this runs, so reporting the trim as failed would make the caller
		// re-attempt work already done. A failure costs only that the file stays large —
		// self-correcting on the next trim. Bounded at 64 pages so it is not the long pole.
		_, _ = s.db.ExecContext(ctx, `PRAGMA incremental_vacuum(64)`)
	}
	return int(n), nil
}

// CountUnpublished implements Store.CountUnpublished: the backlog gauge's own source,
// independent of the drain read (the transactional outbox).
func (s *SQLite) CountUnpublished(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Events implements the control-plane API replay read (Store.Events). Delivery state is not a
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
		`SELECT seq, id, sandbox, kind, state, cause, occurred_at FROM outbox
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

// causeArg maps an EventCause onto the column's two-valued domain: a real cause, or
// NULL for absent. The schema deliberately has no member meaning "no member" (see the
// outbox CHECK), so "" must not reach it as a string.
func causeArg(c model.EventCause) any {
	if c == model.CauseNotApplicable {
		return nil
	}
	return string(c)
}

// scanEvents drains an outbox row set selecting (seq, id, sandbox, kind, state, cause,
// occurred_at) in that order. Shared by both read paths so they cannot drift apart in
// how they materialize an Event.
func scanEvents(rows *sql.Rows) ([]Event, error) {
	out := make([]Event, 0)
	for rows.Next() {
		var e Event
		var sandbox, state string
		// NULL cause = this kind has one route; it scans back as the zero EventCause,
		// which is the same "not applicable" the writer passed in.
		var cause sql.NullString
		var occ int64
		if err := rows.Scan(&e.Seq, &e.ID, &sandbox, &e.Kind, &state, &cause, &occ); err != nil {
			return nil, err
		}
		e.Sandbox = model.SandboxID(sandbox)
		e.State = model.Lifecycle(state)
		e.Cause = model.EventCause(cause.String)
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
// this is the seam's translation point from a raw driver error to the health and reclamation
// contract verdict (ErrInUse on RESTRICT-delete, ErrTemplateNotFound on a
// missing-parent insert). Keeping the string match HERE, not in callers, is what
// stops the backend detail leaking into correctness (the transactional outbox).
//
// The string match is the STABLE side, not a stopgap — do not "optimize" it into
// a code comparison. Measured: with extended result codes on (the DSN turns them
// on), the two FK directions report DIFFERENT codes — an insert with a missing
// parent is 787 (SQLITE_CONSTRAINT_FOREIGNKEY) but deleting a row that an
// ON DELETE RESTRICT reference points at is 1811 (SQLITE_CONSTRAINT_TRIGGER).
// Isolated A/B confirms this is RESTRICT's own behaviour, not a driver quirk
// (the same delete under the default NO ACTION reports 787). Both carry the same
// message text, which is exactly why one string match covers every path; a
// `code == 787` rewrite would silently stop recognising every RESTRICT-delete.
// Unique violations correctly report false here — see isUniqueViolation.
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
		// the FK and we translate to ErrTemplateNotFound (health and reclamation reference-side).
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
			// ErrInUse (health and reclamation referenced-means-undeletable / the external API DELETE → 409).
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

// --- Volume CRUD (the Volume subsystem) ------------------------------------------------------

func (s *SQLite) CreateVolume(ctx context.Context, v model.Volume) error {
	return s.tx(ctx, func(t *sql.Tx) error {
		var exists int
		if err := t.QueryRow(`SELECT 1 FROM volumes WHERE id=?`, string(v.ID)).Scan(&exists); err == nil {
			return ErrConflict
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// parent is nullable: an empty VolumeID becomes SQL NULL (not derived). A
		// non-NULL parent is FK-checked here, and a missing one translates to
		// ErrVolumeNotFound (health and reclamation reference side).
		var parent any
		if v.Parent != "" {
			parent = string(v.Parent)
		}
		if _, err := t.Exec(
			`INSERT INTO volumes(id, backend, parent, cap_byte, created_at) VALUES(?,?,?,?,?)`,
			string(v.ID), v.Backend, parent, v.CapByte, v.CreatedAt.UnixNano(),
		); err != nil {
			if isFKViolation(err) {
				return ErrVolumeNotFound // the named parent does not exist
			}
			return err
		}
		return nil
	})
}

func (s *SQLite) GetVolume(ctx context.Context, id model.VolumeID) (model.Volume, error) {
	return scanVolume(s.db.QueryRowContext(ctx,
		`SELECT id, backend, parent, cap_byte, created_at FROM volumes WHERE id=?`, string(id)))
}

func (s *SQLite) ListVolumes(ctx context.Context) ([]model.Volume, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, backend, parent, cap_byte, created_at FROM volumes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Volume, 0)
	for rows.Next() {
		v, err := scanVolume(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *SQLite) SetVolumeCapacity(ctx context.Context, id model.VolumeID, capByte int64) error {
	return s.tx(ctx, func(t *sql.Tx) error {
		res, err := t.Exec(`UPDATE volumes SET cap_byte=? WHERE id=?`, capByte, string(id))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrVolumeNotFound
		}
		return nil
	})
}

func (s *SQLite) DeleteVolume(ctx context.Context, id model.VolumeID) error {
	return s.tx(ctx, func(t *sql.Tx) error {
		res, err := t.Exec(`DELETE FROM volumes WHERE id=?`, string(id))
		if err != nil {
			if isFKViolation(err) {
				// TWO RESTRICT paths land here and the caller's remedy differs, so they must be
				// told apart rather than collapsed: a mount row means "detach it", a descendant
				// means "delete the snapshot". SQLite reports the same message text for both, so
				// the distinction is drawn by asking WHICH reference exists.
				var exists int
				if qerr := t.QueryRow(`SELECT 1 FROM volumes WHERE parent=? LIMIT 1`, string(id)).Scan(&exists); qerr == nil {
					return ErrVolumeHasDescendants
				}
				return ErrVolumeInUse
			}
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// Reported, never folded into success (the API error contract): the reference-protection
			// refusals land on this same path, so folding here would make a REFUSED
			// delete indistinguishable from a successful one.
			return ErrVolumeNotFound
		}
		return nil
	})
}

// --- Mount table (the Volume subsystem) ------------------------------------------------------

// CreateMounts inserts the whole binding atomically. See Store.CreateMounts for why
// judgment and insert must be one statement rather than a pre-check plus an insert.
func (s *SQLite) CreateMounts(ctx context.Context, mounts []model.Mount) error {
	if len(mounts) == 0 {
		return nil
	}
	if err := validateBinding(mounts); err != nil {
		return err
	}
	return s.tx(ctx, func(t *sql.Tx) error {
		return insertBinding(t, mounts)
	})
}

// ReplaceMounts swaps the whole binding in ONE transaction. See Store.ReplaceMounts for
// why the atomicity is a contract promise (and why the delete cannot simply be reordered
// after the insert).
func (s *SQLite) ReplaceMounts(ctx context.Context, sandbox model.SandboxID, next []model.Mount) error {
	if err := validateReplacement(sandbox, next); err != nil {
		return err
	}
	return s.tx(ctx, func(t *sql.Tx) error {
		// DELETE FIRST, INSERT SECOND, ONE COMMIT. The order is required (a re-submitted
		// identical binding would collide with the rows it replaces); the single commit is
		// what keeps the no-rows state from ever being observable — SQLite's write
		// serialization plus the pinned single connection mean no other reader sees this
		// transaction's intermediate rows at all.
		if _, err := t.Exec(`DELETE FROM mounts WHERE sandbox=?`, string(sandbox)); err != nil {
			return err
		}
		// Any verdict here rolls the DELETE back too, so a refused replacement leaves the
		// OLD binding in place — that is consequence  and it needs no compensating code.
		return insertBinding(t, next)
	})
}

// insertBinding inserts every row of a validated binding inside an EXISTING transaction,
// enforcing the lease per row. Shared by CreateMounts and ReplaceMounts so the lease
// predicate has ONE spelling: a second copy is how the two write points would drift apart
// on the invariant the Volume subsystem calls the only new one Volume introduces.
//
// It assumes the binding was already validated (validateBinding / validateReplacement).
func insertBinding(t *sql.Tx, mounts []model.Mount) error {
	for _, mt := range mounts {
		// The full cross-row predicate, in ONE statement. It covers all four
		// combinations the partial index is blind to: inserting exclusive when ANY row
		// exists, and inserting anything when an EXCLUSIVE row exists.
		// RowsAffected()==0 ⟹ the lease is contended.
		res, err := t.Exec(
			`INSERT INTO mounts(sandbox, volume, path, access)
			 SELECT ?1, ?2, ?3, ?4
			 WHERE NOT EXISTS (
			   SELECT 1 FROM mounts m
			   WHERE m.volume = ?2
			     AND (?4 = 'exclusive' OR m.access = 'exclusive')
			 )`,
			string(mt.Sandbox), string(mt.Volume), mt.Path, string(mt.Access),
		)
		if err != nil {
			// A missing sandbox or volume is the REFERENCE side of health and reclamation atomicity (a
			// concurrent delete won). The two are distinguished by which row is absent.
			if isFKViolation(err) {
				var exists int
				if qerr := t.QueryRow(`SELECT 1 FROM volumes WHERE id=?`, string(mt.Volume)).Scan(&exists); errors.Is(qerr, sql.ErrNoRows) {
					return ErrVolumeNotFound
				}
				return ErrNotFound
			}
			// A uniqueness violation is a DIFFERENT verdict from an FK one and needs its
			// own predicate — isFKViolation correctly reports false here. Reaching this
			// means validateBinding was bypassed or is wrong, so the schema assertion is
			// doing its backstop job.
			if isUniqueViolation(err) {
				return ErrMountBindingInvalid
			}
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// ALL-OR-NOTHING: returning here rolls the transaction back, so a
			// multi-volume binding that loses one lease inserts NONE of its rows. A
			// sandbox holds one complete binding, so a partial one is not a state the
			// model has.
			return ErrMountLeaseConflict
		}
	}
	return nil
}

func (s *SQLite) ListMounts(ctx context.Context, sandbox model.SandboxID) ([]model.Mount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sandbox, volume, path, access FROM mounts WHERE sandbox=? ORDER BY path`,
		string(sandbox))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMounts(rows)
}

func (s *SQLite) ListMountsByVolume(ctx context.Context, volume model.VolumeID) ([]model.Mount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sandbox, volume, path, access FROM mounts WHERE volume=? ORDER BY sandbox, path`,
		string(volume))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMounts(rows)
}

func (s *SQLite) DeleteMounts(ctx context.Context, sandbox model.SandboxID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mounts WHERE sandbox=?`, string(sandbox))
	return err // idempotent: zero rows is success
}

func scanMounts(rows *sql.Rows) ([]model.Mount, error) {
	out := make([]model.Mount, 0)
	for rows.Next() {
		var mt model.Mount
		var sandbox, volume, access string
		if err := rows.Scan(&sandbox, &volume, &mt.Path, &access); err != nil {
			return nil, err
		}
		mt.Sandbox = model.SandboxID(sandbox)
		mt.Volume = model.VolumeID(volume)
		mt.Access = model.AccessMode(access)
		out = append(out, mt)
	}
	return out, rows.Err()
}

// isUniqueViolation reports whether err is a uniqueness/constraint failure that is
// NOT an FK violation.
//
// It is a SEPARATE predicate from isFKViolation and must stay one: that match
// correctly reports false for a uniqueness failure, so no single string match can
// cover all four the API error contract verdicts. Same reasoning as isFKViolation for why this stays a
// text match rather than a result-code comparison.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "CHECK constraint failed")
}

// validateBinding rejects a binding that CONTRADICTS ITSELF, before any row is
// written (the Volume subsystem, judged 400 by the API error contract: decidable without reading another row, and no
// retry can make it legal).
//
// It runs in the STORE rather than only at the API boundary so both backends deliver
// one verdict from one implementation — and because the schema's UNIQUE assertions
// cannot express all of it: `UNIQUE(sandbox, path)` CANNOT catch NESTING, since
// `/data` and `/data/sub` are simply different strings. Nesting needs a prefix scan,
// which is not expressible as a UNIQUE. Both layers stay: the check fires first, the
// assertion is the net for when the check is bypassed or wrong.
func validateBinding(mounts []model.Mount) error {
	seenPath := make(map[string]bool, len(mounts))
	seenVol := make(map[model.VolumeID]bool, len(mounts))
	for _, mt := range mounts {
		if !mt.Access.Valid() {
			return fmt.Errorf("%w: access %q is not one of exclusive / shared-ro / shared-rw", ErrMountBindingInvalid, mt.Access)
		}
		if mt.Path != normalizedMountPath(mt.Path) {
			return fmt.Errorf("%w: path %q is not normalized (absolute, cleaned) — the uniqueness assertions are literal string comparisons, so an un-normalized path makes them silently not apply",
				ErrMountBindingInvalid, mt.Path)
		}
		// The guest root is normalized BUT ILLEGAL (the Volume subsystem: absolute, cleaned, not "/"), and
		// this is a SEPARATE check rather than a stricter normalizer because "/" is a FIXED
		// POINT of path.Clean: "//", "/.", "/data/.." and " / " all normalize TO "/" and are
		// caught above, so only the bare "/" reaches here. Folding the two would make
		// normalizedMountPath return a value that is not normalized, destroying the ONE
		// definition it exists to share with the mediator. No uniqueness assertion can
		// express "does not shadow everything", so this check is the only place it is caught.
		if mt.Path == "/" {
			return fmt.Errorf("%w: path %q is the guest root — a bind there shadows the ENTIRE sandbox rootfs, leaving the overlay mounted but unreachable",
				ErrMountBindingInvalid, mt.Path)
		}
		if seenPath[mt.Path] {
			return fmt.Errorf("%w: destination %q appears twice — the second bind would SHADOW the first, leaving one volume invisible in the guest while it still counts in resource admission and still holds its lease",
				ErrMountBindingInvalid, mt.Path)
		}
		if seenVol[mt.Volume] {
			return fmt.Errorf("%w: volume %s appears twice in one sandbox", ErrMountBindingInvalid, mt.Volume)
		}
		seenPath[mt.Path] = true
		seenVol[mt.Volume] = true
	}
	// Nesting: volumes must not mount inside one another's subtree (the Volume subsystem), or the
	// ownership of the mount-point placeholder in a Promote capture becomes ambiguous.
	// A prefix scan, because UNIQUE cannot express it.
	for i, a := range mounts {
		for j, b := range mounts {
			if i == j {
				continue
			}
			if strings.HasPrefix(b.Path, a.Path+"/") {
				return fmt.Errorf("%w: %q is nested inside %q — volumes must not mount within one another's subtree",
					ErrMountBindingInvalid, b.Path, a.Path)
			}
		}
	}
	return nil
}

// validateReplacement is validateBinding plus the one assertion a replacement adds: every
// row must name the sandbox being rebound.
//
// It is a 400 rather than a silent filter because the alternative is genuinely
// destructive: ReplaceMounts deletes `sandbox`'s rows, so a row carrying a DIFFERENT
// sandbox would detach one sandbox and attach another in a single commit the caller
// believes touched only one. Both backends run this, so it is contract, not a SQLite check.
func validateReplacement(sandbox model.SandboxID, next []model.Mount) error {
	for _, mt := range next {
		if mt.Sandbox != sandbox {
			return fmt.Errorf("%w: row for sandbox %s appears in a replacement of %s's binding — a replacement rewrites exactly one sandbox's rows",
				ErrMountBindingInvalid, mt.Sandbox, sandbox)
		}
	}
	return validateBinding(next)
}

// normalizedMountPath is the ONE definition of a normalized mount destination,
// shared by the validation above and by the mediator that prepares bindings, so the
// two cannot disagree about what "normalized" means.
//
// It answers "what is the canonical spelling of this path", NOT "is this path a legal
// mount destination". "/" is a fixed point here — canonical, and still refused by
// validateBinding above, which is the only place legality is decided.
func normalizedMountPath(p string) string {
	c := path.Clean("/" + strings.TrimSpace(p))
	return c
}

// NormalizeMountPath exposes that definition to the mediator (the Volume subsystem requires paths to
// be normalized BEFORE insert). Exported from the store because the store is where
// the uniqueness assertions live, so the normalization and the assertions it protects
// stay in one place.
func NormalizeMountPath(p string) string { return normalizedMountPath(p) }

func scanVolume(r rowScanner) (model.Volume, error) {
	var v model.Volume
	var id, backend string
	var parent sql.NullString
	var capByte, created int64
	err := r.Scan(&id, &backend, &parent, &capByte, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Volume{}, ErrVolumeNotFound
	}
	if err != nil {
		return model.Volume{}, err
	}
	v.ID = model.VolumeID(id)
	v.Backend = backend
	v.Parent = model.VolumeID(parent.String) // "" when NULL
	v.CapByte = capByte
	v.CreatedAt = time.Unix(0, created)
	return v, nil
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
	var id, tmpl, ov, state, deathCause, netJSON string
	var idleTimeout, retention, cpuMillis, memoryByte, diskByte, created, updated int64
	err := r.Scan(&id, &tmpl, &ov, &state, &idleTimeout, &retention,
		&cpuMillis, &memoryByte, &diskByte, &deathCause, &netJSON, &created, &updated)
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
	net, nerr := unmarshalNetwork(netJSON)
	if nerr != nil {
		return model.Sandbox{}, nerr
	}
	sb.Network = net
	sb.CreatedAt = time.Unix(0, created)
	sb.UpdatedAt = time.Unix(0, updated)
	return sb, nil
}

// compile-time: SQLite satisfies Store.
var _ Store = (*SQLite)(nil)
