package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/TSHOGX/khora/pkg/transport/gen/khora/transport/v1"
)

// The errno→code table is the fs service's whole error contract, and a wrong row
// is invisible in normal use (the call still fails, just with a code that misleads
// the caller into the wrong recovery). So every row is pinned.
func TestFsErrMapsEveryErrno(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want connect.Code
	}{
		{"ENOENT", syscall.ENOENT, connect.CodeNotFound},
		{"fs.ErrNotExist", fs.ErrNotExist, connect.CodeNotFound},
		{"EEXIST", syscall.EEXIST, connect.CodeAlreadyExists},
		{"fs.ErrExist", fs.ErrExist, connect.CodeAlreadyExists},
		{"EACCES", syscall.EACCES, connect.CodePermissionDenied},
		{"EPERM", syscall.EPERM, connect.CodePermissionDenied},
		// The resource admission capacity cap reaches the caller through this row: a full quota
		// must read as resource-exhausted, not as an engine fault.
		{"ENOSPC", syscall.ENOSPC, connect.CodeResourceExhausted},
		{"EDQUOT", syscall.EDQUOT, connect.CodeResourceExhausted},
		{"ENOTEMPTY", syscall.ENOTEMPTY, connect.CodeFailedPrecondition},
		{"EISDIR", syscall.EISDIR, connect.CodeFailedPrecondition},
		// EXDEV is reachable in ordinary use here (overlay upper + tmpfs + volume
		// mounts in one namespace) and must never fall through to internal.
		{"EXDEV", syscall.EXDEV, connect.CodeFailedPrecondition},
		{"EINVAL", syscall.EINVAL, connect.CodeInvalidArgument},
		{"ENAMETOOLONG", syscall.ENAMETOOLONG, connect.CodeInvalidArgument},
		{"ENOTDIR", syscall.ENOTDIR, connect.CodeInvalidArgument},
		{"ELOOP", syscall.ELOOP, connect.CodeInvalidArgument},
		{"unknown", errors.New("something else"), connect.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fsErr("op", "/some/path", tc.err)
			if got == nil {
				t.Fatal("fsErr returned nil for a non-nil error")
			}
			if code := connect.CodeOf(got); code != tc.want {
				t.Errorf("fsErr(%v) code = %v; want %v", tc.err, code, tc.want)
			}
			// The op and path must survive into the message: without them a
			// not-found tells the caller nothing about WHAT was not found.
			if !strings.Contains(got.Error(), "/some/path") {
				t.Errorf("fsErr message lost the path: %q", got.Error())
			}
		})
	}
	if fsErr("op", "/p", nil) != nil {
		t.Error("fsErr(nil) must be nil — a nil error is not a failure")
	}
}

func TestCollectWatchChangeDetection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := collectWatch(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("initial snapshot = %+v", first)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := collectWatch(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if second[filepath.Join(dir, "a")].size != 3 || second[filepath.Join(dir, "b")].type_ != v1.EntryType_ENTRY_TYPE_FILE {
		t.Fatalf("updated snapshot = %+v", second)
	}
}

// EXDEV gets a bespoke message because "invalid cross-device link" alone sends the
// reader looking for a bug instead of telling them what to do instead.
func TestFsErrExdevExplainsTheRemedy(t *testing.T) {
	msg := fsErr("move", "/dst", syscall.EXDEV).Error()
	if !strings.Contains(msg, "copy then remove") {
		t.Errorf("EXDEV message should name the remedy; got %q", msg)
	}
}

// depth is the one parameter of List with real logic, and getting it wrong is
// either a silent under-read (missing entries) or an unbounded filesystem walk.
func TestListDirDepth(t *testing.T) {
	root := t.TempDir()
	// root/a/b/c.txt + root/top.txt — three levels below root.
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "c.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "top.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	names := func(depth int) []string {
		t.Helper()
		entries, err := listDir(root, depth)
		if err != nil {
			t.Fatalf("listDir depth=%d: %v", depth, err)
		}
		var got []string
		for _, e := range entries {
			rel, err := filepath.Rel(root, e.Path)
			if err != nil {
				t.Fatalf("entry path %q is not under root %q", e.Path, root)
			}
			got = append(got, rel)
		}
		return got
	}

	// depth 1 = immediate children only.
	d1 := names(1)
	if len(d1) != 2 {
		t.Errorf("depth 1 returned %v; want exactly the 2 immediate children", d1)
	}
	// depth 2 adds a/b but NOT a/b/c.txt.
	d2 := names(2)
	if len(d2) != 3 {
		t.Errorf("depth 2 returned %v; want 3 (top.txt, a, a/b)", d2)
	}
	// depth 3 reaches the leaf. Flattened, so the leaf appears at top level of the
	// result with its full path — that is what makes a flat list unambiguous.
	d3 := names(3)
	foundLeaf := false
	for _, n := range d3 {
		if n == filepath.Join("a", "b", "c.txt") {
			foundLeaf = true
		}
	}
	if !foundLeaf {
		t.Errorf("depth 3 did not reach a/b/c.txt; got %v", d3)
	}
	// Deeper than the tree is not an error, it just stops.
	if got := names(9); len(got) != len(d3) {
		t.Errorf("depth 9 returned %d entries; want the same %d as depth 3", len(got), len(d3))
	}
}

// Stat and List must describe the same path identically — they share one
// constructor precisely so they cannot drift, and a symlink is the case where a
// drift would show up first (one side following it, the other not).
func TestEntryInfoSymlinkIsLstatAndAbsolutized(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	// A RELATIVE target, to prove it gets absolutized: "target.txt" means nothing
	// to a caller outside the guest.
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	e := entryInfo(link, fi)
	if e.Type.String() != "ENTRY_TYPE_SYMLINK" {
		t.Errorf("symlink reported as %v; want SYMLINK (lstat, not stat)", e.Type)
	}
	if e.SymlinkTarget == nil {
		t.Fatal("symlink_target not set on a symlink")
	}
	if *e.SymlinkTarget != target {
		t.Errorf("symlink_target = %q; want the absolutized %q", *e.SymlinkTarget, target)
	}

	// And the same path via the List walk agrees.
	entries, err := listDir(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, le := range entries {
		if le.Path != link {
			continue
		}
		if le.Type != e.Type || le.SymlinkTarget == nil || *le.SymlinkTarget != *e.SymlinkTarget {
			t.Errorf("List and Stat disagree on %s: list=%+v stat=%+v", link, le, e)
		}
		return
	}
	t.Errorf("List did not return the symlink at all; got %d entries", len(entries))
}

// `length` off the wire is an UPPER BOUND, not an allocation size. Pre-sizing a
// buffer from it let one number drive a guest-side allocation of arbitrary size,
// and the sandbox is TERMINATED on exceeding its RAM cap (resource admission) — so this is the
// transport behavior "neither end holds the whole thing" security property, at the RPC.
//
// Asserted two ways because either alone is passable by a wrong fix: the returned
// length (a size-clamp would also satisfy this) AND total bytes allocated, which
// only a bound-not-preallocate fix satisfies. HeapAlloc deltas are noisy, so the
// threshold is orders of magnitude below the 16 GiB the old code took for 1<<34
// and far above ordinary test churn.
func TestFsReadHugeLengthDoesNotPreallocate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "small")
	if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var before, after goruntime.MemStats
	goruntime.GC()
	goruntime.ReadMemStats(&before)

	resp, err := (&fsServer{}).Read(context.Background(), connect.NewRequest(&v1.ReadRequest{
		Path: p, Length: 1 << 34, // 16 GiB, on a 10-byte file
	}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	goruntime.ReadMemStats(&after)

	if got := string(resp.Msg.Data); got != "0123456789" {
		t.Fatalf("Read data = %q, want %q", got, "0123456789")
	}
	const cap = 64 << 20 // 64 MiB: ~256x under 16 GiB, way over test churn
	if grew := after.TotalAlloc - before.TotalAlloc; grew > cap {
		t.Fatalf("Read(length=1<<34) allocated %d bytes on a 10-byte file; "+
			"length must bound the read, not size the buffer", grew)
	}
}

// A length larger than int64 slice range PANICs in makeslice if it reaches there before
// a bound is applied — a request that kills the transport rather than one that errors.
func TestFsReadAbsurdLengthDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "small")
	if err := os.WriteFile(p, []byte("hi"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	resp, err := (&fsServer{}).Read(context.Background(), connect.NewRequest(&v1.ReadRequest{
		Path: p, Length: 1 << 50,
	}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(resp.Msg.Data); got != "hi" {
		t.Fatalf("Read data = %q, want %q", got, "hi")
	}
}

// THE GUARD ON THE FIX ITSELF. The tempting bound is min(length, Size()), and it
// is WRONG: measured, /proc/self/status reports st_size 0 while yielding >1 KB.
// Clamping by size reads every such file as EMPTY — a silent wrong answer, worse
// than the allocation it replaces. Nothing else in the suite would catch it, since
// regular-file tests all have honest sizes.
func TestFsReadProcFileWithZeroStatSize(t *testing.T) {
	const p = "/proc/self/status"
	fi, err := os.Stat(p)
	if err != nil {
		t.Skipf("no %s on this platform: %v", p, err)
	}
	if fi.Size() != 0 {
		t.Skipf("%s reports size %d, not 0 — this guard needs a size-0 file", p, fi.Size())
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("baseline read: %v", err)
	}
	resp, err := (&fsServer{}).Read(context.Background(), connect.NewRequest(&v1.ReadRequest{
		Path: p, Length: 1 << 20,
	}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n := len(resp.Msg.Data); n == 0 {
		t.Fatalf("Read(%s) returned 0 bytes though the file yields %d: "+
			"length must not be clamped by a reported size of 0", p, len(want))
	}
	if !strings.Contains(string(resp.Msg.Data), "Name:") {
		t.Fatalf("Read(%s) = %q; want the real content", p, resp.Msg.Data)
	}
}

// length still truncates when it is genuinely smaller than the file, and offset
// composes with it — the ordinary contract the bound must not have broken.
func TestFsReadLengthTruncatesAndComposesWithOffset(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("abcdefghij"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	for _, tc := range []struct {
		off, length int64
		want        string
	}{
		{0, 4, "abcd"},
		{6, 4, "ghij"},
		{6, 100, "ghij"},     // length past EOF is not an error
		{0, 0, "abcdefghij"}, // length<=0 = read to EOF
	} {
		resp, err := (&fsServer{}).Read(context.Background(), connect.NewRequest(&v1.ReadRequest{
			Path: p, Offset: tc.off, Length: tc.length,
		}))
		if err != nil {
			t.Fatalf("Read(off=%d,len=%d): %v", tc.off, tc.length, err)
		}
		if got := string(resp.Msg.Data); got != tc.want {
			t.Fatalf("Read(off=%d,len=%d) = %q, want %q", tc.off, tc.length, got, tc.want)
		}
	}
}
