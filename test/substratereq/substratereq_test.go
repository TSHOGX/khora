package substratereq

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// These guards are the thing standing between "the substrate ran" and a green `ok` that
// ran nothing, so they are falsified rather than trusted — the same rule the recorded design
// applies to the structural guards. Each case drives the guard with a KNOWN-BAD input
// through a recorder and asserts it fails, and that its message names the fact a reader
// needs to fix the box.
//
// Deliberately NOT behind the `substrate` tag: a guard that only compiles under the tag
// it polices cannot be exercised by the cheap gate, and this file asserts the FAILURE
// paths, which need no substrate at all.

// rec captures a Fatalf without unwinding, which is enough to assert on: the guards call
// Fatalf exactly once and return immediately after.
type rec struct {
	testing.TB
	failed bool
	msg    string
}

func (r *rec) Helper() {}
func (r *rec) Fatalf(format string, args ...any) {
	r.failed = true
	r.msg = fmt.Sprintf(format, args...)
}

// fsType reports dir's filesystem as a short name, for deciding whether a case's
// known-bad input really is bad on THIS host.
func fsType(t *testing.T, dir string) string {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatalf("statfs %s: %v", dir, err)
	}
	if int64(st.Type) == btrfsSuperMagic {
		return "btrfs"
	}
	return fmt.Sprintf("%#x", st.Type)
}

func TestBtrfsRefusesANonBtrfsPathAndNamesIt(t *testing.T) {
	// /tmp is tmpfs on the dev box — the exact misconfiguration that made the fixtures
	// use /var/tmp, so it is the right known-bad input.
	if got := fsType(t, "/tmp"); got == "btrfs" {
		t.Skip("/tmp is btrfs on this host, so it is not a known-bad input here")
	}
	r := &rec{TB: t}
	Btrfs(r, "/tmp")
	if !r.failed {
		t.Fatal("Btrfs accepted a tmpfs path — a substrate suite would then run with no quota enforcement and report ok")
	}
	for _, want := range []string{"/tmp", "btrfs"} {
		if !strings.Contains(r.msg, want) {
			t.Errorf("failure message %q does not name %q; a reader cannot tell which path to fix", r.msg, want)
		}
	}
}

func TestBtrfsAcceptsTheScratchPathTheFixturesUse(t *testing.T) {
	if got := fsType(t, "/var/tmp"); got != "btrfs" {
		t.Skipf("/var/tmp is %s here, not btrfs; this assertion is about the dev box's shape", got)
	}
	r := &rec{TB: t}
	Btrfs(r, "/var/tmp")
	if r.failed {
		t.Fatalf("Btrfs rejected /var/tmp, which IS btrfs here: %s", r.msg)
	}
}

func TestBtrfsFailsOnAMissingPathRatherThanPassing(t *testing.T) {
	r := &rec{TB: t}
	Btrfs(r, "/nonexistent-substrate-probe-path")
	if !r.failed {
		t.Fatal("Btrfs accepted a path that does not exist — statfs failure must be a finding, not a pass")
	}
}

func TestBinReportsEveryMissingNameAtOnce(t *testing.T) {
	r := &rec{TB: t}
	Bin(r, "definitely-not-a-real-binary-a", "sh", "definitely-not-a-real-binary-b")
	if !r.failed {
		t.Fatal("Bin accepted two absent executables")
	}
	for _, want := range []string{"definitely-not-a-real-binary-a", "definitely-not-a-real-binary-b"} {
		if !strings.Contains(r.msg, want) {
			t.Errorf("message %q omits %q — reporting one missing name per run turns provisioning into a guessing loop", r.msg, want)
		}
	}
	if strings.Contains(r.msg, `"sh"`) {
		t.Errorf("message %q names sh, which is present", r.msg)
	}
}

func TestBinPassesWhenEveryNameResolves(t *testing.T) {
	r := &rec{TB: t}
	Bin(r, "sh")
	if r.failed {
		t.Fatalf("Bin rejected sh, which is on PATH: %s", r.msg)
	}
}

func TestRootAgreesWithTheProcessItIsRunningIn(t *testing.T) {
	r := &rec{TB: t}
	Root(r)
	// Asserted against the real euid rather than one branch: the cheap gate runs
	// unrooted and the substrate gate runs rooted, so this test must pin BOTH and pins
	// whichever it is in.
	if os.Geteuid() == 0 && r.failed {
		t.Fatalf("Root failed as root: %s", r.msg)
	}
	if os.Geteuid() != 0 && !r.failed {
		t.Fatal("Root passed as non-root — this is the check that stops a substrate suite reporting ok having skipped everything")
	}
}
