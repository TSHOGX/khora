//go:build substrate

// L2 for the §5.C local arm against REAL storage. Requires root (unit + quota
// ioctls). The engine scratch fs is btrfs in squota mode (ADR-0002 /
// rootfs-overlay D6.2).
//
//	sudo env PATH=$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOSUMDB=off HOME=$HOME \
//	  go test -tags substrate -v -count=1 -p 1 ./internal/volume/
package volume_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TSHOGX/warren/internal/volume"
	"github.com/TSHOGX/warren/internal/volume/volumetest"
	"github.com/TSHOGX/warren/test/substratereq"
)

// realScratch makes a scratch dir on a real quota-capable filesystem. t.TempDir()
// is NOT usable: TMPDIR is /tmp = tmpfs here, where the storage units this arm
// needs cannot exist. The engine has the same constraint in production (ADR-0002:
// scratch must be on native btrfs), so testing on tmpfs would test a configuration
// the engine does not support.
func realScratch(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "volume-l2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The units are btrfs subvolumes and os.RemoveAll CANNOT delete one (rmdir(2)
		// refuses it), so they go through the tool first. Cleanup REPORTS its own
		// failures: reclamation is part of what this suite tests, so a scratch dir that
		// silently survives costs the ability to tell "the engine leaks" from "my
		// fixtures leak".
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			// Contents first: `subvolume delete` addresses the unit itself, while its
			// children are ordinary dirs inside it.
			inner, _ := os.ReadDir(p)
			for _, ie := range inner {
				_ = os.RemoveAll(filepath.Join(p, ie.Name()))
			}
			out, err := exec.Command("btrfs", "subvolume", "delete", p).CombinedOutput()
			if err != nil && !strings.Contains(string(out), "not a subvolume") {
				_ = os.RemoveAll(p)
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("cleanup scratch %s: %v", dir, err)
		}
	})
	return dir
}

// TestLocalBtrfsContract runs the SAME shared seam contract the plain-directory arm
// runs (volume.md D2: both arms, one set of assertions). This is the half that
// exercises real capacity enforcement — Capabilities().Quota is true here, so the
// suite's cap subtests take their capped branch and assert the cap actually reaches
// the storage rather than being recorded and forgotten.
func TestLocalBtrfsContract(t *testing.T) {
	substratereq.Root(t)
	volumetest.RunContract(t, func(t *testing.T) volume.Backend {
		b, err := volume.NewLocalBtrfs(filepath.Join(realScratch(t), "volumes"))
		if err != nil {
			t.Fatalf("new local arm: %v", err)
		}
		if !b.Capabilities().Quota {
			t.Fatalf("the local arm reports no Quota capability — the scratch dir is not on a quota-capable fs, so this run would silently exercise the uncapped branch")
		}
		return b
	})
}

// TestCapIsEnforcedNotJustRecorded pins the property the whole capacity axis rests
// on, and it cannot be checked on the plain arm: past the cap a write gets a PLAIN
// ENOSPC (§3.4), rather than the cap being a number in a record that nothing
// enforces.
//
// This is the negative control for the failure mode volume.md D7.3 calls silent and
// undetectable-after-the-fact: an engine that recorded size=N but never set the cap
// would let a writer pour in far more than N with zero errors, and Usage()'s limit
// read cannot tell that apart from a deliberately uncapped volume.
func TestCapIsEnforcedNotJustRecorded(t *testing.T) {
	substratereq.Root(t)
	ctx := context.Background()
	b, err := volume.NewLocalBtrfs(filepath.Join(realScratch(t), "volumes"))
	if err != nil {
		t.Fatalf("new local arm: %v", err)
	}
	const cap = 8 << 20
	if err := b.CreateVolume(ctx, "vol_cap", cap); err != nil {
		t.Fatalf("create capped: %v", err)
	}
	r, err := b.OpenRoot(ctx, "vol_cap")
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer r.Close()

	// Write well past the cap in chunks. The cap must bite; where exactly it bites is
	// not asserted, because ENOSPC is NOT A STICKY WALL on this substrate (§5.C: the
	// next small write after hitting it sometimes still succeeds). What is asserted is
	// that a write eventually fails, and that the total landed is bounded near the cap.
	chunk := make([]byte, 1<<20)
	var landed int64
	var sawENOSPC bool
	for i := 0; i < 32; i++ {
		n, err := r.WriteAt("blob", chunk, landed)
		landed += int64(n)
		if err != nil {
			sawENOSPC = true
			break
		}
	}
	if !sawENOSPC {
		t.Fatalf("wrote %d bytes into an %d-byte capped volume with no error — the cap was recorded but NOT ENFORCED, which is silent and undetectable afterwards (volume.md D7.3)", landed, cap)
	}
	if landed > 2*cap {
		t.Errorf("landed %d bytes under an %d-byte cap — enforcement is far looser than the declared bound", landed, cap)
	}
	t.Logf("cap %d bytes: write failed after %d bytes landed", cap, landed)
}
