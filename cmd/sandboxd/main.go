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
// the two: a transport crash degrades to in-process-recoverable (§4.2 "a crash =
// an involuntary pause") instead of substrate death, and keeps PID 1 signal
// semantics away
// from the complex server (guest-runtime D4.2). That restart is crash recovery
// only, NOT a version-upgrade path: /proc/self/exe is a handle on PID 1's
// already-open inode, so the supervise loop always re-execs the same bytes.
package main

import (
	"fmt"
	"os"
)

// version is stamped by the Makefile's -X at build time; "dev" means built by a
// bare `go build`. It answers what a binary in the field IS, which is the one
// thing a tag cannot do on its own.
//
// A separate var per binary rather than a shared package, because the gateway
// "imports nothing from internal/" (ADR-0007) and a version package would be the
// first exception. For THIS binary there is a second reason: an in-guest daemon
// that imported an engine package would put engine code inside the sandbox.
var version = "dev"

func main() {
	mode := "init"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	// --version is a MODE here, not a flag: this binary dispatches on os.Args[1]
	// before any flag parsing, so a flag would be unreachable behind the default
	// arm's "unknown mode" exit.
	case "--version", "-version", "version":
		fmt.Printf("sandboxd %s\n", version)
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
