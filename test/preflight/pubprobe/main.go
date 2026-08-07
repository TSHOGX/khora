// pubprobe — a two-mode helper for spike-publish-ingress.sh.
//
//	pubprobe serve <bind-addr> <banner>   run a TCP listener, reply <banner> per conn
//	pubprobe dial  <addr> <timeout-ms>    dial once, classify the outcome, print one line
//	pubprobe echo  <bind-addr>            listener that echoes each line back, connection
//	                                      held open (serve closes after one banner, so it
//	                                      cannot answer "is this flow still carrying bytes")
//	pubprobe hold  <addr> <n> <gap-ms>    dial once, then send n messages and READ BACK each
//	                                      echo, reporting per-message success. This is the
//	                                      only mode that distinguishes a live flow from a
//	                                      dead one: a write() to a socket whose peer is gone
//	                                      typically succeeds once before EPIPE, so "the write
//	                                      returned 0" proves nothing — the readback does.
//	pubprobe idle                         stay alive, listen on nothing (the "no
//	                                      listener" control case; the spike rootfs
//	                                      has no coreutils, so there is no `sleep`)
//
// The dial side exists because the thing under measurement is exactly the
// DISTINCTION between failure modes (refused vs reset vs timeout), and shell
// tools collapse them: nc prints the same "failed" for refused and reset, and
// curl's exit codes fold reset into 56/52 depending on version. Go's net errors
// carry the syscall errno, so the classification below is the measurement, not
// a convenience wrapper.
//
// Output is one KEY=value line, so the shell can assert on it without parsing prose.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pubprobe serve <bind> <banner> | pubprobe dial <addr> <timeout-ms>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2], os.Args[3])
	case "idle":
		fmt.Println("IDLE_NO_LISTENER=1")
		os.Stdout.Sync()
		// Not `select {}`: the Go runtime detects that as an all-goroutines
		// deadlock and panics, which would kill the guest under measurement.
		for {
			time.Sleep(time.Hour)
		}
	case "echo":
		echo(os.Args[2])
	case "hold":
		n, err := strconv.Atoi(os.Args[3])
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad count: %v\n", err)
			os.Exit(2)
		}
		gap, err := strconv.Atoi(os.Args[4])
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad gap: %v\n", err)
			os.Exit(2)
		}
		hold(os.Args[2], n, time.Duration(gap)*time.Millisecond)
	case "dial":
		ms, err := strconv.Atoi(os.Args[3])
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad timeout: %v\n", err)
			os.Exit(2)
		}
		dial(os.Args[2], time.Duration(ms)*time.Millisecond)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", os.Args[1])
		os.Exit(2)
	}
}

func serve(bind, banner string) {
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		fmt.Printf("SERVE_BIND_FAILED=%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("SERVE_LISTENING=%s\n", ln.Addr().String())
	os.Stdout.Sync()
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			io.WriteString(c, banner+"\n")
		}(c)
	}
}

// echo keeps every connection open and echoes each line back, so a caller can
// prove a flow still carries payload rather than merely holding an open fd.
func echo(bind string) {
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		fmt.Printf("ECHO_BIND_FAILED=%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("ECHO_LISTENING=%s\n", ln.Addr().String())
	os.Stdout.Sync()
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				if _, err := io.WriteString(c, sc.Text()+"\n"); err != nil {
					return
				}
			}
		}(c)
	}
}

// hold dials once, then sends n messages with gap between them, reading back the
// echo of each. Per-message OK/FAIL is the measurement: the write half can
// succeed on a flow that is already dead, the readback cannot.
func hold(addr string, n int, gap time.Duration) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		fmt.Printf("HOLD_DIAL_FAILED=%s ERR=%q\n", classify(err), err.Error())
		os.Exit(1)
	}
	defer c.Close()
	fmt.Printf("HOLD_ESTABLISHED=%s->%s\n", c.LocalAddr(), c.RemoteAddr())
	os.Stdout.Sync()
	r := bufio.NewReader(c)
	okCount, failCount := 0, 0
	for i := 1; i <= n; i++ {
		msg := fmt.Sprintf("msg-%d", i)
		if _, err := io.WriteString(c, msg+"\n"); err != nil {
			fmt.Printf("MSG=%d WRITE_FAILED=%s ERR=%q\n", i, classify(err), err.Error())
			failCount++
		} else {
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			line, rerr := r.ReadString('\n')
			switch {
			case rerr != nil:
				fmt.Printf("MSG=%d READBACK_FAILED=%s ERR=%q\n", i, classify(rerr), rerr.Error())
				failCount++
			case strings.TrimSpace(line) != msg:
				fmt.Printf("MSG=%d READBACK_MISMATCH got=%q want=%q\n", i, strings.TrimSpace(line), msg)
				failCount++
			default:
				fmt.Printf("MSG=%d OK\n", i)
				okCount++
			}
		}
		os.Stdout.Sync()
		if i < n {
			time.Sleep(gap)
		}
	}
	fmt.Printf("HOLD_DONE OK=%d FAIL=%d\n", okCount, failCount)
}

func dial(addr string, timeout time.Duration) {
	start := time.Now()
	c, err := net.DialTimeout("tcp", addr, timeout)
	el := time.Since(start)
	if err != nil {
		fmt.Printf("OUTCOME=%s ELAPSED_MS=%d ERR=%q\n", classify(err), el.Milliseconds(), err.Error())
		return
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 256)
	n, rerr := c.Read(buf)
	el = time.Since(start)
	if rerr != nil && n == 0 {
		fmt.Printf("OUTCOME=connected_no_payload ELAPSED_MS=%d ERR=%q\n", el.Milliseconds(), rerr.Error())
		return
	}
	fmt.Printf("OUTCOME=connected ELAPSED_MS=%d PAYLOAD=%q\n", el.Milliseconds(), string(buf[:n]))
}

// classify maps a dial error onto the distinction the spike is measuring.
// refused = an RST arrived (someone answered "no"); timeout = nothing answered
// at all (a silent drop); unreachable = an ICMP error arrived.
func classify(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "host_unreachable"
	case errors.Is(err, syscall.ENETUNREACH):
		return "net_unreachable"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return "prohibited"
	}
	return "other"
}
