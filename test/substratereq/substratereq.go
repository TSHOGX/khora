// Package substratereq states the preconditions a `substrate`-tagged test needs and
// FAILS when they are absent. It is the answer to a measured hazard: `ok` from a
// substrate package used to mean either "the substrate was exercised" or "every test
// in here skipped", and the two were indistinguishable — `go test -tags substrate
// ./internal/btrfs/` printed `ok` in 0.007s having run nothing.
//
// FAIL, NOT SKIP. This repo has one development box and the maintainer is root on it,
// so a skip branch serves an environment that does not exist and its only effect is to
// manufacture green. A missing precondition here is a misconfigured machine, which is a
// finding, not an exemption.
//
// This does NOT touch the `substrate` build tag, which is a different axis and stays:
// the tag is about COST (the cheap gate runs in seconds, this suite in minutes) and it
// is what makes the fast loop possible. Privilege is the axis being collapsed.
//
// LEAF BY NECESSITY, not by preference: it imports nothing from the engine, because
// `test/harness` imports internal/runtime and internal/btrfs, so any guard living there
// is unreachable from those packages' own tests (an import cycle). The btrfs check is
// therefore a raw statfs rather than a call to internal/btrfs.
package substratereq

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

// btrfsSuperMagic identifies a btrfs filesystem in statfs(2)'s f_type.
// linux/magic.h: BTRFS_SUPER_MAGIC.
const btrfsSuperMagic = 0x9123683e

// Main is the reporter to pass from a TestMain, which has no *testing.T. It prints the
// same message and exits non-zero, so a precondition stated once per package fails the
// whole package instead of each test restating it.
//
// Exit code 1 rather than a panic: `go test` renders a panic as a crash with a stack
// nobody needs, while a plain message plus a non-zero exit is what a CI log should show
// for "this box is not set up".
var Main testing.TB = mainReporter{}

type mainReporter struct{ testing.TB }

func (mainReporter) Helper() {}
func (mainReporter) Fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "substrate precondition: "+format+"\n", args...)
	os.Exit(1)
}

// Root fails unless the process is root. Every substrate capability this repo uses
// needs it: runsc --network=sandbox refuses --rootless (the recorded design Privilege), an
// overlayfs mount needs CAP_SYS_ADMIN, and the btrfs quota ioctls need it too.
func Root(t testing.TB) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("substrate test needs root and this process is euid %d — "+
			"run: sudo env \"PATH=$PATH\" \"HOME=$HOME\" go test -tags substrate -count=1 -p 1 ./...",
			os.Geteuid())
	}
}

// Btrfs fails unless dir is on a btrfs filesystem. Disk caps ARE btrfs quotas, so a
// non-btrfs scratch path does not degrade the test, it removes the thing under test.
// Names the path and its actual type: /tmp on the dev box is tmpfs, which is why the
// fixtures use /var/tmp, and "needs btrfs" alone sends the reader looking in the wrong
// place.
func Btrfs(t testing.TB, dir string) {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatalf("substrate test needs %s on btrfs; statfs failed: %v", dir, err)
	}
	if int64(st.Type) != btrfsSuperMagic {
		t.Fatalf("substrate test needs %s on btrfs (disk caps are btrfs quotas, the recorded design); "+
			"its f_type is %#x, not btrfs's %#x", dir, st.Type, btrfsSuperMagic)
	}
}

// Bin fails unless every named executable is on PATH, reporting ALL the missing ones
// rather than the first: a fresh box is usually missing several, and one name per run
// turns provisioning into a guessing loop.
func Bin(t testing.TB, names ...string) {
	t.Helper()
	var missing []string
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("substrate test needs these on PATH and they are absent: %v (the recorded design lists why each is hard)", missing)
	}
}
