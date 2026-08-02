// qgprobe — a static, dependency-free probe binary used inside a runsc guest by
// spike-qgroup-enospc.sh. It exists because the interesting answers are *exact
// errnos* and *exact byte counts*: `dd` stringifies errno and coreutils are
// absent from the minimal guest rootfs, so a self-contained Go binary is the only
// way to report `errno=28 ENOSPC` unambiguously from guest userspace.
//
// Every subcommand prints `KEY=value` lines to stdout and exits 0 even on the
// errors it probes (the error IS the datum); non-zero exit means harness fault.
//
// Subcommands:
//
//	idle                      block forever (PID1 keep-alive for a detached container)
//	fill   <path> <blockKiB>  write blocks until write(2) fails; report bytes+errno
//	poke   <path>             open O_WRONLY + write 1 byte at offset 0 (forces overlayfs copy-up)
//	md5    <path>             size + md5 of full read-back (detects truncation/corruption)
//	sparse <path> <sizeMiB>   ftruncate to sizeMiB (sparse), then report
//	falloc <path> <sizeMiB>   fallocate(2) sizeMiB, report errno
//	holes  <path> <stepMiB> <count>  write 1 byte every stepMiB into a sparse file
//	link   <src> <dst>        hardlink(2), report errno
//	rename <src> <dst>        rename(2), report errno
//	stat   <path>             size / blocks / mode
//	ls     <dir>              entry names (guest-side dir listing without coreutils)
//	echo   <text>             liveness marker
package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"
)

func errnoOf(err error) (int, string) {
	if err == nil {
		return 0, "OK"
	}
	e := err
	for i := 0; i < 8 && e != nil; i++ {
		if en, ok := e.(syscall.Errno); ok {
			return int(en), en.Error()
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	return -1, err.Error()
}

func report(tag string, err error) {
	n, s := errnoOf(err)
	fmt.Printf("%s_ERRNO=%d\n%s_ERRSTR=%s\n", tag, n, tag, s)
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qgprobe: bad number %q\n", s)
		os.Exit(2)
	}
	return n
}

// fill writes fixed-size blocks until a write fails, then reports how many bytes
// landed and with what errno. It fsyncs at the end so a delayed-allocation cap
// can still surface (btrfs qgroup enforcement is at reserve time, but ENOSPC can
// also come back only on fsync/close in some stacks — report all three).
func fill(path string, blockKiB int) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	report("OPEN", err)
	if err != nil {
		// Still emit FILL_BYTES so callers always have the byte count: an
		// already-exhausted cap refuses the O_CREATE open itself, and "0 bytes"
		// is the answer, not a missing field.
		fmt.Println("FILL_BYTES=0")
		return
	}
	buf := make([]byte, blockKiB*1024)
	for i := range buf {
		buf[i] = byte('A' + i%26)
	}
	var total int64
	var werr error
	const maxBytes = 4 << 30 // hard stop so a missing cap cannot fill the host
	for total < maxBytes {
		n, err := f.Write(buf)
		total += int64(n)
		if err != nil {
			werr = err
			break
		}
	}
	fmt.Printf("FILL_BYTES=%d\n", total)
	report("WRITE", werr)
	report("FSYNC", f.Sync())
	report("CLOSE", f.Close())
	if fi, err := os.Stat(path); err == nil {
		fmt.Printf("FILL_STATSIZE=%d\n", fi.Size())
	}
	if total >= maxBytes {
		fmt.Println("FILL_VERDICT=NO_CAP_HIT_4GiB_GUARD")
	}
}

// poke forces an overlayfs copy-up of a lower file: O_WRONLY open (copy-up
// happens in the open path) then a 1-byte write at offset 0.
func poke(path string) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	report("OPEN", err)
	if err != nil {
		return
	}
	n, werr := f.WriteAt([]byte{0x5A}, 0)
	fmt.Printf("POKE_WROTE=%d\n", n)
	report("WRITE", werr)
	report("FSYNC", f.Sync())
	report("CLOSE", f.Close())
}

func md5of(path string) {
	f, err := os.Open(path)
	if err != nil {
		report("OPEN", err)
		return
	}
	defer f.Close()
	h := md5.New()
	n, cerr := io.Copy(h, f)
	fmt.Printf("MD5_READBYTES=%d\nMD5_SUM=%x\n", n, h.Sum(nil))
	report("READ", cerr)
	if fi, serr := f.Stat(); serr == nil {
		fmt.Printf("MD5_STATSIZE=%d\n", fi.Size())
	}
}

func sparse(path string, sizeMiB int) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		report("OPEN", err)
		return
	}
	report("TRUNCATE", f.Truncate(int64(sizeMiB)<<20))
	report("CLOSE", f.Close())
	statLine("SPARSE", path)
}

func falloc(path string, sizeMiB int) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		report("OPEN", err)
		return
	}
	err = syscall.Fallocate(int(f.Fd()), 0, 0, int64(sizeMiB)<<20)
	report("FALLOCATE", err)
	report("CLOSE", f.Close())
	statLine("FALLOC", path)
}

// holes writes one byte per stepMiB into an existing (sparse) file: the cheapest
// way to allocate scattered real blocks and see whether they hit the cap.
func holes(path string, stepMiB, count int) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		report("OPEN", err)
		return
	}
	var wrote int
	var werr error
	for i := 0; i < count; i++ {
		if _, err := f.WriteAt([]byte{0x42}, int64(i)*int64(stepMiB)<<20); err != nil {
			werr = err
			break
		}
		wrote++
	}
	fmt.Printf("HOLES_WRITTEN=%d\n", wrote)
	report("WRITE", werr)
	report("FSYNC", f.Sync())
	report("CLOSE", f.Close())
}

func statLine(tag, path string) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		report(tag+"_STAT", err)
		return
	}
	fmt.Printf("%s_SIZE=%d\n%s_BLOCKS=%d\n%s_ALLOCBYTES=%d\n%s_MODE=%o\n",
		tag, st.Size, tag, st.Blocks, tag, st.Blocks*512, tag, st.Mode&0o7777)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qgprobe <subcommand> [args]")
		os.Exit(2)
	}
	a := os.Args
	switch a[1] {
	case "idle":
		fmt.Println("QGPROBE_IDLE_READY")
		for {
			time.Sleep(time.Hour)
		}
	case "fill":
		fill(a[2], atoi(a[3]))
	case "poke":
		poke(a[2])
	case "md5":
		md5of(a[2])
	case "sparse":
		sparse(a[2], atoi(a[3]))
	case "falloc":
		falloc(a[2], atoi(a[3]))
	case "holes":
		holes(a[2], atoi(a[3]), atoi(a[4]))
	case "link":
		report("LINK", os.Link(a[2], a[3]))
		statLine("LINK", a[3])
	case "rename":
		report("RENAME", os.Rename(a[2], a[3]))
	case "stat":
		statLine("STAT", a[2])
	case "ls":
		ents, err := os.ReadDir(a[2])
		report("READDIR", err)
		for _, e := range ents {
			fmt.Printf("ENT=%s\n", e.Name())
		}
	case "echo":
		fmt.Printf("ECHO=%s\n", a[2])
	default:
		fmt.Fprintf(os.Stderr, "qgprobe: unknown subcommand %q\n", a[1])
		os.Exit(2)
	}
}
