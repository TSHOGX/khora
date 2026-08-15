package orchestrator

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
)

// The orchestrator-side half of the the documented invariant directed-signal boundary. The
// AUTHORITATIVE check is the guest's (the documented invariant: the guest can write its own fs, so
// it must validate for itself); this one is fail-fast, saving a round trip.
//
// What makes it testable with a zero Engine is exactly the property under test:
// the refusal must come BEFORE clientFor, i.e. before any sandbox lookup or
// dial. A proxy over a nil-seam Engine would panic if the check were placed
// after — so "it returns invalid_argument instead of panicking" IS the ordering
// assertion. A valid pid is deliberately not exercised here; that path needs a
// real guest and is covered on the substrate.
func TestSendSignalFailsFastOnNonPositivePid(t *testing.T) {
	p := &processProxy{e: &Engine{}}
	for _, pid := range []int32{-2147483648, -1, 0} {
		req := connect.NewRequest(&v1.SendSignalRequest{Pid: pid, Signal: 15})
		req.Header().Set(sandboxIDHeader, "sbx-irrelevant")

		resp, err := p.SendSignal(context.Background(), req)
		if err == nil {
			t.Errorf("SendSignal(pid=%d) accepted (resp %v), want invalid_argument — "+
				"pid <= 0 is POSIX fan-out and belongs to guest PID 1 (the documented invariant)", pid, resp)
			continue
		}
		if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
			t.Errorf("SendSignal(pid=%d) code = %v, want invalid_argument (err %v)", pid, code, err)
		}
		if !strings.Contains(err.Error(), "PID 1") {
			t.Errorf("SendSignal(pid=%d) message does not say where fan-out belongs: %v", pid, err)
		}
	}
}

// The two ends must refuse for the same stated reason: a caller that retries
// against the guest after the host refused should not get a different story.
// The message is duplicated on purpose (see the const's comment) — which is
// precisely why it needs a test that the copies have not drifted.
func TestSignalPidErrMatchesGuestWording(t *testing.T) {
	const guest = "pid must be > 0: SendSignal carries DIRECTED signals only " +
		"(one process, defaulting to its group). pid <= 0 is POSIX fan-out — " +
		"kill(0)/kill(-1) address a whole group or the whole pid namespace — and " +
		"container-level fan-out belongs to guest PID 1, not the transport channel " +
		"(guest-runtime the documented invariant / the documented invariant)"
	if signalPidErr != guest {
		t.Errorf("host and guest refusal messages have drifted\n host:  %q\n guest: %q\n"+
			"(the guest copy lives in cmd/sandboxd/transport.go)", signalPidErr, guest)
	}
}
