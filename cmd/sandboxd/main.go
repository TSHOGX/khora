// Command sandboxd is the in-guest daemon — the core the transport data plane / the guest startup topology component.
// ONE binary, TWO modes (the guest startup topology):
//
//	init mode (PID 1)      : minimal self-authored init — reaper (waitpid loop),
//	                         container-level signal fan-out, supervise +
//	                         in-place restart of transport mode. tini source is
//	                         the correctness checklist; the tini binary is NOT
//	                         bundled (the guest startup topology / guest init semantics).
//	transport mode (child) : the process/fs endpoints of the transport data plane (Connect-RPC over
//	                         h2c) — payload-agnostic, knows NOTHING of
//	                         agent/turn/session (the process/fs boundary).
//
// runsc uses sandboxd as the container entrypoint; init mode (PID 1) re-execs
// itself via /proc/self/exe into transport mode as a child (the guest startup topology). Splitting
// the two: a transport crash degrades to in-process-recoverable (FS-first pause and resume "a crash =
// an involuntary pause") instead of substrate death, and keeps PID 1 signal
// semantics away
// from the complex server (the init and sandboxd split). That restart is crash recovery
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
// "imports nothing from internal/" (the deployment boundary) and a version package would be the
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
