//go:build !testguest

package main

// exitIfCrashLoopSentinel is a no-op in a production guest: the sentinel that
// makes transport mode exit before serving exists only under the `testguest`
// build tag (see faultinject_testguest.go). Keeping the seam as two files rather
// than one runtime check is deliberate — a guest is root-capable and can write
// its own filesystem (the engine vocabulary boundary), so a fault path compiled into production would be
// reachable by the sandbox itself.
func exitIfCrashLoopSentinel() {}
