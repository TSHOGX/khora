package store

import (
	"time"

	"github.com/TSHOGX/khora/internal/model"
)

// validateProcessUpdate enforces the monotonic logical-process lifecycle at
// the Store boundary. Reconciliation and output forwarding can race: once a
// retention pass has changed an exited row to reclaimed, a delayed frame must
// not resurrect that handle or restore its cleared terminal metadata.
//
// The Store owns this check because Memory and SQLite must make the same
// decision while holding their respective write critical sections. Callers
// receive the existing process-conflict sentinel; no backend-specific error
// or diagnostic text is part of the contract.
func validateProcessUpdate(old, next model.Process) error {
	if next.OutputCursor.Sequence < old.OutputCursor.Sequence {
		return ErrProcessConflict
	}

	switch old.State {
	case model.ProcessStarting:
		if next.State == model.ProcessStarting && next.PID != old.PID {
			return ErrProcessConflict
		}
		if next.State == model.ProcessRunning && next.PID <= 0 {
			return ErrProcessConflict
		}
		if next.State == model.ProcessPaused && next.PID != 0 {
			return ErrProcessConflict
		}
		switch next.State {
		case model.ProcessStarting, model.ProcessRunning, model.ProcessExited, model.ProcessPaused:
			return nil
		default:
			return ErrProcessConflict
		}
	case model.ProcessRunning:
		if next.State == model.ProcessRunning && next.PID != old.PID {
			return ErrProcessConflict
		}
		if next.State == model.ProcessPaused && next.PID != 0 {
			return ErrProcessConflict
		}
		if next.State == model.ProcessExited && next.PID != 0 && next.PID != old.PID {
			return ErrProcessConflict
		}
		switch next.State {
		case model.ProcessRunning, model.ProcessExited, model.ProcessPaused:
			return nil
		default:
			return ErrProcessConflict
		}
	case model.ProcessExited:
		// Exit metadata is terminal. A duplicate replay may advance the output
		// cursor, but it cannot replace the observed exit result or locator.
		if next.State != model.ProcessExited || next.PID != old.PID || !sameInt(old.ExitCode, next.ExitCode) || !sameTime(old.ExitedAt, next.ExitedAt) {
			return ErrProcessConflict
		}
		return nil
	case model.ProcessPaused:
		// A paused row has no live provider locator. A late data-plane frame may
		// not reopen it or alter terminal metadata; an identical replay with a
		// newer cursor remains harmless and monotonic.
		if next.State != model.ProcessPaused || next.PID != old.PID || !sameInt(old.ExitCode, next.ExitCode) || !sameTime(old.ExitedAt, next.ExitedAt) {
			return ErrProcessConflict
		}
		return nil
	case model.ProcessReclaimed:
		// Reclamation is the terminal retention boundary. No delayed stream
		// update may resurrect the handle or rewrite its cleared metadata.
		return ErrProcessConflict
	default:
		return ErrProcessConflict
	}
}

func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
