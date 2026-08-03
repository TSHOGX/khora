// fakeguest is a MINIMAL stand-in guest entrypoint used ONLY by the local_runsc
// L2 test (build tag 'substrate'). It isolates the runtime subsystem from the
// real wire protocol: as PID 1 it just binds the transport port inside
// the netns and answers, proving netns/veth/IP-discovery/runsc-adopt/dial
// without pulling in the real sandboxd. Not shipped, not the real daemon.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"

	"engine/internal/guestport"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "OK")
	})
	// The port comes from the shared leaf, so this fake cannot drift from the real
	// daemon. internal/guestport has no imports of its own, which is what keeps
	// this throwaway dependency-free (see the package doc).
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(guestport.Port)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeguest: listen: %v\n", err)
		os.Exit(1)
	}
	_ = http.Serve(ln, mux)
}
