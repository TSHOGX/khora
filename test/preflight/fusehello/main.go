// fusehello — a minimal read-only FUSE filesystem spoken directly over
// /dev/fuse using only the Go stdlib (syscall + encoding/binary), so it builds
// with GO111MODULE=off and no external module. It exists solely for
// spike-fuse-passthrough.sh (the documented invariant remote arm):
// mount it on the HOST, bind it into a runsc guest, and prove gVisor's gofer
// proxies a FUSE-backed mountpoint into the sandbox as a normal POSIX dir.
//
// Layout it serves:
//
//	/from_fuse        -> "FUSE_BACKED_CONTENT\n"   (node 2, regular)
//	/subdir/          -> dir                        (node 3)
//	/subdir/nested    -> "FUSE_NESTED\n"            (node 4, regular)
//
// Usage: fusehello <mountpoint>    (must run as root; mounts fstype "fuse")
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
)

// opcodes (linux uapi fuse.h)
const (
	opLookup      = 1
	opForget      = 2
	opGetattr     = 3
	opReadlink    = 5
	opOpen        = 14
	opRead        = 15
	opStatfs      = 17
	opRelease     = 18
	opGetxattr    = 22
	opListxattr   = 23
	opFlush       = 25
	opInit        = 26
	opOpendir     = 27
	opReaddir     = 28
	opReleasedir  = 29
	opAccess      = 34
	opInterrupt   = 36
	opBatchForget = 42
)

const (
	sIFDIR = 0o040000
	sIFREG = 0o100000
	dtDir  = 4
	dtReg  = 8
)

var (
	content1 = []byte("FUSE_BACKED_CONTENT\n")
	content2 = []byte("FUSE_NESTED\n")
)

type node struct {
	mode uint32
	size uint64
	dir  bool
	data []byte
}

var nodes = map[uint64]node{
	1: {mode: sIFDIR | 0o555, size: 4096, dir: true},
	2: {mode: sIFREG | 0o444, size: uint64(len(content1)), data: content1},
	3: {mode: sIFDIR | 0o555, size: 4096, dir: true},
	4: {mode: sIFREG | 0o444, size: uint64(len(content2)), data: content2},
}

type dirent struct {
	ino  uint64
	name string
	typ  uint32
}

var dirs = map[uint64][]dirent{
	1: {{2, "from_fuse", dtReg}, {3, "subdir", dtDir}},
	3: {{4, "nested", dtReg}},
}

func lookupChild(parent uint64, name string) (uint64, bool) {
	for _, e := range dirs[parent] {
		if e.name == name {
			return e.ino, true
		}
	}
	return 0, false
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fusehello <mountpoint>")
		os.Exit(2)
	}
	mnt := os.Args[1]

	fd, err := syscall.Open("/dev/fuse", syscall.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open /dev/fuse:", err)
		os.Exit(1)
	}
	data := fmt.Sprintf("fd=%d,rootmode=40000,user_id=0,group_id=0", fd)
	if err := syscall.Mount("fusehello", mnt, "fuse", syscall.MS_NOSUID|syscall.MS_NODEV, data); err != nil {
		fmt.Fprintln(os.Stderr, "mount fuse:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "fusehello: mounted at", mnt)

	buf := make([]byte, 1<<20+4096)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			fmt.Fprintln(os.Stderr, "read /dev/fuse:", err)
			return
		}
		if n < 40 {
			continue
		}
		handle(fd, buf[:n])
	}
}

func handle(fd int, req []byte) {
	// fuse_in_header: len u32, opcode u32, unique u64, nodeid u64, uid,gid,pid,pad u32
	opcode := binary.LittleEndian.Uint32(req[4:8])
	unique := binary.LittleEndian.Uint64(req[8:16])
	nodeid := binary.LittleEndian.Uint64(req[16:24])
	body := req[40:]

	switch opcode {
	case opInit:
		// fuse_init_in: major,minor,max_readahead,flags (we only need major/minor)
		kmajor := binary.LittleEndian.Uint32(body[0:4])
		kminor := binary.LittleEndian.Uint32(body[4:8])
		_ = kmajor
		out := make([]byte, 64)
		binary.LittleEndian.PutUint32(out[0:4], 7) // major
		min := kminor
		if min > 36 {
			min = 36
		}
		binary.LittleEndian.PutUint32(out[4:8], min)     // minor
		binary.LittleEndian.PutUint32(out[8:12], 0)      // max_readahead
		binary.LittleEndian.PutUint32(out[12:16], 0)     // flags (no features)
		binary.LittleEndian.PutUint32(out[24:28], 1<<17) // max_write = 128K
		reply(fd, unique, 0, out)
	case opGetattr:
		reply(fd, unique, 0, attrOut(nodeid))
	case opLookup:
		name := cstr(body)
		child, ok := lookupChild(nodeid, name)
		if !ok {
			reply(fd, unique, -int32(syscall.ENOENT), nil)
			return
		}
		reply(fd, unique, 0, entryOut(child))
	case opOpen, opOpendir:
		out := make([]byte, 16) // fuse_open_out: fh u64, open_flags u32, padding u32
		binary.LittleEndian.PutUint64(out[0:8], nodeid)
		reply(fd, unique, 0, out)
	case opRead:
		// fuse_read_in: fh u64, offset u64, size u32, ...
		off := binary.LittleEndian.Uint64(body[8:16])
		size := binary.LittleEndian.Uint32(body[16:20])
		n := nodes[nodeid]
		var out []byte
		if int(off) < len(n.data) {
			end := off + uint64(size)
			if end > uint64(len(n.data)) {
				end = uint64(len(n.data))
			}
			out = n.data[off:end]
		}
		reply(fd, unique, 0, out)
	case opReaddir:
		off := binary.LittleEndian.Uint64(body[8:16])
		size := binary.LittleEndian.Uint32(body[16:20])
		reply(fd, unique, 0, readdir(nodeid, off, size))
	case opAccess, opFlush, opRelease, opReleasedir:
		reply(fd, unique, 0, nil)
	case opGetxattr, opListxattr:
		reply(fd, unique, -int32(syscall.ENODATA), nil)
	case opStatfs:
		reply(fd, unique, 0, make([]byte, 80)) // fuse_kstatfs zeros
	case opForget, opBatchForget, opInterrupt:
		// no reply expected
	default:
		reply(fd, unique, -int32(syscall.ENOSYS), nil)
	}
}

// attr writes an 88-byte fuse_attr for nodeid into b. Field offsets (linux uapi
// fuse.h): ino0 size8 blocks16 atime24 mtime32 ctime40 atimensec48 mtimensec52
// ctimensec56 mode60 nlink64 uid68 gid72 rdev76 blksize80 flags84.
func attr(b []byte, nodeid uint64) {
	n := nodes[nodeid]
	binary.LittleEndian.PutUint64(b[0:8], nodeid) // ino
	binary.LittleEndian.PutUint64(b[8:16], n.size)
	binary.LittleEndian.PutUint64(b[16:24], (n.size+511)/512) // blocks
	// atime/mtime/ctime (sec + nsec) left zero
	binary.LittleEndian.PutUint32(b[60:64], n.mode)
	nlink := uint32(1)
	if n.dir {
		nlink = 2
	}
	binary.LittleEndian.PutUint32(b[64:68], nlink)
	binary.LittleEndian.PutUint32(b[80:84], 4096) // blksize
}

// fuse_attr_out: attr_valid u64, attr_valid_nsec u32, dummy u32, attr(88) = 104
func attrOut(nodeid uint64) []byte {
	out := make([]byte, 104)
	attr(out[16:], nodeid)
	return out
}

// fuse_entry_out: nodeid u64, generation u64, entry_valid u64, attr_valid u64,
// entry_valid_nsec u32, attr_valid_nsec u32 (=40 hdr), attr(88) = 128
func entryOut(nodeid uint64) []byte {
	out := make([]byte, 128)
	binary.LittleEndian.PutUint64(out[0:8], nodeid)
	attr(out[40:], nodeid)
	return out
}

// readdir emits fuse_dirent entries starting at logical offset `off`.
func readdir(nodeid uint64, off uint64, size uint32) []byte {
	entries := dirs[nodeid]
	var out []byte
	for i := int(off); i < len(entries); i++ {
		e := entries[i]
		name := []byte(e.name)
		reclen := 24 + len(name)
		pad := (8 - reclen%8) % 8
		if len(out)+reclen+pad > int(size) {
			break
		}
		rec := make([]byte, reclen+pad)
		binary.LittleEndian.PutUint64(rec[0:8], e.ino)
		binary.LittleEndian.PutUint64(rec[8:16], uint64(i+1)) // next offset
		binary.LittleEndian.PutUint32(rec[16:20], uint32(len(name)))
		binary.LittleEndian.PutUint32(rec[20:24], e.typ)
		copy(rec[24:], name)
		out = append(out, rec...)
	}
	return out
}

// reply writes a fuse_out_header + payload back to /dev/fuse.
func reply(fd int, unique uint64, errno int32, data []byte) {
	out := make([]byte, 16+len(data))
	binary.LittleEndian.PutUint32(out[0:4], uint32(len(out)))
	binary.LittleEndian.PutUint32(out[4:8], uint32(errno))
	binary.LittleEndian.PutUint64(out[8:16], unique)
	copy(out[16:], data)
	_, _ = syscall.Write(fd, out)
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
