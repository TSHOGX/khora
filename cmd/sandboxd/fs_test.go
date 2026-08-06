package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"connectrpc.com/connect"
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
		// The §3.4 capacity cap reaches the caller through this row: a full quota
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
