//go:build testguest

// Fault injection for the in-guest daemon, compiled ONLY under the `testguest`
// tag. A production sandboxd does not contain this file, so the sentinel path
// below cannot influence a production guest even if the file happens to exist —
// the check is not there to be bypassed, it is absent.
//
// The engine's own fault hooks (FaultAfterColdStart) live host-side in the
// mediator and can stay in the normal build because the engine controls when
// they fire. This one is different: it triggers on state INSIDE a guest whose
// filesystem the guest itself can write (the engine vocabulary boundary root-capable), so shipping it in
// production would let a sandbox disable its own transport by touching a path.

package main

import "os"

// crashLoopSentinel is the guest analog of the mediator's FaultAfterColdStart
// hook: if this file exists on the guest's tmpfs /tmp when transport mode
// starts, the child exits BEFORE serving. A test writes it, then kills the
// transport child — PID1 re-execs it, it sees the sentinel and exits, and it
// keeps crash-looping (L1 container alive, L2 never comes up = transport-down
// forever). That manufactures the health and reclamation transport-down crash-loop the reaper must
// escalate past the restart budget (the transport-down restart budget). Because /tmp is tmpfs
// (not the Overlay) it is CLEARED on resume, so the reaper's escalation
// cold-start comes up healthy on a fresh /tmp.
//
// Its path must stay in rootfs.promoteExcludedPaths: a Promote that captured it
// would mint a Template whose every sandbox is born unable to serve. Under this
// build tag that mirroring only has to hold for test guests, but the exclusion
// list is cheap and stays unconditional.
const crashLoopSentinel = "/tmp/.sandboxd-crashloop"

func exitIfCrashLoopSentinel() {
	if _, err := os.Stat(crashLoopSentinel); err != nil {
		return
	}
	logf("transport: crash-loop sentinel present (%s) — exiting before serve (TEST fault injection)", crashLoopSentinel)
	os.Exit(1)
}
