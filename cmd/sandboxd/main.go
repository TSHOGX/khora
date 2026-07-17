// Command sandboxd is the in-guest daemon — the core §5.E / §5.E.1 component.
// ONE binary, TWO modes (§5.E.1):
//
//	init mode (PID 1)      : minimal self-authored init — reaper (waitpid loop),
//	                         container-level signal fan-out, PR_SET_CHILD_SUBREAPER
//	                         fallback, supervise + in-place restart of transport
//	                         mode. tini source is the correctness checklist; the
//	                         tini binary is NOT bundled (§5.E.1 / guest-runtime D4.1).
//	transport mode (child) : the process/fs endpoints of §5.E — payload-agnostic,
//	                         knows NOTHING of agent/turn/session (transport D1).
//
// runsc uses sandboxd as the container entrypoint; init mode (PID 1) re-execs
// itself via /proc/self/exe into transport mode as a child (§5.E.1). Splitting
// the two: a transport crash degrades to in-process-recoverable (§4.2 崩溃 =
// 非自愿 pause) instead of substrate death, enables hot-upgrade, and keeps PID 1
// signal semantics away from the complex server (guest-runtime D4.2).
//
// M1: skeleton dispatch on mode. Real init reaper + transport RPC server are the
// M1 implementation work.
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
		// M1 TODO: PID 1 — install reaper + signal fan-out, PR_SET_CHILD_SUBREAPER,
		// re-exec /proc/self/exe transport, supervise + restart.
		fmt.Println("sandboxd: init mode (PID 1) — M1 skeleton")
	case "transport":
		// M1 TODO: bind netns TCP, serve Connect-RPC process/fs + Health.
		fmt.Println("sandboxd: transport mode — M1 skeleton")
	default:
		fmt.Fprintf(os.Stderr, "sandboxd: unknown mode %q\n", mode)
		os.Exit(2)
	}
}
