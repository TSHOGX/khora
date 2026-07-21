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
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "OK")
	})
	// Port 9000 must match transport.Port; not imported here to keep this
	// throwaway fake dependency-free (see the package doc).
	ln, err := net.Listen("tcp", "0.0.0.0:9000")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeguest: listen: %v\n", err)
		os.Exit(1)
	}
	_ = http.Serve(ln, mux)
}
