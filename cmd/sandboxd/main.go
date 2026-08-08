// Command sandboxd is the in-guest daemon — the core §5.E / §5.E.1 component.
// ONE binary, TWO modes (§5.E.1):
//
//	init mode (PID 1)      : minimal self-authored init — reaper (waitpid loop),
//	                         container-level signal fan-out, supervise +
//	                         in-place restart of transport mode. tini source is
//	                         the correctness checklist; the tini binary is NOT
//	                         bundled (§5.E.1 / guest-runtime D4.1).
//	transport mode (child) : the process/fs endpoints of §5.E (Connect-RPC over
//	                         h2c) — payload-agnostic, knows NOTHING of
//	                         agent/turn/session (transport D1).
//
// runsc uses sandboxd as the container entrypoint; init mode (PID 1) re-execs
// itself via /proc/self/exe into transport mode as a child (§5.E.1). Splitting
// the two: a transport crash degrades to in-process-recoverable (§4.2 崩溃 =
// 非自愿 pause) instead of substrate death, and keeps PID 1 signal semantics away
// from the complex server (guest-runtime D4.2). That restart is crash recovery
// only, NOT a version-upgrade path: /proc/self/exe is a handle on PID 1's
// already-open inode, so the supervise loop always re-execs the same bytes.
package main

import (
	"fmt"
	"os"
)

func main() {
	mode := "init"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "init":
		initMode()
	case "transport":
		transportMode()
	default:
		fmt.Fprintf(os.Stderr, "sandboxd: unknown mode %q\n", mode)
		os.Exit(2)
	}
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[sandboxd] "+format+"\n", a...)
}
