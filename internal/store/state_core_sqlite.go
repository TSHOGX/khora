package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TSHOGX/khora/internal/model"
)

func stateTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func readStateTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (s *SQLite) CreatePlacement(ctx context.Context, p model.Placement) error {
	if err := p.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO placements(sandbox, epoch, provider, state, created_at, updated_at) VALUES(?,?,?,?,?,?)`,
		string(p.Sandbox), int64(p.Epoch), p.Provider, string(p.State), stateTime(p.CreatedAt), stateTime(p.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if isFKViolation(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (s *SQLite) GetPlacement(ctx context.Context, sandbox model.SandboxID) (model.Placement, error) {
	var p model.Placement
	var sb, provider, state string
	var epoch, created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT sandbox, epoch, provider, state, created_at, updated_at FROM placements WHERE sandbox=?`, string(sandbox)).Scan(&sb, &epoch, &provider, &state, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Placement{}, ErrPlacementNotFound
	}
	if err != nil {
		return model.Placement{}, err
	}
	p.Sandbox = model.SandboxID(sb)
	p.Epoch = model.PlacementEpoch(epoch)
	p.Provider = provider
	p.State = model.PlacementState(state)
	p.CreatedAt = readStateTime(created)
	p.UpdatedAt = readStateTime(updated)
	return p, nil
}

func (s *SQLite) ListPlacements(ctx context.Context) ([]model.Placement, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sandbox, epoch, provider, state, created_at, updated_at FROM placements ORDER BY sandbox`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Placement, 0)
	for rows.Next() {
		var sb, provider, state string
		var epoch, created, updated int64
		if err := rows.Scan(&sb, &epoch, &provider, &state, &created, &updated); err != nil {
			return nil, err
		}
		out = append(out, model.Placement{Sandbox: model.SandboxID(sb), Epoch: model.PlacementEpoch(epoch), Provider: provider, State: model.PlacementState(state), CreatedAt: readStateTime(created), UpdatedAt: readStateTime(updated)})
	}
	return out, rows.Err()
}

func (s *SQLite) UpdatePlacement(ctx context.Context, p model.Placement) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return s.tx(ctx, func(t *sql.Tx) error {
		var current int64
		if err := t.QueryRow(`SELECT epoch FROM placements WHERE sandbox=?`, string(p.Sandbox)).Scan(&current); errors.Is(err, sql.ErrNoRows) {
			return ErrPlacementNotFound
		} else if err != nil {
			return err
		}
		if int64(p.Epoch) < current {
			return model.ErrStaleEpoch
		}
		_, err := t.Exec(`UPDATE placements SET epoch=?, provider=?, state=?, created_at=?, updated_at=? WHERE sandbox=?`, int64(p.Epoch), p.Provider, string(p.State), stateTime(p.CreatedAt), stateTime(p.UpdatedAt), string(p.Sandbox))
		return err
	})
}

func (s *SQLite) DeletePlacement(ctx context.Context, sandbox model.SandboxID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM placements WHERE sandbox=?`, string(sandbox))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrPlacementNotFound
	}
	return nil
}

func encodeCursor(c model.Cursor) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeCursor(raw string) (model.Cursor, error) {
	var c model.Cursor
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return model.Cursor{}, err
	}
	if err := c.Validate(); err != nil {
		return model.Cursor{}, err
	}
	return c, nil
}

func (s *SQLite) CreateProcess(ctx context.Context, p model.Process) error {
	if err := p.Validate(); err != nil {
		return err
	}
	cursor, err := encodeCursor(p.OutputCursor)
	if err != nil {
		return fmt.Errorf("store: encode process cursor: %w", err)
	}
	var exitCode any
	if p.ExitCode != nil {
		exitCode = *p.ExitCode
	}
	var exited any
	if p.ExitedAt != nil {
		exited = stateTime(*p.ExitedAt)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO processes(id, sandbox, placement_epoch, pid, state, exit_code, started_at, exited_at, output_cursor) VALUES(?,?,?,?,?,?,?,?,?)`, string(p.ID), string(p.Sandbox), int64(p.PlacementEpoch), p.PID, string(p.State), exitCode, stateTime(p.StartedAt), exited, cursor)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrProcessConflict
		}
		if isFKViolation(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func scanProcessRow(row rowScanner) (model.Process, error) {
	var p model.Process
	var id, sandbox, state, cursorRaw string
	var epoch, started int64
	var pid int
	var exitCode sql.NullInt64
	var exited sql.NullInt64
	if err := row.Scan(&id, &sandbox, &epoch, &pid, &state, &exitCode, &started, &exited, &cursorRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Process{}, ErrProcessNotFound
		}
		return model.Process{}, err
	}
	cursor, err := decodeCursor(cursorRaw)
	if err != nil {
		return model.Process{}, fmt.Errorf("store: decode process cursor: %w", err)
	}
	p.ID = model.ProcessID(id)
	p.Sandbox = model.SandboxID(sandbox)
	p.PlacementEpoch = model.PlacementEpoch(epoch)
	p.PID = pid
	p.State = model.ProcessState(state)
	if exitCode.Valid {
		v := int(exitCode.Int64)
		p.ExitCode = &v
	}
	p.StartedAt = readStateTime(started)
	if exited.Valid {
		t := readStateTime(exited.Int64)
		p.ExitedAt = &t
	}
	p.OutputCursor = cursor
	return p, nil
}

func (s *SQLite) GetProcess(ctx context.Context, id model.ProcessID) (model.Process, error) {
	return scanProcessRow(s.db.QueryRowContext(ctx, `SELECT id, sandbox, placement_epoch, pid, state, exit_code, started_at, exited_at, output_cursor FROM processes WHERE id=?`, string(id)))
}

func (s *SQLite) ListProcesses(ctx context.Context, sandbox model.SandboxID) ([]model.Process, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, sandbox, placement_epoch, pid, state, exit_code, started_at, exited_at, output_cursor FROM processes WHERE sandbox=? ORDER BY started_at, id`, string(sandbox))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Process, 0)
	for rows.Next() {
		p, err := scanProcessRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLite) UpdateProcess(ctx context.Context, p model.Process) error {
	if err := p.Validate(); err != nil {
		return err
	}
	cursor, err := encodeCursor(p.OutputCursor)
	if err != nil {
		return err
	}
	var exitCode, exited any
	if p.ExitCode != nil {
		exitCode = *p.ExitCode
	}
	if p.ExitedAt != nil {
		exited = stateTime(*p.ExitedAt)
	}
	return s.tx(ctx, func(t *sql.Tx) error {
		var sandbox string
		var epoch, started int64
		if err := t.QueryRow(`SELECT sandbox, placement_epoch, started_at FROM processes WHERE id=?`, string(p.ID)).Scan(&sandbox, &epoch, &started); errors.Is(err, sql.ErrNoRows) {
			return ErrProcessNotFound
		} else if err != nil {
			return err
		}
		if sandbox != string(p.Sandbox) || epoch != int64(p.PlacementEpoch) || started != stateTime(p.StartedAt) {
			return ErrProcessConflict
		}
		_, err := t.Exec(`UPDATE processes SET pid=?, state=?, exit_code=?, exited_at=?, output_cursor=? WHERE id=?`, p.PID, string(p.State), exitCode, exited, cursor, string(p.ID))
		return err
	})
}

func (s *SQLite) DeleteProcess(ctx context.Context, id model.ProcessID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM processes WHERE id=?`, string(id))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrProcessNotFound
	}
	return nil
}

func (s *SQLite) ReclaimProcesses(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE processes SET state='reclaimed', pid=0, exit_code=NULL WHERE state='exited' AND exited_at IS NOT NULL AND exited_at <= ?`, stateTime(before))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func operationArgs(op model.Operation) (any, any, error) {
	var resultKind, resultID any
	if op.Result != nil {
		resultKind, resultID = string(op.Result.Kind), op.Result.ID
	}
	return resultKind, resultID, nil
}

func scanOperationRow(row rowScanner) (model.Operation, error) {
	var op model.Operation
	var id, scope, key, hash, state, phase string
	var resultKind, resultID, errorClass sql.NullString
	var attempt int64
	var started, updated, lease int64
	if err := row.Scan(&id, &scope, &key, &hash, &state, &phase, &attempt, &resultKind, &resultID, &errorClass, &started, &updated, &lease); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Operation{}, ErrOperationNotFound
		}
		return model.Operation{}, err
	}
	op.ID = model.OperationID(id)
	op.Scope = scope
	op.IdempotencyKey = model.IdempotencyKey(key)
	op.RequestHash = hash
	op.State = model.OperationState(state)
	op.Phase = model.OperationPhase(phase)
	op.Attempt = uint32(attempt)
	op.StartedAt = readStateTime(started)
	op.UpdatedAt = readStateTime(updated)
	op.LeaseUntil = readStateTime(lease)
	op.ErrorClass = model.ErrorClass(errorClass.String)
	if resultKind.Valid || resultID.Valid {
		op.Result = &model.OperationResult{Kind: model.ObjectKind(resultKind.String), ID: resultID.String}
	}
	return op, nil
}

func (s *SQLite) CreateOperation(ctx context.Context, op model.Operation) error {
	if err := op.Validate(); err != nil {
		return err
	}
	kind, resultID, err := operationArgs(op)
	if err != nil {
		return err
	}
	var errorClass any
	if op.ErrorClass != model.ErrorNone {
		errorClass = string(op.ErrorClass)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO operations(id, scope, idempotency_key, request_hash, state, phase, attempt, result_kind, result_id, error_class, started_at, updated_at, lease_until) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, string(op.ID), op.Scope, string(op.IdempotencyKey), op.RequestHash, string(op.State), string(op.Phase), op.Attempt, kind, resultID, errorClass, stateTime(op.StartedAt), stateTime(op.UpdatedAt), stateTime(op.LeaseUntil))
	if err != nil {
		if isUniqueViolation(err) {
			// Distinguish a duplicate operation id from a reused scope/key.
			var exists int
			if qerr := s.db.QueryRowContext(ctx, `SELECT 1 FROM operations WHERE id=?`, string(op.ID)).Scan(&exists); qerr == nil {
				return ErrConflict
			}
			return ErrOperationKeyConflict
		}
		return err
	}
	return nil
}

func (s *SQLite) GetOperation(ctx context.Context, id model.OperationID) (model.Operation, error) {
	return scanOperationRow(s.db.QueryRowContext(ctx, `SELECT id, scope, idempotency_key, request_hash, state, phase, attempt, result_kind, result_id, error_class, started_at, updated_at, lease_until FROM operations WHERE id=?`, string(id)))
}

func (s *SQLite) GetOperationByKey(ctx context.Context, scope string, key model.IdempotencyKey) (model.Operation, error) {
	return scanOperationRow(s.db.QueryRowContext(ctx, `SELECT id, scope, idempotency_key, request_hash, state, phase, attempt, result_kind, result_id, error_class, started_at, updated_at, lease_until FROM operations WHERE scope=? AND idempotency_key=?`, scope, string(key)))
}

func (s *SQLite) ListOperations(ctx context.Context, state model.OperationState) ([]model.Operation, error) {
	query := `SELECT id, scope, idempotency_key, request_hash, state, phase, attempt, result_kind, result_id, error_class, started_at, updated_at, lease_until FROM operations`
	args := []any{}
	if state != "" {
		query += ` WHERE state=?`
		args = append(args, string(state))
	}
	query += ` ORDER BY updated_at, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Operation, 0)
	for rows.Next() {
		op, err := scanOperationRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// TakeoverOperation atomically transfers an expired lease. The WHERE clause
// carries the observed attempt token, so two retries cannot both acquire the
// same operation after expiry.
func (s *SQLite) TakeoverOperation(ctx context.Context, id model.OperationID, expectedAttempt uint32, now time.Time, lease time.Duration) (model.Operation, error) {
	if expectedAttempt == 0 || now.IsZero() || lease <= 0 {
		return model.Operation{}, model.ErrInvalidArgument
	}
	var out model.Operation
	err := s.tx(ctx, func(t *sql.Tx) error {
		res, err := t.Exec(`UPDATE operations
			SET attempt=attempt+1, updated_at=?, lease_until=?
			WHERE id=? AND state='in_progress' AND attempt=?
			  AND lease_until>0 AND lease_until<=?`,
			stateTime(now), stateTime(now.Add(lease)), string(id), expectedAttempt, stateTime(now))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrOperationOwnershipConflict
		}
		out, err = scanOperationRow(t.QueryRow(`SELECT id, scope, idempotency_key, request_hash, state, phase, attempt, result_kind, result_id, error_class, started_at, updated_at, lease_until FROM operations WHERE id=?`, string(id)))
		return err
	})
	if err != nil {
		return model.Operation{}, err
	}
	return out, nil
}

// RenewOperation extends a live lease only for its current attempt. A caller
// holding an old attempt token cannot resurrect an operation after takeover.
func (s *SQLite) RenewOperation(ctx context.Context, id model.OperationID, expectedAttempt uint32, now time.Time, lease time.Duration) error {
	if expectedAttempt == 0 || now.IsZero() || lease <= 0 {
		return model.ErrInvalidArgument
	}
	res, err := s.db.ExecContext(ctx, `UPDATE operations SET updated_at=?, lease_until=?
		WHERE id=? AND state='in_progress' AND attempt=? AND lease_until>?`,
		stateTime(now), stateTime(now.Add(lease)), string(id), expectedAttempt, stateTime(now))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrOperationOwnershipConflict
	}
	return nil
}

func (s *SQLite) UpdateOperation(ctx context.Context, op model.Operation) error {
	if err := op.Validate(); err != nil {
		return err
	}
	kind, resultID, err := operationArgs(op)
	if err != nil {
		return err
	}
	var errorClass any
	if op.ErrorClass != model.ErrorNone {
		errorClass = string(op.ErrorClass)
	}
	return s.tx(ctx, func(t *sql.Tx) error {
		var scope, key, hash, state string
		var attempt int64
		var updated, leaseUntil int64
		if err := t.QueryRow(`SELECT scope, idempotency_key, request_hash, state, attempt, updated_at, lease_until FROM operations WHERE id=?`, string(op.ID)).Scan(&scope, &key, &hash, &state, &attempt, &updated, &leaseUntil); errors.Is(err, sql.ErrNoRows) {
			return ErrOperationNotFound
		} else if err != nil {
			return err
		}
		if scope != op.Scope || key != string(op.IdempotencyKey) || hash != op.RequestHash {
			return ErrOperationKeyConflict
		}
		if uint32(attempt) != op.Attempt || state != string(model.OperationInProgress) && state != string(op.State) || stateTime(op.UpdatedAt) < updated || (op.State == model.OperationInProgress && stateTime(op.LeaseUntil) < leaseUntil) {
			return ErrOperationOwnershipConflict
		}
		if state != string(model.OperationInProgress) {
			current, err := scanOperationRow(t.QueryRow(`SELECT id, scope, idempotency_key, request_hash, state, phase, attempt, result_kind, result_id, error_class, started_at, updated_at, lease_until FROM operations WHERE id=?`, string(op.ID)))
			if err != nil {
				return err
			}
			if !sameTerminalOperation(current, op) {
				return ErrOperationOwnershipConflict
			}
		}
		res, err := t.Exec(`UPDATE operations SET state=?, phase=?, attempt=?, result_kind=?, result_id=?, error_class=?, started_at=?, updated_at=?, lease_until=? WHERE id=? AND attempt=?`, string(op.State), string(op.Phase), op.Attempt, kind, resultID, errorClass, stateTime(op.StartedAt), stateTime(op.UpdatedAt), stateTime(op.LeaseUntil), string(op.ID), op.Attempt)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n != 1 {
			return ErrOperationOwnershipConflict
		}
		return err
	})
}

func (s *SQLite) DeleteOperation(ctx context.Context, id model.OperationID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM operations WHERE id=?`, string(id))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrOperationNotFound
	}
	return nil
}

func (s *SQLite) AppendTelemetry(ctx context.Context, sample model.TelemetrySample) error {
	if sample.Sandbox == "" {
		return model.ErrInvalidArgument
	}
	if err := sample.Snapshot.CPU.Validate(); err != nil {
		return err
	}
	if err := sample.Snapshot.Memory.Validate(); err != nil {
		return err
	}
	if err := sample.Snapshot.Disk.Validate(); err != nil {
		return err
	}
	if sample.Snapshot.At.IsZero() {
		sample.Snapshot.At = time.Now()
	}
	if sample.Envelope.OccurredAt.IsZero() {
		sample.Envelope.OccurredAt = sample.Snapshot.At
	}
	return s.tx(ctx, func(t *sql.Tx) error {
		var exists int
		if err := t.QueryRow(`SELECT 1 FROM sandboxes WHERE id=?`, string(sample.Sandbox)).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var seq int64
		if err := t.QueryRow(`SELECT COALESCE(MAX(sequence),0)+1 FROM telemetry WHERE sandbox=?`, string(sample.Sandbox)).Scan(&seq); err != nil {
			return err
		}
		sample.Envelope.Stream = model.CursorStreamTelemetry
		sample.Envelope.ObjectKind = model.ObjectSandbox
		sample.Envelope.ObjectID = string(sample.Sandbox)
		sample.Envelope.Sequence = uint64(seq)
		if sample.Envelope.ID == "" {
			sample.Envelope.ID = fmt.Sprintf("telemetry_%s_%d", sample.Sandbox, seq)
		}
		if err := sample.Validate(); err != nil {
			return err
		}
		_, err := t.Exec(`INSERT INTO telemetry(sandbox,sequence,event_id,placement_epoch,process_id,state,cpu_available,cpu_value,memory_available,memory_value,disk_available,disk_value,at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, string(sample.Sandbox), seq, sample.Envelope.ID, int64(sample.PlacementEpoch), nullableProcess(sample.Process), sample.Snapshot.State, boolInt(sample.Snapshot.CPU.Available), sample.Snapshot.CPU.Value, boolInt(sample.Snapshot.Memory.Available), sample.Snapshot.Memory.Value, boolInt(sample.Snapshot.Disk.Available), sample.Snapshot.Disk.Value, stateTime(sample.Snapshot.At))
		if isFKViolation(err) {
			return ErrNotFound
		}
		return err
	})
}

func nullableProcess(id model.ProcessID) any {
	if id == "" {
		return nil
	}
	return string(id)
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func scanTelemetry(row rowScanner) (model.TelemetrySample, error) {
	var sandbox, eventID, state string
	var seq, epoch, at int64
	var process sql.NullString
	var ca, cv, ma, mv, da, dv int
	if err := row.Scan(&sandbox, &seq, &eventID, &epoch, &process, &state, &ca, &cv, &ma, &mv, &da, &dv, &at); err != nil {
		return model.TelemetrySample{}, err
	}
	sid := model.SandboxID(sandbox)
	return model.TelemetrySample{Sandbox: sid, PlacementEpoch: model.PlacementEpoch(epoch), Process: model.ProcessID(process.String), Envelope: model.EventEnvelope{ID: eventID, Stream: model.CursorStreamTelemetry, ObjectKind: model.ObjectSandbox, ObjectID: sandbox, Epoch: 0, Sequence: uint64(seq), OccurredAt: readStateTime(at)}, Snapshot: model.ResourceSnapshot{CPU: model.Measurement{Available: ca != 0, Value: int64(cv)}, Memory: model.Measurement{Available: ma != 0, Value: int64(mv)}, Disk: model.Measurement{Available: da != 0, Value: int64(dv)}, State: state, At: readStateTime(at)}}, nil
}

func (s *SQLite) Telemetry(ctx context.Context, sandbox model.SandboxID, after uint64, limit int) ([]model.TelemetrySample, uint64, uint64, error) {
	if limit <= 0 {
		return nil, 0, 0, fmt.Errorf("store: Telemetry limit must be positive")
	}
	var earliest, latest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(sequence), MAX(sequence) FROM telemetry WHERE sandbox=?`, string(sandbox)).Scan(&earliest, &latest); err != nil {
		return nil, 0, 0, err
	}
	if !earliest.Valid {
		return []model.TelemetrySample{}, 0, 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sandbox,sequence,event_id,placement_epoch,process_id,state,cpu_available,cpu_value,memory_available,memory_value,disk_available,disk_value,at FROM telemetry WHERE sandbox=? AND sequence>? ORDER BY sequence LIMIT ?`, string(sandbox), int64(after), limit)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	out := make([]model.TelemetrySample, 0, limit)
	for rows.Next() {
		sample, err := scanTelemetry(rows)
		if err != nil {
			return nil, 0, 0, err
		}
		out = append(out, sample)
	}
	return out, uint64(earliest.Int64), uint64(latest.Int64), rows.Err()
}

func (s *SQLite) TrimTelemetry(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("store: TrimTelemetry limit must be positive")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM telemetry WHERE rowid IN (SELECT rowid FROM telemetry t WHERE t.at < ? AND t.sequence < (SELECT MAX(sequence) FROM telemetry t2 WHERE t2.sandbox=t.sandbox) ORDER BY t.at LIMIT ?)`, stateTime(before), limit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
