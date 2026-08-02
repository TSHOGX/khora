// memhog — a bounded, statically-linkable memory probe for the M-? preflight
// spike "where does a RAM cap on a gVisor sandbox land?". Stdlib only
// (syscall + os), so it builds with CGO_ENABLED=0 GO111MODULE=off and drops
// into the minimal preflight rootfs, which has no coreutils and no compiler.
//
// Modes:
//   memhog idle                     block forever (usable as guest PID 1)
//   memhog meminfo                  dump what the guest kernel reports as RAM
//   memhog alloc <stepMiB> <maxMiB>  mmap+touch in bounded steps, print progress
//
// alloc uses raw anonymous mmap (not the Go heap) so that a refused allocation
// surfaces as an ENOMEM return value we can print, rather than a Go runtime
// "out of memory" fatal panic; and it TOUCHES every page so the memory is
// really charged, not just reserved. It stops at maxMiB — it can never grow
// without bound, which matters because the host is shared.
package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

const mib = 1 << 20

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: memhog idle|meminfo|alloc <stepMiB> <maxMiB>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "idle":
		fmt.Println("MEMHOG_IDLE_READY")
		for {
			time.Sleep(time.Hour)
		}
	case "meminfo":
		meminfo()
	case "alloc":
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: memhog alloc <stepMiB> <maxMiB>")
			os.Exit(2)
		}
		step, err1 := strconv.Atoi(os.Args[2])
		max, err2 := strconv.Atoi(os.Args[3])
		if err1 != nil || err2 != nil || step <= 0 || max <= 0 || max > 4096 {
			fmt.Fprintln(os.Stderr, "bad args (max is capped at 4096 MiB by design)")
			os.Exit(2)
		}
		alloc(step, max)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", os.Args[1])
		os.Exit(2)
	}
}

// meminfo dumps the fields that tell us whether a cap is visible IN the guest.
func meminfo() {
	dump := func(path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("READ_FAIL %s: %v\n", path, err)
			return
		}
		for _, line := range splitLines(string(b)) {
			for _, k := range []string{"MemTotal", "MemFree", "MemAvailable", "SwapTotal", "Committed_AS", "CommitLimit"} {
				if len(line) > len(k) && line[:len(k)] == k {
					fmt.Printf("MEMINFO %s\n", line)
				}
			}
		}
	}
	dump("/proc/meminfo")
	// If the guest exposes a cgroup hierarchy, report what it claims.
	for _, p := range []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory.current",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
		"/proc/self/cgroup",
	} {
		if b, err := os.ReadFile(p); err == nil {
			fmt.Printf("GUESTCG %s = %s\n", p, trim(string(b)))
		} else {
			fmt.Printf("GUESTCG %s = <absent: %v>\n", p, err)
		}
	}
}

// alloc mmaps stepMiB at a time up to maxMiB, touching every page.
func alloc(stepMiB, maxMiB int) {
	fmt.Printf("ALLOC_START step=%dMiB max=%dMiB pid=%d\n", stepMiB, maxMiB, os.Getpid())
	var keep [][]byte
	total := 0
	pageSize := syscall.Getpagesize()
	for total < maxMiB {
		b, err := syscall.Mmap(-1, 0, stepMiB*mib,
			syscall.PROT_READ|syscall.PROT_WRITE,
			syscall.MAP_ANON|syscall.MAP_PRIVATE)
		if err != nil {
			fmt.Printf("MMAP_REFUSED at total=%dMiB err=%v\n", total, err)
			fmt.Printf("ALLOC_END_REFUSED total=%dMiB\n", total)
			return
		}
		// Touch every page so it is actually committed, not just reserved.
		for i := 0; i < len(b); i += pageSize {
			b[i] = 0xA5
		}
		keep = append(keep, b)
		total += stepMiB
		fmt.Printf("TOUCHED total=%dMiB\n", total)
	}
	fmt.Printf("ALLOC_END_COMPLETE total=%dMiB regions=%d\n", total, len(keep))
	// Hold briefly so an asynchronous killer has a chance to act, then exit 0.
	time.Sleep(2 * time.Second)
	fmt.Println("STILL_ALIVE_AFTER_HOLD")
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i] + " | " + trim(s[i+1:])
		}
	}
	return s
}
