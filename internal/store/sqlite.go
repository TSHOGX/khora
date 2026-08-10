package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
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
	-- the §5.D reachability declaration (§3.4 Binding), as an OPAQUE JSON document.
	-- Every placement regenerates its rule set from THIS column, so an edit made
	-- while paused takes effect at the next resume and §4.4-A can rebuild a
	-- crash-surviving rule set from it.
	--
	-- One column rather than normalized tables, because of what the field IS: a
	-- whole-value Binding with exactly two write points, both of which replace or
	-- append to the WHOLE document (§6.1 PATCH swaps whole binding for whole
	-- binding; POST .../publish appends one item). Nothing queries INTO it -- the
	-- generator receives the whole declaration — so columns would buy no query and
	-- cost a table per repeated field. Contrast the mounts table, normalized precisely
	-- because the exclusive lease is a CROSS-ROW predicate the store arbitrates.
	--
	-- NOT NULL with no DEFAULT: an omitted network object is normalized to an explicit
	-- {deny, [], [], []} at the WRITE POINT (§5.D), so a row with no declaration
	-- cannot be inserted. A schema default would reintroduce exactly the
	-- "a null cannot be told from a choice" ambiguity that normalization removes.
	network      TEXT NOT NULL,
	created_at   INTEGER NOT NULL, -- unix nanos
	updated_at   INTEGER NOT NULL
);
-- KNOWN LIMITATION (open-decisions §2, "§4.4-C 事件流可重放窗口大小"):
-- published rows are marked (published_at set), NEVER deleted, so this table
-- grows monotonically. The partial index below keeps the publisher's
-- unpublished-scan from degrading, but at-rest size is unbounded. This is
-- DEFERRED, not answered: the trim window has to be set from the reconnect
-- window promised to external subscribers, and there are none yet, so there is
-- no arguable value to pick. It remains a limitation and did NOT become a virtue
-- by being decided upon.
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
	-- The CHECK asserts §4.1's CLOSED set of six kinds, for the reason mounts.access
	-- carries one: a value the schema cannot write is a value no reader has to handle.
	-- §4.1 states the closure and nothing enforced it, so a typo'd 'crash_paused' would
	-- have compiled, persisted, and reached subscribers as a seventh kind. Kept in step
	-- with model.EventKinds, which the Go side enumerates.
	--
	-- There is deliberately no 'evict': eviction reaches destroyLocked and emits
	-- 'destroyed'. The three paused kinds differ in WHY (§4.3).
	kind         TEXT NOT NULL CHECK (kind IN
	                 ('created','paused','auto-paused','crash-paused','resumed','destroyed')),
	state        TEXT NOT NULL,
	occurred_at  INTEGER NOT NULL,       -- unix nanos; DATA, not the ordering key
	published_at INTEGER                 -- NULL = unpublished (the query predicate)
);
CREATE INDEX IF NOT EXISTS outbox_unpublished ON outbox(seq) WHERE published_at IS NULL;
-- volumes: first-class persistent object with a lifecycle DECOUPLED from every
-- sandbox (§5.C / §4.3). Existence-is-state (no state column), derived refcount
-- (no count column) — the same shape as templates.
--
-- What is deliberately NOT a column here, because each absence is a decision
-- (see model.Volume): no access mode (it is a property of the MOUNT ROW — one
-- volume can be shared-rw to one sandbox and shared-ro to another at the same
-- time, so a per-volume column could not carry it honestly), no lease or owner
-- (an exclusive lease IS the existence of an 'exclusive' mount row, not an
-- object), and no state.
CREATE TABLE IF NOT EXISTS volumes (
	id         TEXT PRIMARY KEY,           -- opaque "vol_<token>"
	backend    TEXT NOT NULL,              -- which registered volume.Backend holds the bytes
	-- lineage: the volume this one was snapshotted/cloned from. NULL = not derived.
	-- ON DELETE RESTRICT is a CORRECTNESS lever, not tidiness: a quota row leaks exactly
	-- when a unit owning extents is deleted while a live sharer references them, and
	-- measured, deleting sharers BEFORE the source leaks nothing. RESTRICT makes that
	-- order the only reachable one, so the leak is PREVENTED rather than reclaimed.
	parent     TEXT REFERENCES volumes(id) ON DELETE RESTRICT,
	-- per-volume capacity cap, 0 = uncapped (§5.C / §3.4). THIS COLUMN IS THE
	-- AUTHORITY: the substrate maps "no cap" and 0 onto each other in both
	-- directions, so a read-back cannot distinguish "never capped" from
	-- "deliberately uncapped" and no after-the-fact reconciliation against storage
	-- is possible (volume.md D7.3).
	cap_byte   INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL             -- unix nanos
);
-- mounts: the mount table (§5.C), a first-class persistent object and structurally
-- the DOUBLING of sandboxes.template — two columns, each ON DELETE RESTRICT, which
-- together give "a volume with a live mount row cannot be deleted" AND "a sandbox
-- with a live mount row cannot be deleted". No state column, no count column:
-- whether a volume is exclusively held is DERIVED from these rows (§4.3).
CREATE TABLE IF NOT EXISTS mounts (
	sandbox TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE RESTRICT,
	volume  TEXT NOT NULL REFERENCES volumes(id)   ON DELETE RESTRICT,
	-- guest destination, NORMALIZED by the caller (absolute, cleaned, not '/'). The
	-- UNIQUE below is a literal string comparison, so '/data', '/data/' and
	-- '/data/../data' would be three different mount points to it — an un-normalized
	-- path makes the assertion SILENTLY not apply.
	path    TEXT NOT NULL,
	-- how THIS MOUNT views the volume; per-row, never per-volume (one volume can be
	-- shared-rw to one sandbox and shared-ro to another at the same time).
	--
	-- The CHECK is REQUIRED, not defensive: the lease index below has a LITERAL STRING
	-- predicate, so a mis-cased 'Exclusive' would make that index simply NOT APPLY and
	-- let two exclusive rows both insert. That is a silent failure of the one new
	-- invariant Volume introduces, so the legal set is asserted in the schema.
	access  TEXT NOT NULL CHECK (access IN ('exclusive','shared-ro','shared-rw')),
	-- Two topology assertions (§5.C), both judged 400 rather than 409 by the API:
	--   (sandbox, path):   two volumes at one destination means the second BIND SHADOWS
	--                      the first, so one volume is invisible in the guest while
	--                      still counting in §3.4 and still holding its lease. "An
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
-- SCHEMA-LEVEL ASSERTION, NOT THE INVARIANT'S ENFORCEMENT (§5.C). It forbids only
-- "two exclusive rows" and does NOT forbid an exclusive row coexisting with a shared
-- one — it covers half the invariant. The other half is delivered by the atomic
-- INSERT…SELECT…WHERE NOT EXISTS in CreateMounts, which expresses the full predicate.
--
-- The predicate keys on 'access', which is IMMUTABLE FOR THE ROW'S LIFETIME, and that
-- is a CHOICE rather than a coincidence. An external implementation keyed its
-- live-owner partial index on a soft-delete TIMESTAMP instead: paths that set the
-- column existed while NOTHING in the entire codebase ever cleared it back to NULL —
-- so one mistaken write both condemned the volume permanently AND freed the
-- uniqueness slot for a second holder to take. Writing the reason down is the point;
-- otherwise the next person moves this onto some 'detached_at'.
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
		// One statement set, one transaction (§4.4-C): the caps and the death-cause
		// clear that lifts §4.3's refusal land together or not at all. updated_at is
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
			// Without this branch a mounts.sandbox RESTRICT violation would reach the API's
			// default arm as a 500 instead of a 409 (§6.2). isFKViolation is table- and
			// column-agnostic, so the predicate is shared; the translation to a sentinel is
			// per-call-site and cannot be inherited.
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
// Unique violations (a second exclusive mount row, a PK collision) are a
// DIFFERENT verdict and correctly report false here — they need their own
// predicate and their own sentinel, not this one (§6.2's four distinct 409s).
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

// --- Volume CRUD (§5.C) ------------------------------------------------------

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
		// ErrVolumeNotFound (§4.3 reference side).
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
			// Reported, never folded into success (§6.2): the reference-protection
			// refusals land on this same path, so folding here would make a REFUSED
			// delete indistinguishable from a successful one.
			return ErrVolumeNotFound
		}
		return nil
	})
}

// --- Mount table (§5.C) ------------------------------------------------------

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
		// OLD binding in place — that is consequence ① and it needs no compensating code.
		return insertBinding(t, next)
	})
}

// insertBinding inserts every row of a validated binding inside an EXISTING transaction,
// enforcing the lease per row. Shared by CreateMounts and ReplaceMounts so the lease
// predicate has ONE spelling: a second copy is how the two write points would drift apart
// on the invariant §5.C calls the only new one Volume introduces.
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
			// A missing sandbox or volume is the REFERENCE side of §4.3 atomicity (a
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
// cover all four §6.2 verdicts. Same reasoning as isFKViolation for why this stays a
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
// written (§5.C, judged 400 by §6.2: decidable without reading another row, and no
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
		// The guest root is normalized BUT ILLEGAL (§5.C: absolute, cleaned, not "/"), and
		// this is a SEPARATE check rather than a stricter normalizer because "/" is a FIXED
		// POINT of path.Clean: "//", "/.", "/data/.." and " / " all normalize TO "/" and are
		// caught above, so only the bare "/" reaches here. Folding the two would mean making
		// normalizedMountPath return a value that is not normalized — destroying the ONE
		// definition NormalizeMountPath exists to share with the mediator. Rejected because a
		// bind at "/" SHADOWS THE ENTIRE GUEST ROOTFS: the overlay is still there and still
		// mounted, but nothing in it is reachable, and no uniqueness assertion can express
		// "does not shadow everything".
		if mt.Path == "/" {
			return fmt.Errorf("%w: path %q is the guest root — a bind there shadows the ENTIRE sandbox rootfs, leaving the overlay mounted but unreachable",
				ErrMountBindingInvalid, mt.Path)
		}
		if seenPath[mt.Path] {
			return fmt.Errorf("%w: destination %q appears twice — the second bind would SHADOW the first, leaving one volume invisible in the guest while it still counts in §3.4 and still holds its lease",
				ErrMountBindingInvalid, mt.Path)
		}
		if seenVol[mt.Volume] {
			return fmt.Errorf("%w: volume %s appears twice in one sandbox", ErrMountBindingInvalid, mt.Volume)
		}
		seenPath[mt.Path] = true
		seenVol[mt.Volume] = true
	}
	// Nesting: volumes must not mount inside one another's subtree (§5.C), or the
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

// NormalizeMountPath exposes that definition to the mediator (§5.C requires paths to
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
