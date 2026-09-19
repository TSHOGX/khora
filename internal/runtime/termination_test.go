package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeRunsc(t *testing.T, script string) *LocalRunsc {
	t.Helper()
	root := t.TempDir()
	binary := filepath.Join(root, "runsc")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := NewLocalRunsc(filepath.Join(root, "state"), filepath.Join(root, "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	p.RunscBin = binary
	return p
}
func TestContainerInspectionDoesNotInventDeath(t *testing.T) {
	for name, script := range map[string]string{
		"command failure":                 "exit 1\n",
		"malformed state":                 "case \" $* \" in *' state '*) echo not-json;; *) echo '[]';; esac\n",
		"missing inventory identity":      "case \" $* \" in *' state '*) exit 1;; *) echo '[{}]';; esac\n",
		"inventory still names container": "case \" $* \" in *' state '*) exit 1;; *) echo '[{\"id\":\"container\"}]';; esac\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := fakeRunsc(t, script)
			if _, err := p.containerRunning(context.Background(), "container"); err == nil {
				t.Fatal("uncertainty reported as death")
			}
		})
	}
	p := fakeRunsc(t, "case \" $* \" in *' state '*) exit 1;; *) echo '[]';; esac\n")
	if alive, err := p.containerRunning(context.Background(), "absent"); err != nil || alive {
		t.Fatalf("verified absent container: %t %v", alive, err)
	}
}
func TestTeardownRetainsAllocationOnUnverifiedDelete(t *testing.T) {
	p := fakeRunsc(t, "case \" $* \" in *' list '*) echo '[{\"id\":\"container\"}]';; *) exit 1;; esac\n")
	p.inUse["10.79.1.2"] = true
	handle := &placementHandle{cid: "container", guestIP: "10.79.1.2"}
	if err := p.teardownPlacement(context.Background(), handle); err == nil || !strings.Contains(err.Error(), "remains") {
		t.Fatalf("unverified teardown succeeded: %v", err)
	}
	if !p.inUse[handle.guestIP] {
		t.Fatal("failed teardown released address for reuse")
	}
}

func TestVerifiedTeardownRequiresAnAddressableHandle(t *testing.T) {
	provider := fakeRunsc(t, "exit 1\n")
	for _, placement := range []*Placement{nil, {}} {
		if err := provider.Stop(context.Background(), placement); err == nil {
			t.Fatal("stop accepted an unaddressable placement")
		}
		if err := provider.Destroy(context.Background(), placement); err == nil {
			t.Fatal("destroy accepted an unaddressable placement")
		}
	}
	provider = fakeRunsc(t, "echo invalid-json\n")
	if _, err := provider.containerAbsent(context.Background(), "container"); err == nil {
		t.Fatal("malformed inventory proved absence")
	}
}

func TestRunscEmptyInventoryAcceptsNull(t *testing.T) {
	// runsc release-20260714.0 emits null when no containers remain.
	provider := fakeRunsc(t, "echo null\n")
	if absent, err := provider.containerAbsent(context.Background(), "absent"); err != nil || !absent {
		t.Fatalf("valid empty inventory: %t %v", absent, err)
	}
}
